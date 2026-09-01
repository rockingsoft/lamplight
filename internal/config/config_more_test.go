package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, dir, source string) string {
	t.Helper()
	path := filepath.Join(dir, ConfigFilename)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolvePathSemantics(t *testing.T) {
	tests := []struct {
		name      string
		options   func(string) Options
		wantError string
	}{
		{name: "explicit relative", options: func(dir string) Options { return Options{WorkingDir: dir, ConfigPath: ConfigFilename} }},
		{name: "explicit absolute", options: func(dir string) Options {
			return Options{WorkingDir: dir, ConfigPath: filepath.Join(dir, ConfigFilename)}
		}},
		{name: "missing working directory", options: func(dir string) Options { return Options{WorkingDir: filepath.Join(dir, "missing")} }, wantError: "working directory"},
		{name: "missing config", options: func(dir string) Options {
			return Options{WorkingDir: dir, ConfigPath: filepath.Join(dir, "missing.hcl")}
		}, wantError: "configuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, `project { base_dir = "." }`)
			paths, diags := Resolve(tt.options(dir))
			if tt.wantError == "" {
				realDir, err := filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(diags) != 0 || paths.ConfigPath == "" || paths.ConfigDir != realDir {
					t.Fatalf("paths=%#v diagnostics=%#v", paths, diags)
				}
			} else if len(diags) == 0 || !strings.Contains(diags[0].Message, tt.wantError) {
				t.Fatalf("diagnostics=%#v", diags)
			}
		})
	}
}

func TestResolveSearchAndInvalidTargets(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, dir, `project { base_dir = "." }`)
	paths, diags := Resolve(Options{WorkingDir: nested})
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 0 || paths.ConfigPath != realPath {
		t.Fatalf("paths=%#v diagnostics=%#v", paths, diags)
	}
	if _, diags := Resolve(Options{WorkingDir: t.TempDir()}); len(diags) == 0 || !strings.Contains(diags[0].Message, "could not find") {
		t.Fatalf("diagnostics=%#v", diags)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, diags := Resolve(Options{WorkingDir: file}); len(diags) == 0 {
		t.Fatal("file working directory unexpectedly resolved")
	}
	if _, diags := Resolve(Options{WorkingDir: filepath.Dir(file), ConfigPath: filepath.Dir(file)}); len(diags) == 0 || !strings.Contains(diags[0].Message, "not a regular file") {
		t.Fatalf("diagnostics=%#v", diags)
	}
	link := filepath.Join(filepath.Dir(file), "linked.hcl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	paths, diags = Resolve(Options{WorkingDir: filepath.Dir(file), ConfigPath: link})
	if len(diags) != 0 || paths.ConfigPath != realPath {
		t.Fatalf("symlink paths=%#v diagnostics=%#v", paths, diags)
	}
}

func TestLoadConfigurationTable(t *testing.T) {
	tests := []struct {
		name, source       string
		wantNil, wantDiags bool
	}{
		{name: "full client", source: "project {\n  base_dir = \"tests\"\n  http_client {\n    timeout = duration(\"2s\")\n    follow_redirects = false\n    max_request_body_bytes = 123\n    max_response_body_bytes = 456\n    proxy = \"http://proxy.test\"\n    tls_skip_verify = true\n  }\n}\ndatasource \"tempo\" { endpoint = \"http://tempo.test\" }\n", wantDiags: true},
		{name: "legacy output", source: "project {\n  base_dir = \"tests\"\n  output = \"json\"\n}\n"},
		{name: "invalid legacy output", source: "project {\n  base_dir = \"tests\"\n  output = \"xml\"\n}\n", wantDiags: true},
		{name: "runtime base and proxy", source: "project {\n  base_dir = var.BASE\n  http_client {\n    proxy = var.PROXY\n  }\n}\n", wantDiags: true},
		{name: "invalid literals", source: "project {\n  base_dir = \".\"\n  http_client {\n    timeout = 0\n    follow_redirects = \"yes\"\n    max_request_body_bytes = 1.5\n    max_response_body_bytes = 0\n    proxy = 42\n    tls_skip_verify = \"yes\"\n  }\n}\n", wantDiags: true},
		{name: "parse error", source: `project { base_dir = `, wantNil: true, wantDiags: true},
		{name: "no project", source: "datasource \"tempo\" {\n  endpoint = \"http://tempo.test\"\n}\n", wantNil: true, wantDiags: true},
		{name: "duplicate blocks", source: "project {\n  base_dir = \".\"\n}\nproject {\n  base_dir = \".\"\n}\ndatasource \"tempo\" {\n  endpoint = \"one\"\n}\ndatasource \"tempo\" {\n  endpoint = \"two\"\n}\n", wantDiags: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "tests"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeConfig(t, dir, tt.source)
			got, diags := Load(Options{WorkingDir: dir})
			if (got == nil) != tt.wantNil || (len(diags) > 0) != tt.wantDiags {
				t.Fatalf("config=%#v diagnostics=%#v", got, diags)
			}
		})
	}
}

func TestLoadHTTPClientValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, dir, "project {\n  base_dir = \"tests\"\n  http_client {\n    timeout = duration(\"2s\")\n    follow_redirects = false\n    max_request_body_bytes = 123\n    max_response_body_bytes = 456\n    proxy = \"http://proxy.test\"\n    tls_skip_verify = true\n  }\n}\n")
	got, diags := Load(Options{WorkingDir: dir})
	if got == nil || len(diags) != 1 || diags[0].Severity != "warning" || got.HTTPClient.Timeout != 2*time.Second || got.HTTPClient.FollowRedirects || got.HTTPClient.MaxRequestBodyBytes != 123 || got.HTTPClient.MaxResponseBodyBytes != 456 || got.HTTPClient.Proxy != "http://proxy.test" || !got.HTTPClient.TLSSkipVerify {
		t.Fatalf("config=%#v diagnostics=%#v", got, diags)
	}
}
