package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"tracetest/internal/datasource"
	"tracetest/internal/model"
)

func TestPollRejectsInvalidInputsAndHandlesEmptyChecks(t *testing.T) {
	if _, err := Poll(context.Background(), nil, "trace", Config{ObservationWindow: time.Second}, nil); err == nil {
		t.Fatal("nil datasource accepted")
	}
	fake := &datasource.Fake{}
	if result, err := Poll(context.Background(), fake, "trace", Config{}, nil); err != nil || result.Checks != nil {
		t.Fatalf("empty checks result=%#v err=%v", result, err)
	}
	for _, config := range []Config{{}, {ObservationWindow: 0}} {
		if _, err := Poll(context.Background(), fake, "trace", config, []SpanCheck{{Name: "x", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}}); err == nil {
			t.Fatal("invalid observation window accepted")
		}
	}
	for _, check := range []SpanCheck{{Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}, {Name: "x", Rule: model.QuantityRule{Kind: "at_least", Value: 1}}, {Name: "x", Rule: model.QuantityRule{Kind: "at_least", Value: -1}, Match: matchAll}, {Name: "x", Rule: model.QuantityRule{Kind: "never", Value: 0}, Match: matchAll}} {
		if _, err := Poll(context.Background(), fake, "trace", Config{ObservationWindow: time.Second}, []SpanCheck{check}); err == nil {
			t.Fatalf("invalid check accepted: %#v", check)
		}
	}
}

func TestApplyObservationRulesAndDeadlineStates(t *testing.T) {
	now := time.Unix(10, 0)
	observation := model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: []model.Span{{}, {}}}
	checks := []SpanCheck{
		{Name: "at-most", Rule: model.QuantityRule{Kind: "at_most", Value: 1}, Match: matchAll},
		{Name: "exactly", Rule: model.QuantityRule{Kind: "exactly", Value: 1}, Match: matchAll},
		{Name: "at-least", Rule: model.QuantityRule{Kind: "at_least", Value: 3}, Match: matchAll},
	}
	states := make([]checkState, len(checks))
	for i, check := range checks {
		states[i].check = check
	}
	if err := applyObservation(&states, observation, 0, time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if states[0].result.Reason != "maximum_exceeded" || states[1].result.Reason != "exact_count_exceeded" || states[2].result.Status != "" {
		t.Fatalf("observation states=%#v", states)
	}
	states = []checkState{{check: SpanCheck{Name: "most", Rule: model.QuantityRule{Kind: "at_most", Value: 2}, Match: matchAll}}, {check: SpanCheck{Name: "exact", Rule: model.QuantityRule{Kind: "exactly", Value: 2}, Match: matchAll}}}
	if err := applyObservation(&states, observation, 0, time.Time{}, now); err != nil || !terminal(states) {
		t.Fatalf("complete states=%#v err=%v", states, err)
	}
	settled := []checkState{{check: SpanCheck{Name: "settled", Rule: model.QuantityRule{Kind: "at_most", Value: 0}, Match: func(model.Span) (bool, error) { return false, nil }}}}
	if err := applyObservation(&settled, model.TraceObservation{Valid: true, Found: true}, time.Second, now.Add(-time.Second), now); err != nil || settled[0].result.Status != model.StatusPassed {
		t.Fatalf("settled=%#v err=%v", settled, err)
	}
	deadlineChecks := []SpanCheck{
		{Name: "least", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll},
		{Name: "most", Rule: model.QuantityRule{Kind: "at_most", Value: 1}, Match: matchAll},
		{Name: "exact", Rule: model.QuantityRule{Kind: "exactly", Value: 2}, Match: matchAll},
	}
	deadlineStates := make([]checkState, len(deadlineChecks))
	for i, check := range deadlineChecks {
		deadlineStates[i].check = check
	}
	if err := finishDeadline(&deadlineStates, true, model.TraceObservation{Spans: []model.Span{{}}, Complete: true}); err != nil {
		t.Fatal(err)
	}
	if deadlineStates[0].result.Status != model.StatusPassed || deadlineStates[1].result.Status != model.StatusPassed || deadlineStates[2].result.Status != model.StatusFailed {
		t.Fatalf("deadline states=%#v", deadlineStates)
	}
	partial := []checkState{{check: SpanCheck{Name: "partial", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}}}
	if err := finishDeadline(&partial, true, model.TraceObservation{Spans: []model.Span{{}}, Partial: true}); err != nil || partial[0].result.Reason != "partial_observation" {
		t.Fatalf("partial=%#v err=%v", partial, err)
	}
	notSeen := []checkState{{check: SpanCheck{Name: "missing", Rule: model.QuantityRule{Kind: "exactly", Value: 0}, Match: matchAll}}}
	if err := finishDeadline(&notSeen, false, model.TraceObservation{}); err != nil || notSeen[0].result.Reason != "trace_not_observed" {
		t.Fatalf("not seen=%#v err=%v", notSeen, err)
	}
}

type cancelClock struct {
	now    time.Time
	cancel context.CancelFunc
}

func (c *cancelClock) Now() time.Time { return c.now }
func (c *cancelClock) After(time.Duration) <-chan time.Time {
	c.cancel()
	return nil
}

func TestPollCancellationAndHelperBranches(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	stop()
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true}}}}
	result, err := Poll(ctx, store, "trace", Config{Clock: &fakeClock{now: time.Unix(0, 0)}, ObservationWindow: time.Second}, []SpanCheck{{Name: "one", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}})
	if err != nil || result.Checks[0].Status != model.StatusCancelled {
		t.Fatalf("pre-cancel result=%#v err=%v", result, err)
	}
	ctx, stop = context.WithCancel(context.Background())
	clock := &cancelClock{now: time.Unix(0, 0), cancel: stop}
	store = &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true}}}}
	result, err = Poll(ctx, store, "trace", Config{Clock: clock, ObservationWindow: time.Second}, []SpanCheck{{Name: "one", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}})
	if err != nil || result.Checks[0].Status != model.StatusCancelled {
		t.Fatalf("wait-cancel result=%#v err=%v", result, err)
	}
	if !IsRetriable(&model.ObservationError{Retriable: true}) || IsRetriable(errors.New("no")) || retryDelay(time.Second, 2*time.Second) != 2*time.Second || retryDelay(time.Second, time.Second) != time.Second {
		t.Fatal("helper classification incorrect")
	}
	states := []checkState{{check: SpanCheck{Name: "one"}}, {check: SpanCheck{Name: "two"}, result: model.CheckResult{Status: model.StatusPassed}}}
	cancel(&states)
	if states[0].result.Status != model.StatusCancelled || states[1].result.Status != model.StatusPassed {
		t.Fatalf("cancel states=%#v", states)
	}
	if !terminal([]checkState{{result: model.CheckResult{Status: model.StatusPassed}}}) || terminal([]checkState{{}}) {
		t.Fatal("terminal helper incorrect")
	}
	if got := results([]checkState{{check: SpanCheck{Name: "skip"}}}); got[0].Status != model.StatusSkipped {
		t.Fatalf("results=%#v", got)
	}
	clockReal := realClock{}
	if clockReal.Now().IsZero() || clockReal.After(time.Nanosecond) == nil {
		t.Fatal("real clock helper failed")
	}
	if err := wait(context.Background(), &fakeClock{now: time.Unix(0, 0)}, 0); err != nil {
		t.Fatal(err)
	}
}
