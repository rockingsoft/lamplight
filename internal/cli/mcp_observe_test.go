package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lamplight/internal/config"
	"lamplight/internal/mcpserver"
	"lamplight/internal/result"
)

func TestObserveTraceForMCPUsesProjectDatasourceAndRedactsEvidence(t *testing.T) {
	const traceID = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/traces/"+traceID || request.Header.Get("Authorization") != "Bearer datasource-secret" {
			t.Fatalf("request=%s authorization=%q", request.URL, request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[{"traceId":"%s","spanId":"0123456789abcdef","name":"checkout datasource-secret","kind":2,"startTimeUnixNano":"100","endTimeUnixNano":"150","attributes":[{"key":"customer.note","value":{"stringValue":"datasource-secret"}},{"key":"api.token","value":{"stringValue":"visible-only-before-redaction"}}],"status":{"code":1}}]}]}]}`, traceID)
	}))
	defer server.Close()

	root := t.TempDir()
	base := filepath.Join(root, "tests")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	project := fmt.Sprintf("project { base_dir = \"tests\" }\nvariable \"TEMPO_ENDPOINT\" {\n  type = string\n  default = %q\n}\nvariable \"TEMPO_TOKEN\" {\n  type = string\n  sensitive = true\n}\ndatasource \"tempo\" {\n  endpoint = var.TEMPO_ENDPOINT\n  auth { bearer_token = var.TEMPO_TOKEN }\n}\n", server.URL)
	if err := os.WriteFile(filepath.Join(root, ".lamplight"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := observeTraceForMCP(context.Background(), config.Options{WorkingDir: root}, mcpserver.ObserveTraceRequest{TraceID: traceID, Variables: map[string]string{"TEMPO_TOKEN": "datasource-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Found || evidence.SpanCount != 1 || len(evidence.Spans) != 1 {
		t.Fatalf("evidence=%#v", evidence)
	}
	span := evidence.Spans[0]
	if span.Name != "checkout "+result.Redacted || span.Attributes["customer.note"] != result.Redacted || span.Attributes["api.token"] != result.Redacted {
		t.Fatalf("span was not redacted: %#v", span)
	}
}

func TestObserveTraceForMCPRejectsInvalidIDWithoutLoadingProject(t *testing.T) {
	_, err := observeTraceForMCP(context.Background(), config.Options{}, mcpserver.ObserveTraceRequest{TraceID: "bad"})
	if err == nil {
		t.Fatal("invalid trace ID was accepted")
	}
}
