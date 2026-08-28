package metricpoller

import (
	"context"
	"testing"
	"time"

	"lamplight/internal/model"
)

type fakeStore struct {
	snapshots []model.MetricSnapshot
	calls     int
}

func (f *fakeStore) Snapshot(context.Context, string) (model.MetricSnapshot, error) {
	index := f.calls
	f.calls++
	if index >= len(f.snapshots) {
		index = len(f.snapshots) - 1
	}
	return f.snapshots[index], nil
}

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) After(value time.Duration) <-chan time.Time {
	f.now = f.now.Add(value)
	result := make(chan time.Time, 1)
	result <- f.now
	return result
}

func TestPollEvaluatesDeltaAfterSnapshotSettles(t *testing.T) {
	baseline := model.MetricSnapshot{Samples: []model.MetricSample{{Name: "orders_total", Value: 4, Labels: map[string]string{"result": "ok"}}}}
	after := model.MetricSnapshot{Samples: []model.MetricSample{{Name: "orders_total", Value: 5, Labels: map[string]string{"result": "ok"}}}}
	store := &fakeStore{snapshots: []model.MetricSnapshot{after, after}}
	clock := &fakeClock{now: time.Unix(0, 0)}
	check := Check{Name: "order metric", Query: "orders_total", Rule: model.QuantityRule{Kind: "exactly", Value: 1}, Assertions: []Assertion{{Name: "increments once", Evaluate: func(point model.MetricPoint) (bool, error) { return point.Delta == 1, nil }}}}
	result, err := Poll(context.Background(), store, map[string]model.MetricSnapshot{"orders_total": baseline}, Config{ObservationWindow: 5 * time.Second, SettleWindow: time.Second, Interval: time.Second, Clock: clock}, []Check{check})
	if err != nil || result.Checks[0].Status != model.StatusPassed || result.Checks[0].MetricEvidence.MatchCount != 1 || store.calls != 2 {
		t.Fatalf("result=%#v calls=%d err=%v", result, store.calls, err)
	}
}

func TestPollReportsWrongDeltaAtDeadline(t *testing.T) {
	baseline := model.MetricSnapshot{Samples: []model.MetricSample{{Name: "orders_total", Value: 4}}}
	after := model.MetricSnapshot{Samples: []model.MetricSample{{Name: "orders_total", Value: 6}}}
	store := &fakeStore{snapshots: []model.MetricSnapshot{after}}
	clock := &fakeClock{now: time.Unix(0, 0)}
	check := Check{Name: "order metric", Query: "orders_total", Rule: model.QuantityRule{Kind: "exactly", Value: 1}, Assertions: []Assertion{{Name: "increments once", Evaluate: func(point model.MetricPoint) (bool, error) { return point.Delta == 1, nil }}}}
	result, err := Poll(context.Background(), store, map[string]model.MetricSnapshot{"orders_total": baseline}, Config{ObservationWindow: 2 * time.Second, SettleWindow: time.Second, Interval: time.Second, Clock: clock}, []Check{check})
	if err != nil || result.Checks[0].Status != model.StatusFailed || result.Checks[0].Reason != "metric_assertion_failed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
