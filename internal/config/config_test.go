package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesBaseDirAndDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFilename), []byte("project { base_dir = \"tests\" }"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, diags := Load(Options{WorkingDir: filepath.Join(dir, "tests")})
	if len(diags) != 0 {
		t.Fatalf("Load diagnostics: %#v", diags)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if config.BaseDir != filepath.Join(realDir, "tests") || config.HTTPClient.Timeout.Seconds() != 30 {
		t.Fatalf("unexpected config: %#v", config)
	}
}
