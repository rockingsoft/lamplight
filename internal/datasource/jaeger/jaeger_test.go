package jaeger

import (
	"context"
	"lamplight/internal/model"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStore(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":["svc"]}`)) })
	mux.HandleFunc("/api/traces/0123456789abcdef0123456789abcdef", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"traceID":"0123456789abcdef0123456789abcdef","spans":[{"traceID":"0123456789abcdef0123456789abcdef","spanID":"abc","operationName":"checkout","duration":12,"tags":[{"key":"http.status_code","type":"int64","value":200}],"processID":"p1"}],"processes":{"p1":{"serviceName":"shop","tags":[]}}}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	store, err := New(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := store.Observe(context.Background(), model.TraceID("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || len(got.Spans) != 1 || got.Spans[0].Name != "checkout" || got.Spans[0].Resource["service.name"] != "shop" {
		t.Fatalf("%#v", got)
	}
}
