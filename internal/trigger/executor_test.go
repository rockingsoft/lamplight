package trigger

import (
	"context"
	"testing"

	"lamplight/internal/model"
)

type fakeHTTP struct {
	request model.HTTPRequest
	trace   *model.TestTraceContext
}

func (f *fakeHTTP) Execute(_ context.Context, request model.HTTPRequest, _ model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	f.request, f.trace = request, trace
	return model.Response{StatusCode: 200}, nil
}

func TestGraphQLMapsToHTTPAndPropagatesTraceContext(t *testing.T) {
	http := &fakeHTTP{}
	trace := &model.TestTraceContext{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef"}
	_, err := New(http).Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerGraphQL, Attributes: map[string]any{"url": "https://example.test/graphql", "query": "query { health }", "headers": map[string]any{"X-Test": "yes"}}}, model.DefaultHTTPClientConfig(), trace)
	if err != nil {
		t.Fatal(err)
	}
	if http.request.Method != "POST" || http.request.URL != "https://example.test/graphql" || http.request.Headers["X-Test"] != "yes" || http.trace != trace {
		t.Fatalf("request=%+v trace=%p", http.request, http.trace)
	}
}

func TestTraceIDBasedTriggers(t *testing.T) {
	for _, kind := range []model.TriggerKind{model.TriggerTraceID, model.TriggerCypress, model.TriggerPlaywright, model.TriggerArtillery, model.TriggerK6} {
		result, err := New(nil).Execute(context.Background(), model.TriggerRequest{Kind: kind, Attributes: map[string]any{"id": "0123456789abcdef0123456789abcdef"}}, model.HTTPClientConfig{}, nil)
		if err != nil || result.Body == "" {
			t.Fatalf("kind=%s result=%+v err=%v", kind, result, err)
		}
	}
}
