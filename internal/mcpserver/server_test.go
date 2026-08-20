package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerListsReadsWritesAndRollsBack(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lamplight"), []byte("project {\n  base_dir = \"./tests\"\n  output = \"json\"\n}\n"), 0o644); err != nil {
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
		return 0, []byte(`{"schema_version":"1"}`), nil
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
	if len(tools.Tools) != 7 {
		t.Fatalf("got %d tools, want 7", len(tools.Tools))
	}
	runResult := call(t, ctx, clientSession, "lamplight_run_tests", map[string]any{"test": "health", "variables": map[string]any{"BASE_URL": "http://example.test"}})
	if runResult.IsError {
		t.Fatal("run tool unexpectedly failed")
	}
	if len(runArgs) < 2 || runArgs[0] != "run" || runArgs[len(runArgs)-1] != "health" {
		t.Fatalf("unexpected CLI run arguments: %q", runArgs)
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
