package search

import (
	"context"
	"lamplight/internal/model"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearch(t *testing.T) {
	for _, kind := range []string{"elasticapm", "opensearch"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.Write([]byte(`{"version":{}}`))
					return
				}
				w.Write([]byte(`{"hits":{"hits":[{"_source":{"trace":{"id":"0123456789abcdef0123456789abcdef"},"span":{"id":"abcd","name":"work","duration":{"us":2}},"service":{"name":"api"}}}]}}`))
			}))
			defer srv.Close()
			s, e := New(Config{Kind: kind, Endpoint: srv.URL + "/traces"})
			if e != nil {
				t.Fatal(e)
			}
			if e = s.TestConnection(context.Background()); e != nil {
				t.Fatal(e)
			}
			got, e := s.Observe(context.Background(), model.TraceID("0123456789abcdef0123456789abcdef"))
			if e != nil || len(got.Spans) != 1 || got.Spans[0].Name != "work" {
				t.Fatalf("%#v %v", got, e)
			}
		})
	}
}
