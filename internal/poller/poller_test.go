package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"lamplight/internal/datasource"
	"lamplight/internal/model"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.now = f.now.Add(d)
	channel := make(chan time.Time, 1)
	channel <- f.now
	return channel
}

func matchAll(model.Span) (bool, error) { return true, nil }

func TestPollResolvesMinimumImmediately(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true, Valid: true, Spans: []model.Span{{}}}}}}
	result, err := Poll(context.Background(), store, "trace", Config{Clock: clock, ObservationWindow: time.Minute}, []SpanCheck{{Name: "one", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}})
	if err != nil || result.Checks[0].Status != model.StatusPassed || store.Calls != 1 {
		t.Fatalf("result=%#v calls=%d err=%v", result, store.Calls, err)
	}
}

func TestPollNeverTreatsUnobservedTraceAsZero(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: false}}}}
	result, err := Poll(context.Background(), store, "trace", Config{Clock: clock, ObservationWindow: 2 * time.Second, Interval: time.Second}, []SpanCheck{{Name: "none", Rule: model.QuantityRule{Kind: "exactly", Value: 0}, Match: matchAll}})
	if err != nil || result.Checks[0].Status != model.StatusFailed || result.Checks[0].Reason != "trace_not_observed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPollUsesCompleteTraceForNegativeCheck(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true, Valid: true, Complete: true}}}}
	result, err := Poll(context.Background(), store, "trace", Config{Clock: clock, ObservationWindow: time.Minute}, []SpanCheck{{Name: "none", Rule: model.QuantityRule{Kind: "exactly", Value: 0}, Match: matchAll}})
	if err != nil || result.Checks[0].Status != model.StatusPassed || result.Checks[0].SpanEvidence.Reason != "trace_complete" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPollRetriesDatasourceErrors(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{
		{Err: &model.ObservationError{Err: context.DeadlineExceeded, Retriable: true}},
		{Observation: model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: []model.Span{{}}}},
	}}
	result, err := Poll(context.Background(), store, "trace", Config{Clock: clock, ObservationWindow: time.Minute}, []SpanCheck{{Name: "one", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}})
	if err != nil || result.Checks[0].Status != model.StatusPassed || store.Calls != 2 {
		t.Fatalf("result=%#v calls=%d err=%v", result, store.Calls, err)
	}
}

func TestPollReturnsNonRetriableDatasourceError(t *testing.T) {
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{{Err: &model.ObservationError{Err: context.DeadlineExceeded, Retriable: false}}}}
	_, err := Poll(context.Background(), store, "trace", Config{Clock: &fakeClock{now: time.Unix(0, 0)}, ObservationWindow: time.Minute}, []SpanCheck{{Name: "one", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll}})
	if err == nil {
		t.Fatal("non-retriable error was ignored")
	}
}

func TestPollReturnsPredicateErrorAsTechnicalError(t *testing.T) {
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true, Valid: true, Spans: []model.Span{{}}}}}}
	_, err := Poll(context.Background(), store, "trace", Config{Clock: &fakeClock{now: time.Unix(0, 0)}, ObservationWindow: time.Minute}, []SpanCheck{{Name: "bad", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: func(model.Span) (bool, error) { return false, errors.New("type mismatch") }}})
	if err == nil {
		t.Fatal("predicate error became a check failure")
	}
}

func TestPollAppliesAssertionsToEveryMatchingSpan(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: []model.Span{{Name: "valid"}, {Name: "invalid"}}}}}}
	check := SpanCheck{
		Name:  "all selected spans",
		Rule:  model.QuantityRule{Kind: "at_least", Value: 1},
		Match: matchAll,
		Assertions: []SpanAssertion{{Name: "valid name", Evaluate: func(span model.Span) (bool, error) {
			return span.Name == "valid", nil
		}}},
	}
	result, err := Poll(context.Background(), store, "trace", Config{Clock: clock, ObservationWindow: time.Minute}, []SpanCheck{check})
	if err != nil {
		t.Fatal(err)
	}
	got := result.Checks[0]
	if got.Status != model.StatusFailed || got.Reason != "span_assertion_failed" || got.SpanEvidence.MatchCount != 2 {
		t.Fatalf("check=%#v", got)
	}
	if len(got.SpanEvidence.Assertions) != 1 || got.SpanEvidence.Assertions[0].Passed {
		t.Fatalf("assertion evidence=%#v", got.SpanEvidence.Assertions)
	}
}

func TestPollAtLeastPassesImmediatelyWhenObservedSpansSatisfyAssertions(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	store := &datasource.Fake{Script: []datasource.ScriptedObservation{
		{Observation: model.TraceObservation{Found: true, Valid: true, Spans: []model.Span{{Name: "valid"}}}},
		{Observation: model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: []model.Span{{Name: "valid"}, {Name: "invalid"}}}},
	}}
	check := SpanCheck{Name: "all", Rule: model.QuantityRule{Kind: "at_least", Value: 1}, Match: matchAll, Assertions: []SpanAssertion{{Name: "valid", Evaluate: func(span model.Span) (bool, error) { return span.Name == "valid", nil }}}}
	result, err := Poll(context.Background(), store, "trace", Config{Clock: clock, ObservationWindow: time.Minute, Interval: time.Second}, []SpanCheck{check})
	if err != nil || store.Calls != 1 || result.Checks[0].Status != model.StatusPassed || result.Checks[0].SpanEvidence.Reason != "minimum_reached" {
		t.Fatalf("result=%#v calls=%d err=%v", result, store.Calls, err)
	}
}
