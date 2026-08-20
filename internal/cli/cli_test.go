package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateListAndRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
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
