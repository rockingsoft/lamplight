package trigger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lamplight/internal/k6cloudrun"
	"lamplight/internal/model"
)

type fakeCloudRun struct {
	request k6cloudrun.Request
	result  k6cloudrun.Result
	err     error
}

func (f *fakeCloudRun) Run(_ context.Context, request k6cloudrun.Request) (k6cloudrun.Result, error) {
	f.request = request
	if f.result.Shards != nil || f.err != nil {
		return f.result, f.err
	}
	return k6cloudrun.Result{Execution: "execution", Shards: []k6cloudrun.ShardResult{{Index: 0, ExitCode: 0, Summary: map[string]any{"ok": true}}, {Index: 1, ExitCode: 0}}}, nil
}

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

func TestExecuteK6RunsScriptAndPropagatesTraceContext(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "load.js")
	if err := os.WriteFile(script, []byte("export default function () {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := New(nil)
	executor.command = k6HelperCommand
	executor.lookPath = func(string) (string, error) { return "k6", nil }
	trace := &model.TestTraceContext{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", TraceState: "lamplight=true"}
	response, err := executor.Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{
		"script": script,
		"env":    map[string]any{"BASE_URL": "https://example.test", "LAMPLIGHT_TRACEPARENT": "untrusted"},
		"arguments": map[string]any{
			"vus":        float64(1),
			"iterations": float64(1),
			"quiet":      false,
			"tag":        []any{"suite=smoke", "team=checkout"},
		},
	}}, model.DefaultHTTPClientConfig(), trace)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 0 || !strings.Contains(response.Body, "BASE_URL=https://example.test") || !strings.Contains(response.Body, "TRACEPARENT="+trace.TraceParent()) || !strings.Contains(response.Body, "ARGUMENTS=--iterations=1,--tag=suite=smoke,--tag=team=checkout,--vus=1") {
		t.Fatalf("response=%#v", response)
	}
	result, ok := response.JSON.(map[string]any)
	if !ok || result["summary"] == nil || result["exit_code"] != 0 {
		t.Fatalf("json=%#v", response.JSON)
	}
}

func TestK6ArgumentsRequiresMapAndProtectsSummary(t *testing.T) {
	if _, err := k6Arguments([]any{"--vus", "1"}); err == nil || !strings.Contains(err.Error(), "must be a map") {
		t.Fatalf("err=%v", err)
	}
	if _, err := k6Arguments(map[string]any{"summary_export": "other.json"}); err == nil || !strings.Contains(err.Error(), "cannot override") {
		t.Fatalf("err=%v", err)
	}
}

func TestExecuteK6ReportsThresholdFailure(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "load.js")
	if err := os.WriteFile(script, []byte("export default function () {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := New(nil)
	executor.command = k6HelperCommand
	executor.lookPath = func(string) (string, error) { return "k6", nil }
	response, err := executor.Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{
		"script": script,
		"env":    map[string]any{"K6_HELPER_EXIT_CODE": "99"},
	}}, model.DefaultHTTPClientConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), "k6 exited with code 99") || response.StatusCode != 99 {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestExecuteK6RequiresBinaryInPath(t *testing.T) {
	executor := New(nil)
	executor.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	_, err := executor.Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{"script": "/tmp/load.js"}}, model.DefaultHTTPClientConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), "k6 executable not found in PATH") {
		t.Fatalf("err=%v", err)
	}
}

func TestExecuteK6CloudRunDoesNotRequireLocalBinary(t *testing.T) {
	runner := &fakeCloudRun{}
	executor := New(nil)
	executor.cloudRun = runner
	trace := &model.TestTraceContext{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", TraceState: "lamplight=true"}
	response, err := executor.Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{
		"script": "/project/load.js", "bundle_root": "/project", "env": map[string]any{"TOKEN": "secret", "BASE_URL": "https://example.test"},
		"executor": map[string]any{"kind": "cloud_run", "project": "p", "region": "r", "job": "j", "bucket": "b", "tasks": float64(2), "job_env": []any{"TOKEN"}},
	}}, model.DefaultHTTPClientConfig(), trace)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 0 || runner.request.Tasks != 2 || runner.request.TraceParent != trace.TraceParent() || runner.request.Environment["TOKEN"] != "" || runner.request.Environment["BASE_URL"] == "" || len(runner.request.JobEnvironment) != 1 {
		t.Fatalf("response=%#v request=%#v", response, runner.request)
	}
}

func TestExecuteK6CloudRunJobEnvironmentMustExistInTriggerEnvironment(t *testing.T) {
	executor := New(nil)
	executor.cloudRun = &fakeCloudRun{}
	_, err := executor.Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{
		"script": "/project/load.js", "bundle_root": "/project",
		"executor": map[string]any{"kind": "cloud_run", "project": "p", "region": "r", "job": "j", "bucket": "b", "tasks": float64(1), "job_env": []any{"TOKEN"}},
	}}, model.DefaultHTTPClientConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), "not present in k6.env") {
		t.Fatalf("err=%v", err)
	}
}

func TestExecuteK6CloudRunReturnsShardEvidenceOnFailure(t *testing.T) {
	runner := &fakeCloudRun{result: k6cloudrun.Result{Execution: "execution", Shards: []k6cloudrun.ShardResult{{Index: 0, ExitCode: 99, Summary: map[string]any{"thresholds": "failed"}}}}, err: errors.New("shard failed")}
	executor := New(nil)
	executor.cloudRun = runner
	response, err := executor.Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{
		"script": "/project/load.js", "bundle_root": "/project",
		"executor": map[string]any{"kind": "cloud_run", "project": "p", "region": "r", "job": "j", "bucket": "b", "tasks": float64(1)},
	}}, model.DefaultHTTPClientConfig(), nil)
	if err == nil || response.StatusCode != 99 {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	jsonResult := response.JSON.(map[string]any)
	shards := jsonResult["shards"].([]any)
	if shards[0].(map[string]any)["summary"] == nil {
		t.Fatalf("json=%#v", jsonResult)
	}
}

func k6HelperCommand(ctx context.Context, _ string, args ...string) *exec.Cmd {
	commandArgs := []string{"-test.run=TestK6HelperProcess", "--"}
	commandArgs = append(commandArgs, args...)
	return exec.CommandContext(ctx, os.Args[0], commandArgs...)
}

func TestK6HelperProcess(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		return
	}
	args := os.Args[separator+1:]
	summaryPath := ""
	for index, argument := range args {
		if argument == "--summary-export" && index+1 < len(args) {
			summaryPath = args[index+1]
		}
	}
	if summaryPath == "" {
		fmt.Fprintln(os.Stderr, "missing summary path")
		os.Exit(2)
	}
	if err := os.WriteFile(summaryPath, []byte(`{"metrics":{"http_req_failed":{"type":"rate"}}}`), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	arguments := []string{}
	if len(args) > 4 {
		arguments = args[3 : len(args)-1]
	}
	fmt.Printf("BASE_URL=%s\nTRACEPARENT=%s\nARGUMENTS=%s\n", os.Getenv("BASE_URL"), os.Getenv("LAMPLIGHT_TRACEPARENT"), strings.Join(arguments, ","))
	if os.Getenv("K6_HELPER_EXIT_CODE") == "99" {
		fmt.Fprintln(os.Stderr, "thresholds failed")
		os.Exit(99)
	}
}
