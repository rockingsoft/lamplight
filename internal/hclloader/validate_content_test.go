package hclloader

import (
	"os"
	"path/filepath"
	"testing"

	"lamplight/internal/config"
)

func TestValidateTestContentDoesNotWriteAndReplacesDefinitionsInMemory(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "tests")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".lamplight"), []byte("project { base_dir = \"tests\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "health.wick")
	original := []byte("test \"health\" {\n  step \"get\" {\n    http_request {\n      method = \"GET\"\n      url = \"https://example.test\"\n    }\n  }\n}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := []byte("variable \"BASE_URL\" {\n  type = string\n  default = \"https://example.test\"\n}\ntest \"health\" {\n  step \"get\" {\n    graphql_request {\n      url = var.BASE_URL\n      query = \"query { health }\"\n    }\n  }\n}\n")
	definition, diags := (Loader{}).ValidateTestContent(config.Options{WorkingDir: root}, path, candidate)
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 0 || definition == nil || definition.Tests["health"].File != resolvedPath || definition.Variables["BASE_URL"].Range.File != resolvedPath {
		t.Fatalf("definition=%#v diagnostics=%#v", definition, diags)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("prospective validation changed the existing file")
	}
}
