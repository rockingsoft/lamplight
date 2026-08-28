package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/engine"
	"lamplight/internal/model"
	"lamplight/internal/result"
)

func cliExpr(t *testing.T, source string) hcl.Expression {
	t.Helper()
	expression, diagnostics := hclsyntax.ParseExpression([]byte(source), "cli-test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diagnostics.HasErrors() {
		t.Fatal(diagnostics.Error())
	}
	return expression
}

func TestRemoteExecutorCloseDrainsTrailingRuntimeOutput(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	done := make(chan error, 1)

	go func() {
		_, _ = io.Copy(io.Discard, requestReader)
		_ = requestReader.Close()
		_, err := responseWriter.Write([]byte("runtime teardown output"))
		_ = responseWriter.CloseWithError(err)
		done <- err
	}()

	closed := make(chan error, 1)
	go func() {
		closed <- closeRemoteExecutor(context.Background(), requestWriter, responseReader, done)
	}()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote executor close did not drain trailing output")
	}
}

func TestRemoteExecutorCloseReturnsAfterCancellation(t *testing.T) {
	_, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	done := make(chan error)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	err := closeRemoteExecutor(ctx, requestWriter, responseReader, done)
	_ = responseWriter.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("close error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("canceled close took %s", elapsed)
	}
}

func TestMainUsageAndFlagErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"version", "extra"}, {"list"}, {"list", "nope"}, {"validate", "--unknown"}, {"run", "--unknown"}, {"init", "--unknown"}, {"run", "one", "two"}, {"migrate"}, {"migrate", "other"}, {"migrate", "tracetest"}} {
		var out, stderr bytes.Buffer
		if code := Main(context.Background(), args, IO{Out: &out, Err: &stderr}); code != 1 {
			t.Fatalf("args=%v code=%d out=%q err=%q", args, code, out.String(), stderr.String())
		}
		if stderr.Len() == 0 {
			t.Fatalf("args=%v produced no diagnostic", args)
		}
	}
}

func TestMainVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		var stdout, stderr bytes.Buffer
		if code := Main(context.Background(), args, IO{Out: &stdout, Err: &stderr}); code != 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
		if stdout.String() != "lamplight dev\n" || stderr.Len() != 0 {
			t.Fatalf("args=%v stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestMainHelp(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"--help"}, []string{"Usage:", "Commands:", "run [TEST_NAME]"}},
		{[]string{"-h"}, []string{"Lamplight runs trace-based integration tests."}},
		{[]string{"help"}, []string{"migrate tracetest", "help [COMMAND]"}},
		{[]string{"help", "run"}, []string{"--var NAME=VALUE", "--keep-artifacts", "--fail-fast"}},
		{[]string{"run", "health", "--help"}, []string{"lamplight run [options] [TEST_NAME]"}},
		{[]string{"list", "tests", "-h"}, []string{"datasource requirements", "--config FILE"}},
		{[]string{"migrate", "tracetest", "--help"}, []string{"Arguments:", "--output-dir DIR"}},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Main(context.Background(), test.args, IO{Out: &stdout, Err: &stderr}); code != 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q", stderr.String())
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("output %q does not contain %q", stdout.String(), want)
				}
			}
		})
	}
}

func TestMainUnknownHelpTopic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"help", "nope"}, IO{Out: &stdout, Err: &stderr}); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), `unknown help topic "nope"`) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestMainMigratesTracetestProject(t *testing.T) {
	input := filepath.Join(t.TempDir(), "legacy.yaml")
	yaml := "type: Test\nspec:\n  name: Health\n  trigger:\n    type: http\n    httpRequest:\n      method: GET\n      url: http://localhost/health\n  specs:\n    - name: ok\n      selector: span[]\n      assertions:\n        - tracetest.response.status = 200\n"
	if err := os.WriteFile(input, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"-v", "migrate", "tracetest", input, "--output-dir", output}, IO{Out: &stdout, Err: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), input+" processed · 1 test") || !strings.Contains(stdout.String(), "Imported 1 test · 0 datasources · Ignored 0 files") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, expected := range []string{`msg="migration configured"`, "input=" + input, "output_dir=" + output, `msg="discovered Tracetest files" count=1`, `msg="wrote migrated file"`} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr missing %q: %q", expected, stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main(context.Background(), []string{"validate", "-w", output}, IO{Out: &stdout, Err: &stderr}); code != 0 || !strings.Contains(stdout.String(), "Valid: 1 tests") {
		t.Fatalf("validate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestVerboseIsGlobalAndWritesDebugOnlyToStderr(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte("project {\n  base_dir = \".\"\n  output = \"json\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"validate", "-w", dir, "-v"}, IO{Out: &stdout, Err: &stderr})
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Valid:") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	for _, expected := range []string{"level=DEBUG", `msg="starting command"`, `msg="loading project"`, `msg="project loaded"`} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr missing %q: %q", expected, stderr.String())
		}
	}

	stderr.Reset()
	stdout.Reset()
	if code := Main(context.Background(), []string{"validate", "-w", dir}, IO{Out: &stdout, Err: &stderr}); code != 0 || stderr.Len() != 0 {
		t.Fatalf("non-verbose code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestMigrateReportsUnknownFlagAfterInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"-v", "migrate", "tracetest", "./tracetest", "--output", "lamplight"}, IO{Out: &stdout, Err: &stderr})
	if code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -output") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestMigrateForceShortAliasAfterInput(t *testing.T) {
	input := filepath.Join(t.TempDir(), "legacy.yaml")
	yaml := "type: Test\nspec:\n  name: Health\n  trigger:\n    type: http\n    httpRequest:\n      method: GET\n      url: http://localhost/health\n"
	if err := os.WriteFile(input, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	for attempt := 0; attempt < 2; attempt++ {
		var stdout, stderr bytes.Buffer
		code := Main(context.Background(), []string{"migrate", "tracetest", input, "--output-dir", output, "-f"}, IO{Out: &stdout, Err: &stderr})
		if code != 0 {
			t.Fatalf("attempt=%d code=%d stdout=%q stderr=%q", attempt, code, stdout.String(), stderr.String())
		}
	}
}

func TestValidateAndListReportLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"validate", "-w", dir}, {"list", "tests", "-w", dir}} {
		var out, stderr bytes.Buffer
		if code := Main(context.Background(), args, IO{Out: &out, Err: &stderr}); code != 1 || stderr.Len() == 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestVarsFlagAndSmallCLIHelpers(t *testing.T) {
	values := varsFlag{}
	if err := values.Set("NAME=value=with-equals"); err != nil || values["NAME"] != "value=with-equals" {
		t.Fatalf("values=%#v err=%v", values, err)
	}
	for _, raw := range []string{"missing-equals", "=empty", "NAME=duplicate"} {
		if err := values.Set(raw); err == nil {
			t.Fatalf("Set(%q) unexpectedly succeeded", raw)
		}
	}
	if values.String() != "" {
		t.Fatal("varsFlag String should be empty")
	}
	if value, err := evalString(cliExpr(t, `"ok"`), nil); err != nil || value != "ok" {
		t.Fatalf("evalString=%q err=%v", value, err)
	}
	if _, err := evalString(cliExpr(t, `1`), nil); err == nil {
		t.Fatal("expected non-string evaluation error")
	}
	if _, err := evalString(cliExpr(t, `var.MISSING`), nil); err == nil {
		t.Fatal("expected unknown variable error")
	}

	definition := &model.ProjectDefinition{
		HTTPProxy: cliExpr(t, `"proxy"`),
		Datasource: &model.DatasourceDefinition{
			Kind:     "tempo",
			Endpoint: cliExpr(t, `"http://localhost:3200"`), BearerToken: cliExpr(t, `"token"`),
			Headers: map[string]hcl.Expression{"X": cliExpr(t, `"header"`)},
		},
		Tests: map[string]model.TestDefinition{
			"test": {
				Name: "test", Outputs: map[string]hcl.Expression{"test": cliExpr(t, `var.VALUE`)},
				Steps: []model.StepDefinition{{
					Name:    "step",
					HTTP:    model.HTTPRequestDefinition{Method: cliExpr(t, `"GET"`), URL: cliExpr(t, `"url"`), Body: cliExpr(t, `"body"`), Headers: map[string]hcl.Expression{"X": cliExpr(t, `"value"`)}},
					Outputs: map[string]hcl.Expression{"step": cliExpr(t, `var.VALUE`)},
					Checks:  []model.CheckDefinition{{Response: map[string]hcl.Expression{"response": cliExpr(t, `true`)}, Spans: &model.SpanCheckDefinition{Matching: cliExpr(t, `true`), Assertions: map[string]hcl.Expression{"span": cliExpr(t, `true`)}}}},
				}},
			},
		},
	}
	expressions := collectExpressions(definition, []model.TestDefinition{definition.Tests["test"]})
	if len(expressions) != 13 {
		t.Fatalf("collected %d expressions", len(expressions))
	}
	valuesForRedaction := map[string]model.SensitiveValue{"VALUE": {Value: cty.StringVal("secret"), Sensitive: true}, "NUMBER": {Value: cty.NumberIntVal(1)}}
	if got := sensitiveStrings(valuesForRedaction); len(got) != 1 || got[0] != "secret" {
		t.Fatalf("sensitive=%#v", got)
	}
	if !testNeedsDatasource(definition.Tests["test"]) || testNeedsDatasource(model.TestDefinition{}) {
		t.Fatal("unexpected datasource requirement")
	}
}

func TestTempoStoreAndDiagnostics(t *testing.T) {
	values := map[string]model.SensitiveValue{"ENDPOINT": {Value: cty.StringVal("http://localhost:3200")}, "TOKEN": {Value: cty.StringVal("token")}}
	definition := &model.DatasourceDefinition{Kind: "tempo", Endpoint: cliExpr(t, `var.ENDPOINT`), Headers: map[string]hcl.Expression{"X": cliExpr(t, `"header"`)}, BearerToken: cliExpr(t, `var.TOKEN`)}
	if store, err := datasourceStore(definition, values); err != nil || store == nil {
		t.Fatalf("store=%#v err=%v", store, err)
	}
	if _, err := datasourceStore(&model.DatasourceDefinition{Kind: "tempo", Endpoint: cliExpr(t, `"not-a-url"`)}, nil); err == nil {
		t.Fatal("expected invalid endpoint error")
	}
	if _, err := datasourceStore(&model.DatasourceDefinition{Kind: "tempo", Endpoint: cliExpr(t, `"http://localhost:3200"`), Headers: map[string]hcl.Expression{"X": cliExpr(t, `var.MISSING`)}}, nil); err == nil {
		t.Fatal("expected invalid header error")
	}
	if _, err := datasourceStore(&model.DatasourceDefinition{Kind: "tempo", Endpoint: cliExpr(t, `"http://localhost:3200"`), BearerToken: cliExpr(t, `var.MISSING`)}, nil); err == nil {
		t.Fatal("expected invalid bearer token error")
	}

	var output bytes.Buffer
	if !printDiagnostics(&output, []model.Diagnostic{{Severity: "warning", Code: "warn", Message: "be careful"}, {Severity: "error", Code: "bad", Message: "failed", File: "test.hcl", Range: model.SourceRange{StartLine: 2, StartColumn: 3}}}) {
		t.Fatal("expected error diagnostic")
	}
	if !strings.Contains(output.String(), "test.hcl:2:3") || !strings.Contains(output.String(), "warning[warn]") {
		t.Fatalf("diagnostics=%q", output.String())
	}
	var usageOutput bytes.Buffer
	usage(&usageOutput)
	if !strings.Contains(usageOutput.String(), "Usage:") || !strings.Contains(usageOutput.String(), "lamplight <command>") {
		t.Fatalf("usage=%q", usageOutput.String())
	}
}

func TestRunReportsSelectionAndVariableErrors(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	config := []byte("project { base_dir = \"tests\" output = \"json\" }\n")
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	testFile := []byte("variable \"REQUIRED\" { type = string }\ntest \"health\" { step \"get\" { http_request { method = \"GET\" url = \"http://127.0.0.1:1\" } } }\n")
	if err := os.WriteFile(filepath.Join(base, "test.wick"), testFile, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"run", "-w", dir, "missing"}, {"run", "-w", dir, "health"}} {
		var out, stderr bytes.Buffer
		if code := Main(context.Background(), args, IO{Out: &out, Err: &stderr}); code != 1 || stderr.Len() == 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestRunContinuesByDefaultAndSupportsFailFastWithCleanJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	testsDir := filepath.Join(dir, "tests")
	if err := os.Mkdir(testsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte("project {\n  base_dir = \"tests\"\n  output = \"json\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	definitions := map[string]string{
		"a-fails.wick":  "test \"first\" {\n  step \"request\" {\n    http_request {\n      method = \"GET\"\n      url = \"http://127.0.0.1:1\"\n    }\n  }\n}\n",
		"b-passes.wick": fmt.Sprintf("test \"second\" {\n  step \"request\" {\n    http_request {\n      method = \"GET\"\n      url = %q\n    }\n  }\n}\n", server.URL),
	}
	for name, body := range definitions {
		if err := os.WriteFile(filepath.Join(testsDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name       string
		flags      []string
		wantSecond model.Status
	}{
		{name: "continue", wantSecond: model.StatusPassed},
		{name: "fail fast", flags: []string{"--fail-fast"}, wantSecond: model.StatusSkipped},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"run", "-w", dir, "--output", "json"}, test.flags...)
			if code := Main(context.Background(), args, IO{Out: &stdout, Err: &stderr}); code != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var run model.RunResult
			if err := json.Unmarshal(stdout.Bytes(), &run); err != nil {
				t.Fatalf("decode output: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			if len(run.Tests) != 2 || run.Tests[0].Status != model.StatusError || run.Tests[1].Status != test.wantSecond {
				t.Fatalf("tests=%#v\nstdout=%s\nstderr=%s", run.Tests, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("JSON output must not include pretty progress on stderr: %q", stderr.String())
			}
		})
	}
}

func TestRunPrefersExplicitPrometheusMetricsOverOTLPDatasource(t *testing.T) {
	trigger := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer trigger.Close()
	prometheus := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/query" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"operations_total"},"value":[1,"7"]}]}}`))
	}))
	defer prometheus.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	otlpEndpoint := "http://" + listener.Addr().String()
	_ = listener.Close()

	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".lamplight"), []byte(fmt.Sprintf(`project {
  base_dir = "."
  output = "json"
}
datasource "otlp" {
  endpoint = %q
}
metrics "prometheus" {
  endpoint = %q
  observation_window = duration("100ms")
  settle_window = duration("10ms")
  polling_interval = duration("5ms")
}
`, otlpEndpoint, prometheus.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "test.wick"), []byte(fmt.Sprintf(`test "metrics source" {
  step "request" {
    http_request {
      method = "GET"
      url = %q
    }
    check "explicit prometheus is used" {
      metrics {
        query = "operations_total"
        assertions = { "value is returned" = metric.value == 7 }
        exactly = 1
      }
    }
  }
}
`, trigger.URL)), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"run", "-w", directory}, IO{Out: &stdout, Err: &stderr}); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var run model.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &run); err != nil || run.Status != model.StatusPassed {
		t.Fatalf("run=%#v decodeErr=%v stdout=%s stderr=%s", run, err, stdout.String(), stderr.String())
	}
}

func TestRunProgressReportsTraceWaitImmediately(t *testing.T) {
	var output bytes.Buffer
	progress := newRunProgress(&output, result.NewRedactor())
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTracePolling, StepName: "request", ObservationWindow: time.Minute})
	if got := output.String(); !strings.Contains(got, "Polling trace spans (up to 1m0s)...") {
		t.Fatalf("progress=%q", got)
	}
}

func TestRunProgressShowsTriggerResultAndSpanMatches(t *testing.T) {
	var output bytes.Buffer
	progress := newRunProgress(&output, result.NewRedactor())
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTriggerStarted, StepName: "request", Trigger: model.TriggerHTTP})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTriggerCompleted, StepName: "request", Trigger: model.TriggerHTTP, Status: model.StatusPassed, StatusCode: 201, DurationMS: 12})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTracePolling, ObservationWindow: time.Minute})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTraceObserved, Attempt: 2, SpanCount: 4, Checks: []engine.ProgressCheck{{Name: "created", MatchCount: 1, Status: model.StatusPassed}}})
	text := output.String()
	for _, expected := range []string{"Running http trigger", "HTTP trigger succeeded · HTTP 201 · 12 ms", "Trace checks", "attempt 2", "4 spans received", "1/1 ready"} {
		if !strings.Contains(text, expected) {
			t.Errorf("progress missing %q: %s", expected, text)
		}
	}
}

func TestRunProgressShowsFailureDetailsNextToTraceResult(t *testing.T) {
	var output bytes.Buffer
	progress := newRunProgress(&output, result.NewRedactor())
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTracePolling, ObservationWindow: time.Minute})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTraceObserved, Attempt: 2, SpanCount: 3, Checks: []engine.ProgressCheck{
		{Name: "request valid", MatchCount: 1, Status: model.StatusFailed, Reason: "span_assertion_failed"},
		{Name: "worker called", MatchCount: 0, Status: model.StatusFailed, Reason: "count_not_satisfied", Rule: model.QuantityRule{Kind: "at_least", Value: 1}},
	}})

	text := output.String()
	for _, expected := range []string{"Trace checks", "request valid", "did not satisfy the assertion", "worker called", "Expected at least 1 matching spans; found 0"} {
		if !strings.Contains(text, expected) {
			t.Errorf("progress missing %q: %s", expected, text)
		}
	}
}

func TestRunProgressSpinnerUpdatesAndFinishesInTerminalMode(t *testing.T) {
	var output bytes.Buffer
	progress := newRunProgress(&output, result.NewRedactor())
	progress.terminal = true
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTriggerStarted, Trigger: model.TriggerHTTP})
	time.Sleep(20 * time.Millisecond)
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTriggerCompleted, Trigger: model.TriggerHTTP, Status: model.StatusError, DurationMS: 20})
	text := output.String()
	if !strings.Contains(text, "\x1b[2K") || !strings.Contains(text, "HTTP trigger failed") {
		t.Fatalf("spinner output=%q", text)
	}
}

func TestRunProgressReplacesTestAndStepStatusInTerminalMode(t *testing.T) {
	var output bytes.Buffer
	progress := newRunProgress(&output, result.NewRedactor())
	progress.terminal = true
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestStarted, TestName: "checkout"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressStepStarted, StepName: "request"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressStepCompleted, StepName: "request", Status: model.StatusPassed, DurationMS: 12})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestCompleted, TestName: "checkout", Status: model.StatusFailed, DurationMS: 15})

	text := output.String()
	if strings.Count(text, "Test: checkout") != 2 || strings.Count(text, "Step: request") != 2 {
		t.Fatalf("status rows should be rewritten, not appended: %q", text)
	}
	for _, expected := range []string{"\x1b[1A", "\x1b[2A", "✓ Step: request", "✗ Test: checkout"} {
		if !strings.Contains(text, expected) {
			t.Errorf("terminal update missing %q: %q", expected, text)
		}
	}
}

func TestRunProgressFitsSpinnerToOneTerminalLine(t *testing.T) {
	progress := newRunProgress(&bytes.Buffer{}, result.NewRedactor())
	progress.width = 40
	got := progress.fitSpinnerText("    ", strings.Repeat("very long polling status ", 5))
	if len([]rune(got)) > 34 || !strings.HasSuffix(got, "…") {
		t.Fatalf("fitted text=%q len=%d", got, len([]rune(got)))
	}
}

func TestContainsExecutableK6DistinguishesLegacyTraceAttachment(t *testing.T) {
	executable := []model.TestDefinition{{Steps: []model.StepDefinition{{Trigger: model.TriggerDefinition{Kind: model.TriggerK6, Attributes: map[string]hcl.Expression{"script": cliExpr(t, `"load.js"`)}}}}}}
	legacy := []model.TestDefinition{{Steps: []model.StepDefinition{{Trigger: model.TriggerDefinition{Kind: model.TriggerK6, Attributes: map[string]hcl.Expression{"id": cliExpr(t, `"0123456789abcdef0123456789abcdef"`)}}}}}}
	if !containsExecutableK6(executable) || containsExecutableK6(legacy) {
		t.Fatal("k6 execution detection did not distinguish script and id forms")
	}
}
