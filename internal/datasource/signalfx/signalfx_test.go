package signalfx

import (
	"context"
	"lamplight/internal/model"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObserve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/segments") {
			_, _ = w.Write([]byte(`[123]`))
			return
		}
		_, _ = w.Write([]byte(`[{"traceId":"0123456789abcdef0123456789abcdef","spanId":"abcd","operationName":"work","durationMicros":2,"tags":{"http.method":"GET"},"processTags":{"service.name":"api"}}]`))
	}))
	defer srv.Close()
	s, _ := New(Config{Endpoint: srv.URL})
	got, e := s.Observe(context.Background(), model.TraceID("0123456789abcdef0123456789abcdef"))
	if e != nil || len(got.Spans) != 1 || got.Spans[0].Name != "work" {
		t.Fatalf("%#v %v", got, e)
	}
}
