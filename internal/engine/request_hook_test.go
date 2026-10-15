package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/model"
)

func hookProject(t *testing.T, script string) (*model.Project, *model.RequestHookDefinition) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "hook.sh"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	project := &model.Project{Definition: &model.ProjectDefinition{BaseDir: directory}, Variables: map[string]model.SensitiveValue{}, HTTPClient: model.DefaultHTTPClientConfig()}
	hook := &model.RequestHookDefinition{Command: parseExpr(t, `["sh", "hook.sh"]`)}
	return project, hook
}

func TestRequestHookReceivesCompleteHTTPJSONAndChangesRequest(t *testing.T) {
	project, hook := hookProject(t, `cat >/dev/null
printf '%s' '{"method":"POST","url":"http://example.test/modified","headers":{"X-Hook":"yes"},"body":"changed"}'`)
	request := model.HTTPRequest{Method: "GET", URL: "http://example.test/original", Headers: map[string]string{"X-Original": "yes"}, Body: "original"}
	changed, err := applyHTTPHook(context.Background(), hook, project, cty.EmptyObjectVal, cty.EmptyObjectVal, request, nil)
	if err != nil || changed.Method != "POST" || changed.URL != "http://example.test/modified" || changed.Headers["X-Hook"] != "yes" || changed.Body != "changed" {
		t.Fatalf("request=%#v err=%v", changed, err)
	}
}

func TestRequestHookRejectsTraceHeaderChange(t *testing.T) {
	project, hook := hookProject(t, `cat >/dev/null
printf '%s' '{"method":"GET","url":"http://example.test","headers":{"traceparent":"wrong"},"body":""}'`)
	trace := &model.TestTraceContext{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef"}
	request := model.HTTPRequest{Method: "GET", URL: "http://example.test"}
	_, err := applyHTTPHook(context.Background(), hook, project, cty.EmptyObjectVal, cty.EmptyObjectVal, request, trace)
	if err == nil || !strings.Contains(err.Error(), "cannot change trace headers") {
		t.Fatalf("err=%v", err)
	}
}

func TestRequestHookRejectsMalformedOutput(t *testing.T) {
	project, hook := hookProject(t, `cat >/dev/null
printf '%s' 'not json'`)
	_, err := applyHTTPHook(context.Background(), hook, project, cty.EmptyObjectVal, cty.EmptyObjectVal, model.HTTPRequest{Method: "GET", URL: "http://example.test"}, nil)
	if err == nil || !strings.Contains(err.Error(), "decode request_hook output") {
		t.Fatalf("err=%v", err)
	}
}

func TestRequestHookValidatesGenericRequest(t *testing.T) {
	if err := validateTriggerHookOutput(model.TriggerKafka, map[string]any{"broker_urls": []any{"broker:9092"}, "topic": "orders", "message_value": "hello", "headers": map[string]any{"X-Hook": "yes"}}); err != nil {
		t.Fatal(err)
	}
	if err := validateTriggerHookOutput(model.TriggerKafka, map[string]any{"broker_urls": []any{"broker:9092"}, "topic": "orders", "message_value": 42}); err == nil {
		t.Fatal("expected invalid Kafka request to fail")
	}
}
