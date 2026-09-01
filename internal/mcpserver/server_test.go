package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"lamplight/internal/model"
)

func TestServerListsReadsWritesAndRollsBack(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	configContent := []byte("project {\n  base_dir = \"./tests\"\n  default_target = \"compose\"\n}\nvariable \"BASE_URL\" { type = string }\ntarget \"compose\" {\n  runtime = \"docker_compose\"\n  variables = { BASE_URL = \"http://api:8080\" }\n  docker_compose { services = [\"api\"] }\n}\n")
	if err := os.WriteFile(filepath.Join(root, ".lamplight"), configContent, 0o644); err != nil {
		t.Fatal(err)
	}
	original := []byte("test \"health\" {\n  step \"get\" {\n    http_request {\n      method = \"GET\"\n      url    = \"http://example.test\"\n    }\n  }\n}\n")
	if err := os.WriteFile(filepath.Join(base, "health.wick"), original, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var runArgs []string
	server := New(Options{WorkingDir: root, RunCLI: func(_ context.Context, args []string) (int, []byte, []byte) {
		runArgs = append([]string(nil), args...)
		for index, argument := range args {
			if argument == "--json-file" && index+1 < len(args) {
				if err := os.WriteFile(args[index+1], []byte(`{"schema_version":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return 0, []byte("pretty result"), []byte("progress")
	}, ObserveTrace: func(_ context.Context, request ObserveTraceRequest) (TraceEvidence, error) {
		return TraceEvidence{TraceID: request.TraceID, Found: true, Valid: true, SpanCount: 1, Spans: []model.Span{{Name: "GET /health"}}}, nil
	}})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 15 {
		t.Fatalf("got %d tools, want 15", len(tools.Tools))
	}
	capabilitiesResult := call(t, ctx, clientSession, "lamplight_get_capabilities", nil)
	var capabilities capabilitiesOutput
	decodeStructured(t, capabilitiesResult, &capabilities)
	if len(capabilities.Triggers) != 10 || len(capabilities.Functions) == 0 {
		t.Fatalf("capabilities=%#v", capabilities)
	}
	metricContext := strings.Join(capabilities.Checks.MetricContext, ",")
	for _, field := range []string{"metric.type", "metric.attributes", "metric.resource"} {
		if !strings.Contains(metricContext, field) {
			t.Errorf("metric context does not advertise %s: %v", field, capabilities.Checks.MetricContext)
		}
	}
	for _, trigger := range capabilities.Triggers {
		scaffoldResult := call(t, ctx, clientSession, "lamplight_scaffold_test", map[string]any{"trigger": trigger.Block, "test_name": "scaffold_" + trigger.Block})
		if scaffoldResult.IsError {
			t.Fatalf("scaffold %s failed: %#v", trigger.Block, scaffoldResult.Content)
		}
		var scaffold scaffoldOutput
		decodeStructured(t, scaffoldResult, &scaffold)
		validateResult := call(t, ctx, clientSession, "lamplight_validate_test_content", map[string]any{"path": "candidate-" + trigger.Block + ".wick", "content": scaffold.Content})
		if validateResult.IsError {
			t.Fatalf("validate %s failed: %#v", trigger.Block, validateResult.Content)
		}
	}
	referenceResult := call(t, ctx, clientSession, "lamplight_get_dsl_reference", map[string]any{"topic": "checks"})
	var reference referenceOutput
	decodeStructured(t, referenceResult, &reference)
	if reference.Topic != "checks" || reference.Reference == "" {
		t.Fatalf("reference=%#v", reference)
	}
	traceResult := call(t, ctx, clientSession, "lamplight_observe_trace", map[string]any{"trace_id": "0123456789abcdef0123456789abcdef"})
	var evidence TraceEvidence
	decodeStructured(t, traceResult, &evidence)
	if !evidence.Found || evidence.SpanCount != 1 || len(evidence.Spans) != 1 {
		t.Fatalf("evidence=%#v", evidence)
	}
	listResult := call(t, ctx, clientSession, "lamplight_list_tests", nil)
	var listed listOutput
	decodeStructured(t, listResult, &listed)
	if listed.DefaultTarget != "compose" || len(listed.Targets) != 1 || listed.Targets[0] != (targetSummary{Name: "compose", Runtime: "docker_compose"}) {
		t.Fatalf("unexpected target metadata: %#v", listed)
	}
	runResult := call(t, ctx, clientSession, "lamplight_run_tests", map[string]any{"test": "health", "target": "compose", "variables": map[string]any{"BASE_URL": "http://example.test"}})
	if runResult.IsError {
		t.Fatal("run tool unexpectedly failed")
	}
	if len(runArgs) < 2 || runArgs[0] != "run" || runArgs[len(runArgs)-1] != "health" {
		t.Fatalf("unexpected CLI run arguments: %q", runArgs)
	}
	if !containsPair(runArgs, "--target", "compose") {
		t.Fatalf("run did not forward target: %q", runArgs)
	}
	runResult = call(t, ctx, clientSession, "lamplight_run_tests", map[string]any{"tags": []string{"slow", "flaky"}, "exclude": true})
	if runResult.IsError || !containsPair(runArgs, "--tag", "slow") || !containsPair(runArgs, "--tag", "flaky") || !containsValue(runArgs, "--exclude") {
		t.Fatalf("run did not forward repeated excluded tags: result=%#v args=%q", runResult, runArgs)
	}

	configReadResult := call(t, ctx, clientSession, "lamplight_read_project_config", nil)
	var configRead fileOutput
	decodeStructured(t, configReadResult, &configRead)
	if configRead.SHA256 == "" || configRead.Content != string(configContent) {
		t.Fatalf("unexpected config read: %#v", configRead)
	}
	updatedConfig := strings.Replace(configRead.Content, "default_target = \"compose\"", "default_target = \"local-dev\"", 1) + "\ntarget \"local-dev\" { runtime = \"local\" }\n"
	configWriteResult := call(t, ctx, clientSession, "lamplight_write_project_config", map[string]any{"content": updatedConfig, "expected_sha256": configRead.SHA256})
	if configWriteResult.IsError {
		t.Fatalf("valid config write failed: %#v", configWriteResult.Content)
	}
	var configWritten mutationOutput
	decodeStructured(t, configWriteResult, &configWritten)
	if !configWritten.Changed {
		t.Fatal("valid config write reported no change")
	}
	invalidConfigResult := call(t, ctx, clientSession, "lamplight_write_project_config", map[string]any{"content": "project {}", "expected_sha256": configWritten.SHA256})
	if !invalidConfigResult.IsError {
		t.Fatal("invalid config write unexpectedly succeeded")
	}
	configAfter, err := os.ReadFile(filepath.Join(root, ".lamplight"))
	if err != nil {
		t.Fatal(err)
	}
	if digest(configAfter) != configWritten.SHA256 {
		t.Fatal("invalid config write was not rolled back")
	}

	readResult := call(t, ctx, clientSession, "lamplight_read_test_file", map[string]any{"path": "health.wick"})
	var read fileOutput
	decodeStructured(t, readResult, &read)
	if read.SHA256 == "" || read.Content != string(original) {
		t.Fatalf("unexpected read output: %#v (structured: %T %#v)", read, readResult.StructuredContent, readResult.StructuredContent)
	}

	valid := "test \"health\" {\n  step \"get\" {\n    http_request {\n      method = \"GET\"\n      url = \"http://example.test/ready\"\n    }\n  }\n}\n"
	writeResult := call(t, ctx, clientSession, "lamplight_write_test_file", map[string]any{"path": "health.wick", "content": valid, "expected_sha256": read.SHA256})
	if writeResult.IsError {
		t.Fatalf("valid write failed: %#v", writeResult.Content)
	}
	var written mutationOutput
	decodeStructured(t, writeResult, &written)
	if !written.Changed || written.SHA256 == read.SHA256 {
		t.Fatalf("unexpected write output: %#v", written)
	}
	accepted, err := os.ReadFile(filepath.Join(base, "health.wick"))
	if err != nil {
		t.Fatal(err)
	}

	invalid := "test \"health\" {}"
	invalidResult := call(t, ctx, clientSession, "lamplight_write_test_file", map[string]any{"path": "health.wick", "content": invalid, "expected_sha256": written.SHA256})
	if !invalidResult.IsError {
		t.Fatal("invalid write unexpectedly succeeded")
	}
	after, err := os.ReadFile(filepath.Join(base, "health.wick"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(accepted) {
		t.Fatal("invalid write was not rolled back")
	}
}

func containsValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsPair(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

func TestServerRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lamplight"), []byte("project {\n  base_dir = \"./tests\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	server := New(Options{WorkingDir: root})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	result := call(t, ctx, cs, "lamplight_read_test_file", map[string]any{"path": "../secret.wick"})
	if !result.IsError {
		t.Fatal("escaping path unexpectedly succeeded")
	}
}

func call(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func decodeStructured(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
