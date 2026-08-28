package promqlstore

import (
	"context"
	"testing"
	"time"

	"lamplight/internal/model"
)

func TestStoreEvaluatesPromQLAggregation(t *testing.T) {
	now := time.Unix(100, 0)
	store := New()
	store.now = func() time.Time { return now }
	if err := store.Ingest([]model.MetricSample{
		{Name: "orders_total", Value: 2, Labels: map[string]string{"result": "ok", "instance": "one"}},
		{Name: "orders_total", Value: 3, Labels: map[string]string{"result": "ok", "instance": "two"}},
		{Name: "orders_total", Value: 1, Labels: map[string]string{"result": "error", "instance": "one"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background(), `sum by (result) (orders_total)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Samples) != 2 || snapshot.Samples[0].Labels["result"] != "error" || snapshot.Samples[0].Value != 1 || snapshot.Samples[1].Labels["result"] != "ok" || snapshot.Samples[1].Value != 5 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestStoreRetainsSamplesForRangeQueries(t *testing.T) {
	start := time.Unix(100, 0)
	store := New()
	store.now = func() time.Time { return start.Add(time.Second) }
	if err := store.Ingest([]model.MetricSample{{Name: "requests_total", Value: 4}}, start); err != nil {
		t.Fatal(err)
	}
	if err := store.Ingest([]model.MetricSample{{Name: "requests_total", Value: 5}}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background(), `changes(requests_total[1500ms])`)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Samples) != 1 || snapshot.Samples[0].Value != 1 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}
