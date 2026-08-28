// Package metricpoller compares pre-trigger and post-trigger PromQL results
// until metric checks settle or reach their deadline.
package metricpoller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"lamplight/internal/model"
)

type Check struct {
	Name       string
	Query      string
	Rule       model.QuantityRule
	Assertions []Assertion
}

type Assertion struct {
	Name     string
	Source   model.SourceRange
	Evaluate func(model.MetricPoint) (bool, error)
}

type Config struct {
	ObservationWindow time.Duration
	SettleWindow      time.Duration
	Interval          time.Duration
	Clock             model.Clock
	Progress          func(Progress)
}

type Progress struct {
	Attempt     int
	MetricCount int
	RetryError  string
}

type Result struct {
	Checks   []model.CheckResult
	Snapshot model.MetricSnapshot
}

func Poll(ctx context.Context, store model.MetricStore, baselines map[string]model.MetricSnapshot, config Config, checks []Check) (Result, error) {
	if store == nil || len(checks) == 0 {
		return Result{}, errors.New("metric poller requires a store and checks")
	}
	if config.ObservationWindow <= 0 || config.SettleWindow <= 0 || config.Interval <= 0 {
		return Result{}, errors.New("metric polling windows and interval must be positive")
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	for _, check := range checks {
		if err := validate(check); err != nil {
			return Result{}, err
		}
	}
	started := config.Clock.Now()
	deadline := started.Add(config.ObservationWindow)
	stableSince := started
	lastFingerprint := fingerprintMap(baselines)
	last := map[string]model.MetricSnapshot{}
	seen := false
	attempt := 0
	for {
		if ctx.Err() != nil {
			return Result{Checks: cancelled(checks), Snapshot: combine(last)}, nil
		}
		attempt++
		snapshots, err := observeQueries(ctx, store, checks)
		if err != nil {
			var observation *model.ObservationError
			if !errors.As(err, &observation) || !observation.Retriable {
				return Result{}, err
			}
			report(config.Progress, Progress{Attempt: attempt, RetryError: err.Error()})
		} else {
			seen, last = true, snapshots
			report(config.Progress, Progress{Attempt: attempt, MetricCount: len(combine(snapshots).Samples)})
			currentFingerprint := fingerprintMap(snapshots)
			if currentFingerprint != lastFingerprint {
				lastFingerprint, stableSince = currentFingerprint, config.Clock.Now()
			}
			results, satisfied, evalErr := evaluateAll(checks, baselines, snapshots, "settle_window_elapsed")
			if evalErr != nil {
				return Result{}, evalErr
			}
			if satisfied && config.Clock.Now().Sub(stableSince) >= config.SettleWindow {
				return Result{Checks: results, Snapshot: combine(snapshots)}, nil
			}
		}
		if !config.Clock.Now().Before(deadline) {
			break
		}
		delay := config.Interval
		if remaining := deadline.Sub(config.Clock.Now()); delay > remaining {
			delay = remaining
		}
		select {
		case <-ctx.Done():
			return Result{Checks: cancelled(checks), Snapshot: combine(last)}, nil
		case <-config.Clock.After(delay):
		}
	}
	if !seen {
		return Result{}, errors.New("prometheus metrics were not observed before the observation window elapsed")
	}
	results, _, err := evaluateAll(checks, baselines, last, "observation_window_elapsed")
	return Result{Checks: results, Snapshot: combine(last)}, err
}

func observeQueries(ctx context.Context, store model.MetricStore, checks []Check) (map[string]model.MetricSnapshot, error) {
	result := map[string]model.MetricSnapshot{}
	for _, check := range checks {
		if _, exists := result[check.Query]; exists {
			continue
		}
		snapshot, err := store.Snapshot(ctx, check.Query)
		if err != nil {
			return nil, err
		}
		result[check.Query] = snapshot
	}
	return result, nil
}

func report(progress func(Progress), value Progress) {
	if progress != nil {
		progress(value)
	}
}

func validate(check Check) error {
	if check.Name == "" || strings.TrimSpace(check.Query) == "" {
		return errors.New("metric check requires a name and PromQL query")
	}
	if check.Rule.Value < 0 {
		return fmt.Errorf("metric check %q has negative quantity", check.Name)
	}
	if check.Rule.Kind != "at_least" && check.Rule.Kind != "at_most" && check.Rule.Kind != "exactly" {
		return fmt.Errorf("metric check %q has unsupported rule %q", check.Name, check.Rule.Kind)
	}
	for _, assertion := range check.Assertions {
		if assertion.Name == "" || assertion.Evaluate == nil {
			return fmt.Errorf("metric check %q has an invalid assertion", check.Name)
		}
	}
	return nil
}

func evaluateAll(checks []Check, before, after map[string]model.MetricSnapshot, successReason string) ([]model.CheckResult, bool, error) {
	results := make([]model.CheckResult, 0, len(checks))
	all := true
	for _, check := range checks {
		points := deltaPoints(before[check.Query], after[check.Query])
		result, passed, err := evaluate(check, points, successReason)
		if err != nil {
			return nil, false, fmt.Errorf("evaluate metric check %q: %w", check.Name, err)
		}
		results = append(results, result)
		all = all && passed
	}
	return results, all, nil
}

func evaluate(check Check, points []model.MetricPoint, successReason string) (model.CheckResult, bool, error) {
	count := 0
	matchedPoints := []model.MetricPoint{}
	assertionsPassed := true
	evidence := map[string]model.AssertionEvidence{}
	for _, point := range points {
		count++
		matchedPoints = append(matchedPoints, point)
		for _, assertion := range check.Assertions {
			passed, err := assertion.Evaluate(point)
			if err != nil {
				return model.CheckResult{}, false, fmt.Errorf("assertion %q: %w", assertion.Name, err)
			}
			item, exists := evidence[assertion.Name]
			if !exists {
				item = model.AssertionEvidence{Name: assertion.Name, Passed: true, Value: true, Source: assertion.Source}
			}
			if !passed {
				item.Passed, item.Value, assertionsPassed = false, false, false
			}
			evidence[assertion.Name] = item
		}
	}
	ordered := make([]model.AssertionEvidence, 0, len(check.Assertions))
	for _, assertion := range check.Assertions {
		if item, ok := evidence[assertion.Name]; ok {
			ordered = append(ordered, item)
		}
	}
	quantityPassed := check.Rule.Kind == "at_least" && count >= check.Rule.Value || check.Rule.Kind == "at_most" && count <= check.Rule.Value || check.Rule.Kind == "exactly" && count == check.Rule.Value
	passed := assertionsPassed && quantityPassed
	reason := successReason
	status := model.StatusPassed
	if !passed {
		status = model.StatusFailed
		if !assertionsPassed {
			reason = "metric_assertion_failed"
		} else {
			reason = "count_not_satisfied"
		}
	}
	return model.CheckResult{Name: check.Name, Status: status, Reason: reason, MetricEvidence: &model.MetricEvidence{Rule: check.Rule, MatchCount: count, Reason: reason, Assertions: ordered, Metrics: matchedPoints}}, passed, nil
}

func deltaPoints(before, after model.MetricSnapshot) []model.MetricPoint {
	baseline := map[string]model.MetricSample{}
	for _, sample := range before.Samples {
		baseline[sampleKey(sample)] = sample
	}
	points := make([]model.MetricPoint, 0, len(after.Samples))
	for _, sample := range after.Samples {
		previous := baseline[sampleKey(sample)].Value
		points = append(points, model.MetricPoint{Name: sample.Name, Type: sample.Type, Value: sample.Value, PreviousValue: previous, Delta: sample.Value - previous, Labels: sample.Labels, Attributes: sample.Attributes, Resource: sample.Resource})
	}
	return points
}

func sampleKey(sample model.MetricSample) string {
	names := make([]string, 0, len(sample.Labels))
	for name := range sample.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var key strings.Builder
	key.WriteString(sample.Name)
	resource, _ := json.Marshal(sample.Resource)
	key.Write(resource)
	attributes, _ := json.Marshal(sample.Attributes)
	key.Write(attributes)
	for _, name := range names {
		key.WriteByte(0)
		key.WriteString(name)
		key.WriteByte('=')
		key.WriteString(sample.Labels[name])
	}
	return key.String()
}

func fingerprintMap(snapshots map[string]model.MetricSnapshot) string {
	encoded, _ := json.Marshal(snapshots)
	value := sha256.Sum256(encoded)
	return string(value[:])
}

func combine(snapshots map[string]model.MetricSnapshot) model.MetricSnapshot {
	result := model.MetricSnapshot{}
	queries := make([]string, 0, len(snapshots))
	for query := range snapshots {
		queries = append(queries, query)
	}
	sort.Strings(queries)
	for _, query := range queries {
		result.Samples = append(result.Samples, snapshots[query].Samples...)
	}
	return result
}

func cancelled(checks []Check) []model.CheckResult {
	results := make([]model.CheckResult, len(checks))
	for index, check := range checks {
		results[index] = model.CheckResult{Name: check.Name, Status: model.StatusCancelled, Reason: "cancelled"}
	}
	return results
}

type realClock struct{}

func (realClock) Now() time.Time                             { return time.Now() }
func (realClock) After(value time.Duration) <-chan time.Time { return time.After(value) }
