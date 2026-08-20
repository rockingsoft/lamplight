package hclloader

import (
	"os"
	"path/filepath"
	"testing"

	"tracetest/internal/config"
)

func TestSpanExpressionUsesSpanContext(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	root := "project { base_dir = \"tests\" }\n" +
		"datasource \"tempo\" { endpoint = \"http://tempo:3200\" }\n"
	if err := os.WriteFile(filepath.Join(dir, ".tracetest.hcl"), []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := `test "trace" {
  step "request" {
    http_request {
      method = "GET"
      url = "http://example.test"
    }
    check "span" {
      spans {
        matching = span.name == "work" && resource.attributes["service.name"] == "example"
        at_least = 1
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(base, "trace.hcl"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	_, diagnostics := (Loader{}).LoadProject(config.Options{WorkingDir: dir})
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			t.Fatalf("span context should validate: %#v", diagnostics)
		}
	}
}
