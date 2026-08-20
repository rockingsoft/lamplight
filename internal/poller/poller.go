// Package poller implements the single deterministic trace-observation
// lifecycle shared by all span checks in a step.
package poller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"lamplight/internal/debuglog"
	"lamplight/internal/model"
)

const defaultInterval = time.Second

// SpanCheck is the normalized, compiled part of one spans block.
type SpanCheck struct {
	Name       string
	Rule       model.QuantityRule
	Match      func(model.Span) (bool, error)
	Assertions []SpanAssertion
}

// SpanAssertion is evaluated for every span selected by Match.
type SpanAssertion struct {
	Name     string
	Source   model.SourceRange
	Evaluate func(model.Span) (bool, error)
}

// Config controls one step lifecycle. Clock is injectable so tests need no
// wall-clock sleeps. ObservationWindow is a hard deadline.
type Config struct {
	ObservationWindow time.Duration
	SettleWindow      time.Duration
	Interval          time.Duration
	Clock             model.Clock
	Progress          func(Progress)
}

// Progress is emitted after every datasource observation attempt so
// interactive callers can explain what polling is currently seeing.
type Progress struct {
	Attempt    int
	SpanCount  int
	Found      bool
	Complete   bool
	Partial    bool
	RetryError string
	Checks     []CheckProgress
}

type CheckProgress struct {
	Name       string
	MatchCount int
	Status     model.Status
}

// Result records the terminal state for all checks and the last observation.
type Result struct {
	Checks      []model.CheckResult
	Observation model.TraceObservation
}

// Poll starts observing immediately, then uses one tick per attempt for every
// check. It returns a non-nil error only for a non-retriable datasource or a
// malformed check; cancellation is represented by cancelled check states.
func Poll(ctx context.Context, store model.DataStore, traceID model.TraceID, config Config, checks []SpanCheck) (Result, error) {
	debuglog.Debug(ctx, "trace polling started", "trace_id", traceID, "checks", len(checks), "observation_window", config.ObservationWindow, "settle_window", config.SettleWindow)
	if store == nil {
		return Result{}, errors.New("poller requires a datasource")
	}
	if len(checks) == 0 {
		return Result{}, nil
	}
	if config.ObservationWindow <= 0 {
		return Result{}, errors.New("observation window must be positive")
	}
	if config.Interval <= 0 {
		config.Interval = defaultInterval
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	states := make([]checkState, len(checks))
	for index, check := range checks {
		if err := validate(check); err != nil {
			return Result{}, err
		}
		states[index].check = check
	}
	started := config.Clock.Now()
	deadline := started.Add(config.ObservationWindow)
	var last model.TraceObservation
	seenValid := false
	stableSince := time.Time{}
	lastFingerprint := ""
	attempt := 0

	for {
		if ctx.Err() != nil {
			cancel(&states)
			return Result{Checks: results(states), Observation: last}, nil
		}
		now := config.Clock.Now()
		if !now.Before(deadline) {
			if err := finishDeadline(&states, seenValid, last); err != nil {
				return Result{Checks: results(states), Observation: last}, err
			}
			return Result{Checks: results(states), Observation: last}, nil
		}

		attempt++
		observation, err := store.Observe(ctx, traceID)
		if err != nil {
			var observationError *model.ObservationError
			if !errors.As(err, &observationError) || !observationError.Retriable {
				return Result{Checks: results(states), Observation: last}, fmt.Errorf("observe trace %s: %w", traceID, err)
			}
			debuglog.Debug(ctx, "trace observation retry", "trace_id", traceID, "error", err, "retry_after", observationError.RetryAfter)
			reportProgress(config.Progress, Progress{Attempt: attempt, RetryError: err.Error(), Checks: checkProgress(states)})
			if err := wait(ctx, config.Clock, retryDelay(config.Interval, observationError.RetryAfter)); err != nil {
				cancel(&states)
				return Result{Checks: results(states), Observation: last}, nil
			}
			continue
		}
		last = observation
		debuglog.Debug(ctx, "trace observed", "trace_id", traceID, "found", observation.Found, "complete", observation.Complete, "partial", observation.Partial, "spans", len(observation.Spans))
		if observation.Valid && (observation.Found || observation.Complete) {
			seenValid = true
			fingerprint := observation.Fingerprint
			if fingerprint == "" {
				fingerprint = fingerprintOf(observation)
			}
			if fingerprint != lastFingerprint {
				lastFingerprint = fingerprint
				stableSince = now
			}
			if err := applyObservation(&states, observation, config.SettleWindow, stableSince, now); err != nil {
				return Result{Checks: results(states), Observation: last}, err
			}
			reportProgress(config.Progress, Progress{Attempt: attempt, SpanCount: len(observation.Spans), Found: observation.Found, Complete: observation.Complete, Partial: observation.Partial, Checks: checkProgress(states)})
			if terminal(states) {
				debuglog.Debug(ctx, "trace polling completed", "trace_id", traceID)
				return Result{Checks: results(states), Observation: last}, nil
			}
		} else {
			reportProgress(config.Progress, Progress{Attempt: attempt, SpanCount: len(observation.Spans), Found: observation.Found, Complete: observation.Complete, Partial: observation.Partial, Checks: checkProgress(states)})
		}
		if err := wait(ctx, config.Clock, config.Interval); err != nil {
			cancel(&states)
			return Result{Checks: results(states), Observation: last}, nil
		}
	}
}

func reportProgress(report func(Progress), progress Progress) {
	if report != nil {
		report(progress)
	}
}

func checkProgress(states []checkState) []CheckProgress {
	progress := make([]CheckProgress, len(states))
	for index, state := range states {
		progress[index] = CheckProgress{Name: state.check.Name, Status: state.result.Status}
		if state.result.SpanEvidence != nil {
			progress[index].MatchCount = state.result.SpanEvidence.MatchCount
		}
	}
	return progress
}

type checkState struct {
	check  SpanCheck
	result model.CheckResult
}

func validate(check SpanCheck) error {
	if check.Name == "" || check.Match == nil {
		return errors.New("span check requires name and predicate")
	}
	if check.Rule.Value < 0 {
		return fmt.Errorf("span check %q has negative quantity", check.Name)
	}
	for _, assertion := range check.Assertions {
		if assertion.Name == "" || assertion.Evaluate == nil {
			return fmt.Errorf("span check %q has an invalid assertion", check.Name)
		}
	}
	switch check.Rule.Kind {
	case "at_least", "at_most", "exactly":
		return nil
	default:
		return fmt.Errorf("span check %q has unsupported rule %q", check.Name, check.Rule.Kind)
	}
}

func applyObservation(states *[]checkState, observation model.TraceObservation, settle time.Duration, stableSince, now time.Time) error {
	for index := range *states {
		state := &(*states)[index]
		if state.result.Status != "" {
			continue
		}
		evaluated, err := evaluate(state.check, observation.Spans)
		if err != nil {
			return fmt.Errorf("evaluate span check %q: %w", state.check.Name, err)
		}
		count := evaluated.count
		state.result = model.CheckResult{Name: state.check.Name, Status: "", SpanEvidence: &model.SpanEvidence{Rule: state.check.Rule, MatchCount: count, Assertions: evaluated.evidence}}
		if !evaluated.assertionsPassed {
			state.result.Status = model.StatusFailed
			state.result.Reason = "span_assertion_failed"
			state.result.SpanEvidence.Reason = "span_assertion_failed"
			continue
		}
		switch state.check.Rule.Kind {
		case "at_least":
			if count >= state.check.Rule.Value {
				state.result.Status = model.StatusPassed
				state.result.SpanEvidence.Reason = "minimum_reached"
			}
		case "at_most":
			if count > state.check.Rule.Value {
				state.result = failedResult(state.check, count, "maximum_exceeded", evaluated.evidence)
			} else if observation.Complete {
				state.result.Status = model.StatusPassed
				state.result.SpanEvidence.Reason = "trace_complete"
			} else if settle > 0 && !stableSince.IsZero() && now.Sub(stableSince) >= settle {
				state.result.Status = model.StatusPassed
				state.result.SpanEvidence.Reason = "settle_window_elapsed"
			}
		case "exactly":
			if count > state.check.Rule.Value {
				state.result = failedResult(state.check, count, "exact_count_exceeded", evaluated.evidence)
			} else if observation.Complete && count == state.check.Rule.Value {
				state.result.Status = model.StatusPassed
				state.result.SpanEvidence.Reason = "trace_complete"
			} else if state.check.Rule.Value == 0 && settle > 0 && !stableSince.IsZero() && now.Sub(stableSince) >= settle {
				state.result.Status = model.StatusPassed
				state.result.SpanEvidence.Reason = "settle_window_elapsed"
			}
		}
	}
	return nil
}

type evaluation struct {
	count            int
	assertionsPassed bool
	evidence         []model.AssertionEvidence
}

func evaluate(check SpanCheck, spans []model.Span) (evaluation, error) {
	result := evaluation{assertionsPassed: true}
	evidenceByName := map[string]model.AssertionEvidence{}
	for _, span := range spans {
		match, err := check.Match(span)
		if err != nil {
			return result, err
		}
		if !match {
			continue
		}
		result.count++
		for _, assertion := range check.Assertions {
			passed, err := assertion.Evaluate(span)
			if err != nil {
				return result, fmt.Errorf("assertion %q: %w", assertion.Name, err)
			}
			prior, exists := evidenceByName[assertion.Name]
			if !exists {
				prior = model.AssertionEvidence{Name: assertion.Name, Passed: true, Value: true, Source: assertion.Source}
			}
			if !passed {
				prior.Passed, prior.Value = false, false
				result.assertionsPassed = false
			}
			evidenceByName[assertion.Name] = prior
		}
	}
	for _, assertion := range check.Assertions {
		if item, exists := evidenceByName[assertion.Name]; exists {
			result.evidence = append(result.evidence, item)
		}
	}
	return result, nil
}

func finishDeadline(states *[]checkState, seenValid bool, last model.TraceObservation) error {
	for index := range *states {
		state := &(*states)[index]
		if state.result.Status != "" {
			continue
		}
		if !seenValid {
			state.result = failedResult(state.check, 0, "trace_not_observed", nil)
			continue
		}
		evaluated, err := evaluate(state.check, last.Spans)
		if err != nil {
			return fmt.Errorf("evaluate span check %q: %w", state.check.Name, err)
		}
		count := evaluated.count
		state.result = model.CheckResult{Name: state.check.Name, SpanEvidence: &model.SpanEvidence{Rule: state.check.Rule, MatchCount: count, Assertions: evaluated.evidence}}
		if last.Partial && !last.Complete {
			state.result.Status = model.StatusFailed
			state.result.Reason = "partial_observation"
			state.result.SpanEvidence.Reason = "partial_observation"
			continue
		}
		if !evaluated.assertionsPassed {
			state.result.Status = model.StatusFailed
			state.result.Reason = "span_assertion_failed"
			state.result.SpanEvidence.Reason = "span_assertion_failed"
			continue
		}
		passed := (state.check.Rule.Kind == "at_least" && count >= state.check.Rule.Value) ||
			(state.check.Rule.Kind == "at_most" && count <= state.check.Rule.Value) ||
			(state.check.Rule.Kind == "exactly" && count == state.check.Rule.Value)
		if passed {
			state.result.Status = model.StatusPassed
			state.result.SpanEvidence.Reason = "observation_window_elapsed"
		} else {
			state.result = failedResult(state.check, count, "count_not_satisfied", evaluated.evidence)
		}
	}
	return nil
}

func failedResult(check SpanCheck, count int, reason string, evidence []model.AssertionEvidence) model.CheckResult {
	return model.CheckResult{Name: check.Name, Status: model.StatusFailed, Reason: reason, SpanEvidence: &model.SpanEvidence{Rule: check.Rule, MatchCount: count, Reason: reason, Assertions: evidence}}
}

func terminal(states []checkState) bool {
	for _, state := range states {
		if state.result.Status == "" {
			return false
		}
	}
	return true
}
func results(states []checkState) []model.CheckResult {
	result := make([]model.CheckResult, len(states))
	for index, state := range states {
		if state.result.Status == "" {
			state.result.Status = model.StatusSkipped
		}
		result[index] = state.result
	}
	return result
}

// IsRetriable reports whether an error allows the polling state machine to
// continue until its observation deadline.
func IsRetriable(err error) bool {
	var observationError *model.ObservationError
	return errors.As(err, &observationError) && observationError.Retriable
}
func cancel(states *[]checkState) {
	for index := range *states {
		if (*states)[index].result.Status == "" {
			(*states)[index].result = model.CheckResult{Name: (*states)[index].check.Name, Status: model.StatusCancelled, Reason: "cancelled"}
		}
	}
}
func retryDelay(interval, retryAfter time.Duration) time.Duration {
	if retryAfter > interval {
		return retryAfter
	}
	return interval
}

func fingerprintOf(observation model.TraceObservation) string {
	encoded, _ := json.Marshal(observation.Spans)
	sum := sha256.Sum256(encoded)
	return string(sum[:])
}

type realClock struct{}

func (realClock) Now() time.Time                                { return time.Now() }
func (realClock) After(duration time.Duration) <-chan time.Time { return time.After(duration) }
func wait(ctx context.Context, clock model.Clock, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-clock.After(duration):
		return nil
	}
}
