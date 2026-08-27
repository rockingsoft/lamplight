package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"lamplight/internal/model"
)

func TestValidateListAndRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	base := filepath.Join(dir, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	configBody := "project {\n  base_dir = \"tests\"\n  output = \"json\"\n}\n"
	testBody := `variable "BASE_URL" { default = "` + server.URL + `" }
test "health" {
  tags = ["smoke"]
  step "get" {
    http_request {
      method = "GET"
      url = "${var.BASE_URL}/health"
    }
    check "healthy" {
      response = { ok = response.status_code == 200 }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "health.wick"), []byte(testBody), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"validate", []string{"validate", "-w", dir}, "Valid: 1 tests"},
		{"list", []string{"list", "tests", "-w", dir}, "health"},
		{"run", []string{"run", "-w", dir, "--output", "json", "health"}, `"status": "passed"`},
		{"run flags after name", []string{"run", "-w", dir, "health", "--output", "json", "--keep-artifacts"}, `"status": "passed"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := Main(context.Background(), tc.args, IO{Out: &out, Err: &stderr}); code != 0 {
				t.Fatalf("code=%d stderr=%s out=%s", code, stderr.String(), out.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output %q does not contain %q", out.String(), tc.want)
			}
		})
	}
}

func TestRunIncludesOrExcludesRepeatedSelectors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	base := filepath.Join(dir, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte("project {\n  base_dir = \"tests\"\n  output = \"json\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTests := func(file, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(base, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	testDefinition := func(name, tag string) string {
		return fmt.Sprintf(`test %q {
  tags = [%q]
  step "request" {
    http_request {
      method = "GET"
      url    = %q
    }
  }
}
`, name, tag, server.URL)
	}
	writeTests("group.wick", testDefinition("smoke-test", "smoke")+testDefinition("slow-test", "slow"))
	writeTests("other.wick", testDefinition("other-test", "regression"))

	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "repeated tags excluded", args: []string{"--tag", "smoke", "--tag", "slow", "--exclude"}, want: []string{"other-test"}},
		{name: "file", args: []string{"--file", "group.wick"}, want: []string{"smoke-test", "slow-test"}},
		{name: "name excluded with trailing flag", args: []string{"slow-test", "--exclude"}, want: []string{"other-test", "smoke-test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"run", "-w", dir}, test.args...)
			var stdout, stderr bytes.Buffer
			if code := Main(context.Background(), args, IO{Out: &stdout, Err: &stderr}); code != 0 {
				t.Fatalf("code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
			}
			var result model.RunResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode result: %v\n%s", err, stdout.String())
			}
			got := make([]string, 0, len(result.Tests))
			for _, executed := range result.Tests {
				got = append(got, executed.Name)
			}
			sort.Strings(got)
			sort.Strings(test.want)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("executed tests=%v, want %v", got, test.want)
			}
		})
	}
}

func TestRunUsesDefaultLocalTargetVariables(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	dir := t.TempDir()
	base := filepath.Join(dir, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	configBody := `project {
  base_dir = "tests"
  output = "json"
  default_target = "configured_local"
}
variable "BASE_URL" { type = string }
target "configured_local" {
  runtime = "local"
  variables = { BASE_URL = "` + server.URL + `" }
}
`
	testBody := `test "health" {
  step "get" {
    http_request {
      method = "GET"
      url = "${var.BASE_URL}/health"
    }
    check "healthy" { response = { ok = response.status_code == 204 } }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "health.wick"), []byte(testBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"run", "-w", dir, "health"}, IO{Out: &out, Err: &stderr}); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, stderr.String(), out.String())
	}
	if !strings.Contains(out.String(), `"status": "passed"`) {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRunFallsBackToImplicitLocalWhenTargetsExistWithoutDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	configBody := `project {
  base_dir = "tests"
  output = "json"
}
variable "BASE_URL" { type = string }
target "compose" { runtime = "docker_compose" }
`
	testBody := `test "health" {
  step "get" {
    http_request {
      method = "GET"
      url = "${var.BASE_URL}/health"
    }
    check "healthy" { response = { ok = response.status_code == 204 } }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tests", "health.wick"), []byte(testBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAMPLIGHT_VAR_BASE_URL", server.URL)
	var out, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"run", "-w", dir, "health"}, IO{Out: &out, Err: &stderr}); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, stderr.String(), out.String())
	}
}
