package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePrometheusText(t *testing.T) {
	snapshot, err := Parse([]byte("# TYPE orders_total counter\norders_total{route=\"/orders\",result=\"ok\"} 3\n# TYPE request_seconds histogram\nrequest_seconds_bucket{le=\"0.5\"} 2 # {trace_id=\"abc\"} 0.2\nrequest_seconds_count 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Samples) != 3 || snapshot.Samples[0].Name != "orders_total" || snapshot.Samples[0].Type != "counter" || snapshot.Samples[0].Labels["route"] != "/orders" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if snapshot.Samples[1].Type != "histogram" || snapshot.Samples[2].Type != "histogram" {
		t.Fatalf("histogram types=%#v", snapshot.Samples)
	}
}

func TestStoreScrapesContinuouslyBeforeQueries(t *testing.T) {
	var value atomic.Int64
	value.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("jobs_total " + strconv.FormatInt(value.Load(), 10) + "\n"))
	}))
	defer server.Close()
	store, err := New(Config{Endpoint: server.URL, ScrapeInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Snapshot(context.Background(), "jobs_total"); err != nil {
		t.Fatal(err)
	}
	value.Store(2)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot, queryErr := store.Snapshot(context.Background(), "jobs_total")
		if queryErr == nil && len(snapshot.Samples) == 1 && snapshot.Samples[0].Value == 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background scrape did not update the PromQL store")
}

func TestStoreScrapesWithAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte("jobs_total 4\n"))
	}))
	defer server.Close()
	store, err := New(Config{Endpoint: server.URL, BearerToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	snapshot, err := store.Snapshot(context.Background(), "jobs_total")
	if err != nil || len(snapshot.Samples) != 1 || snapshot.Samples[0].Value != 4 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestStoreQueriesPrometheusAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(writer, "query API must use GET", http.StatusMethodNotAllowed)
			return
		}
		if request.URL.Path != "/api/v1/query" || request.FormValue("query") != `sum by (result) (orders_total)` {
			http.Error(writer, "bad query", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"result":"ok"},"value":[1720000000,"5"]}]}}`))
	}))
	defer server.Close()
	store, err := New(Config{Endpoint: server.URL, QueryAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background(), `sum by (result) (orders_total)`)
	if err != nil || len(snapshot.Samples) != 1 || snapshot.Samples[0].Value != 5 || snapshot.Samples[0].Labels["result"] != "ok" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}
