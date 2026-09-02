package k6cloudrun

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientRunsDistributedJobAndCleansRunObjects(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "load.js")
	helper := filepath.Join(directory, "lib", "helper.js")
	if err := os.Mkdir(filepath.Dir(helper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("import './lib/helper.js';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("export const ok = true;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	objects := map[string][]byte{}
	deleted := map[string]bool{}
	var runBody map[string]any
	var progress []Progress
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/upload/storage/v1/"):
			data, _ := io.ReadAll(request.Body)
			mutex.Lock()
			objects[request.URL.Query().Get("name")] = data
			mutex.Unlock()
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/jobs/loadtest"):
			writeJSON(t, writer, map[string]any{"template": map[string]any{"parallelism": 4, "template": map[string]any{"maxRetries": 0}}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/jobs/loadtest:run"):
			if err := json.NewDecoder(request.Body).Decode(&runBody); err != nil {
				t.Error(err)
			}
			writeJSON(t, writer, map[string]any{"name": "projects/p/locations/r/operations/one"})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/operations/one"):
			writeJSON(t, writer, map[string]any{"name": "projects/p/locations/r/operations/one", "done": true, "response": map[string]any{"name": "projects/p/locations/r/jobs/loadtest/executions/run-1", "logUri": "https://console.example/logs"}})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/executions/run-1"):
			writeJSON(t, writer, map[string]any{"name": "projects/p/locations/r/jobs/loadtest/executions/run-1", "logUri": "https://console.example/logs", "completionTime": "2026-09-02T12:01:39Z"})
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/results/"):
			index := 0
			if strings.Contains(request.URL.Path, "%2F1.json") || strings.HasSuffix(request.URL.Path, "/1.json") {
				index = 1
			}
			writeJSON(t, writer, ShardResult{Index: index, ExitCode: 0, Summary: map[string]any{"ok": true}})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/storage/v1/"):
			deleted[request.URL.Path] = true
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newWithHTTPClient(server.Client())
	client.runBase, client.storageBase, client.poll = server.URL, server.URL, time.Millisecond
	client.now = func() time.Time { return time.Unix(100, 0) }
	result, err := client.Run(context.Background(), Request{Project: "p", Region: "r", Job: "loadtest", Bucket: "bucket", Tasks: 2, Timeout: 20 * time.Minute, StartDelay: 15 * time.Second, Script: script, BundleRoot: directory, Files: []string{helper}, Environment: map[string]string{"BASE_URL": "https://example.test"}, JobEnvironment: []string{"TOKEN"}, TraceParent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", TraceState: "lamplight=true", OutputLimit: 1 << 20, Progress: func(event Progress) { progress = append(progress, event) }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Execution == "" || len(result.Shards) != 2 || result.Shards[1].Index != 1 {
		t.Fatalf("result=%#v", result)
	}
	if len(progress) < 4 || progress[0].Phase != "running" || progress[len(progress)-1].Phase != "completed" || progress[len(progress)-1].CompletedShards != 2 {
		t.Fatalf("progress=%#v", progress)
	}
	overrides := runBody["overrides"].(map[string]any)
	if overrides["taskCount"] != float64(2) || overrides["timeout"] != "1200s" {
		t.Fatalf("overrides=%#v", overrides)
	}
	encodedRun, _ := json.Marshal(runBody)
	if bytes.Contains(encodedRun, []byte("example.test")) || bytes.Contains(encodedRun, []byte("traceparent")) {
		t.Fatalf("run override leaked sensitive config: %s", encodedRun)
	}
	mutex.Lock()
	defer mutex.Unlock()
	var config TaskConfig
	for name, data := range objects {
		if !strings.HasPrefix(name, "runs/") {
			t.Fatalf("object %q is outside the Terraform-authorized runs/ prefix", name)
		}
		if strings.HasSuffix(name, "/config.json") {
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
		}
	}
	if config.Environment["BASE_URL"] != "https://example.test" || len(config.JobEnvironment) != 1 || config.JobEnvironment[0] != "TOKEN" || config.TraceParent == "" || !config.StartAt.Equal(time.Unix(115, 0)) {
		t.Fatalf("config=%#v", config)
	}
	if len(deleted) != 4 {
		t.Fatalf("deleted=%d want 4", len(deleted))
	}
}

func TestClientCollectsShardEvidenceWhenCloudRunOperationFails(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "load.js")
	if err := os.WriteFile(script, []byte("export default function() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/upload/storage/v1/"):
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/jobs/loadtest"):
			writeJSON(t, writer, map[string]any{"template": map[string]any{"parallelism": 1, "template": map[string]any{"maxRetries": 0}}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/jobs/loadtest:run"):
			writeJSON(t, writer, map[string]any{"name": "projects/p/locations/r/operations/failed"})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/operations/failed"):
			writeJSON(t, writer, map[string]any{
				"name": "projects/p/locations/r/operations/failed", "done": true,
				"error":    map[string]any{"code": 10, "message": "task failed"},
				"metadata": map[string]any{"name": "projects/p/locations/r/jobs/loadtest/executions/run-failed"},
			})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/executions/run-failed"):
			writeJSON(t, writer, map[string]any{"name": "projects/p/locations/r/jobs/loadtest/executions/run-failed", "completionTime": "2026-09-02T12:01:39Z", "failedCount": 1})
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/results/"):
			writeJSON(t, writer, ShardResult{Index: 0, ExitCode: 99, Stderr: "threshold crossed", Summary: map[string]any{"failed": true}})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/storage/v1/"):
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newWithHTTPClient(server.Client())
	client.runBase, client.storageBase, client.poll = server.URL, server.URL, time.Millisecond
	result, err := client.Run(context.Background(), Request{Project: "p", Region: "r", Job: "loadtest", Bucket: "bucket", Tasks: 1, Timeout: time.Minute, Script: script, BundleRoot: directory, OutputLimit: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "threshold crossed") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(result.Shards) != 1 || result.Shards[0].Summary == nil || result.Execution == "" {
		t.Fatalf("result=%#v", result)
	}
}

func TestClientWaitsForExecutionCompletionBeforeCollectingAndCleaningObjects(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "load.js")
	if err := os.WriteFile(script, []byte("export default function() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	executionPolls := 0
	resultReadsBeforeCompletion := 0
	configDeletedBeforeCompletion := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/upload/storage/v1/"):
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/jobs/loadtest"):
			writeJSON(t, writer, map[string]any{"template": map[string]any{"parallelism": 1, "template": map[string]any{"maxRetries": 0}}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/jobs/loadtest:run"):
			writeJSON(t, writer, map[string]any{"name": "projects/p/locations/r/operations/created"})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/operations/created"):
			writeJSON(t, writer, map[string]any{"name": "projects/p/locations/r/operations/created", "done": true, "response": map[string]any{"name": "projects/p/locations/r/jobs/loadtest/executions/run-racing"}})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/executions/run-racing"):
			mutex.Lock()
			executionPolls++
			polls := executionPolls
			mutex.Unlock()
			response := map[string]any{"name": "projects/p/locations/r/jobs/loadtest/executions/run-racing"}
			if polls >= 3 {
				response["completionTime"] = "2026-09-02T12:01:39Z"
			}
			writeJSON(t, writer, response)
		case request.Method == http.MethodHead && strings.Contains(request.URL.Path, "/results/"):
			writer.WriteHeader(http.StatusNotFound)
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/results/"):
			mutex.Lock()
			if executionPolls < 3 {
				resultReadsBeforeCompletion++
			}
			mutex.Unlock()
			writeJSON(t, writer, ShardResult{Index: 0, ExitCode: 0})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/storage/v1/"):
			mutex.Lock()
			if strings.Contains(request.URL.Path, "config.json") && executionPolls < 3 {
				configDeletedBeforeCompletion = true
			}
			mutex.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newWithHTTPClient(server.Client())
	client.runBase, client.storageBase, client.poll = server.URL, server.URL, time.Millisecond
	result, err := client.Run(context.Background(), Request{Project: "p", Region: "r", Job: "loadtest", Bucket: "bucket", Tasks: 1, Timeout: time.Minute, Script: script, BundleRoot: directory, OutputLimit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if executionPolls != 3 || resultReadsBeforeCompletion != 0 || configDeletedBeforeCompletion {
		t.Fatalf("executionPolls=%d resultReadsBeforeCompletion=%d configDeletedBeforeCompletion=%t result=%#v", executionPolls, resultReadsBeforeCompletion, configDeletedBeforeCompletion, result)
	}
}

func TestWaitOperationReportsPartialShardCompletion(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/operations/run"):
			polls++
			writeJSON(t, writer, map[string]any{"name": "operations/run", "done": polls >= 3})
		case request.Method == http.MethodHead && strings.Contains(request.URL.Path, "0.json"):
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodHead:
			writer.WriteHeader(http.StatusNotFound)
		default:
			http.Error(writer, request.Method+" "+request.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newWithHTTPClient(server.Client())
	client.runBase, client.storageBase, client.poll = server.URL, server.URL, time.Millisecond
	now := time.Unix(100, 0)
	client.now = func() time.Time {
		now = now.Add(5 * time.Second)
		return now
	}
	var progress []Progress
	_, err := client.waitOperation(context.Background(), operation{Name: "operations/run"}, "bucket", []string{"runs/id/results/0.json", "runs/id/results/1.json"}, time.Unix(100, 0), func(event Progress) {
		progress = append(progress, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	foundPartial := false
	for _, event := range progress {
		if event.CompletedShards == 1 && event.TotalShards == 2 {
			foundPartial = true
		}
	}
	if !foundPartial {
		t.Fatalf("progress=%#v", progress)
	}
}

func TestNewAcceptsExplicitGoogleOAuthAccessToken(t *testing.T) {
	t.Setenv("GOOGLE_OAUTH_ACCESS_TOKEN", "temporary-token")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer temporary-token" {
			t.Errorf("Authorization=%q", got)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

func TestBuildBundleUsesProjectRelativeNames(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "k6", "load.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, scriptName, err := buildBundle(directory, script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scriptName != "k6/load.js" {
		t.Fatalf("script=%q", scriptName)
	}
	gzipReader, _ := gzip.NewReader(bytes.NewReader(bundle))
	header, err := tar.NewReader(gzipReader).Next()
	if err != nil || header.Name != "k6/load.js" {
		t.Fatalf("header=%#v err=%v", header, err)
	}
}

func TestTaskHelpersPartitionAndProtectInputs(t *testing.T) {
	if got := executionSegmentSequence(4); got != "0,1/4,1/2,3/4,1" {
		t.Fatalf("sequence=%q", got)
	}
	if got := executionSegment(2, 4); got != "1/2:3/4" {
		t.Fatalf("segment=%q", got)
	}
	archive := new(bytes.Buffer)
	gzipWriter := gzip.NewWriter(archive)
	tarWriter := tar.NewWriter(gzipWriter)
	_ = tarWriter.WriteHeader(&tar.Header{Name: "../escape", Size: 1, Mode: 0o600})
	_, _ = tarWriter.Write([]byte("x"))
	_ = tarWriter.Close()
	_ = gzipWriter.Close()
	if err := extractBundle(archive.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("err=%v", err)
	}
	redacted := redactTaskOutput("token=secret", map[string]string{"TOKEN": "secret"})
	if strings.Contains(redacted, "secret") {
		t.Fatalf("redacted=%q", redacted)
	}
	t.Setenv("LAMPLIGHT_VAR_TOKEN", "secret")
	config := TaskConfig{Environment: map[string]string{"BASE_URL": "https://example.test"}, JobEnvironment: []string{"TOKEN"}}
	environment, err := taskEnvironment(&config)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "TOKEN=secret") || strings.Contains(joined, "LAMPLIGHT_VAR_TOKEN=") {
		t.Fatalf("environment=%q", joined)
	}
	if output := redactTaskOutput("token=secret", config.Environment); strings.Contains(output, "secret") {
		t.Fatalf("inherited secret was not redacted: %q", output)
	}
}

func TestValidateRequestRejectsBundledJobEnvironment(t *testing.T) {
	err := validateRequest(Request{Project: "p", Region: "r", Job: "j", Bucket: "b", Tasks: 1, Timeout: time.Minute, Script: "script", BundleRoot: "root", OutputLimit: 1, Environment: map[string]string{"TOKEN": "secret"}, JobEnvironment: []string{"TOKEN"}})
	if err == nil || !strings.Contains(err.Error(), "must not be included") {
		t.Fatalf("err=%v", err)
	}
}

func TestExecuteShardKeepsSummaryOnNonzeroExit(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "load.js"), []byte("export default function() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	helper := filepath.Join(bin, "k6")
	script := `#!/bin/sh
summary=""
all="$*"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--summary-export" ]; then summary="$2"; shift 2; else shift; fi
done
printf '%s' '{"metrics":{"checks":{"values":{"passes":1}}}}' > "$summary"
printf '%s' "$all"
exit 99
`
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	result := executeShard(context.Background(), directory, TaskConfig{Script: "load.js", OutputLimit: 1 << 20}, 2, 4)
	if result.ExitCode != 99 || result.Summary == nil || !strings.Contains(result.Stdout, "--execution-segment 1/2:3/4") || !strings.Contains(result.Stdout, "--execution-segment-sequence 0,1/4,1/2,3/4,1") {
		t.Fatalf("result=%#v", result)
	}
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Error(err)
	}
}
