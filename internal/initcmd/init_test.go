package initcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	if err := Run(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".lamplight", filepath.Join("lamplight", "healthcheck.wick")} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Run(dir); err == nil {
		t.Fatal("expected overwrite error")
	}
}

func TestRunRefusesAnExistingTestFile(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "lamplight")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "healthcheck.wick"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(dir); err == nil {
		t.Fatal("expected existing test file error")
	}
}

func TestRunReturnsAnErrorWhenDirectoryIsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(path); err == nil {
		t.Fatal("expected an error for a file used as a directory")
	}
}
