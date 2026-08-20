// Package mcpserver exposes Lamplight project operations to coding agents.
package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"lamplight/internal/config"
	"lamplight/internal/discovery"
	"lamplight/internal/hclloader"
	"lamplight/internal/model"
)

const version = "0.1.0"

type Options struct {
	ConfigPath string
	WorkingDir string
	RunCLI     func(context.Context, []string) (exitCode int, stdout, stderr []byte)
}

type service struct {
	options Options
	mu      sync.Mutex
}

// New returns a stdio-ready MCP server. The caller owns the transport.
func New(options Options) *mcp.Server {
	svc := &service{options: options}
	server := mcp.NewServer(&mcp.Implementation{Name: "lamplight", Version: version}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lamplight_list_tests", Description: "List Lamplight tests, tags, source files, and whether they require a tracing datasource."}, svc.listTests)
	mcp.AddTool(server, &mcp.Tool{Name: "lamplight_read_test_file", Description: "Read one .wick definition file from the configured Lamplight project."}, svc.readFile)
	mcp.AddTool(server, &mcp.Tool{Name: "lamplight_write_test_file", Description: "Create or replace one .wick definition file. Replacements require the SHA-256 returned by read/list, and invalid project changes are rolled back."}, svc.writeFile)
	mcp.AddTool(server, &mcp.Tool{Name: "lamplight_delete_test_file", Description: "Delete one .wick definition file using an expected SHA-256 precondition. The deletion is rolled back if it makes the project invalid."}, svc.deleteFile)
	mcp.AddTool(server, &mcp.Tool{Name: "lamplight_format_test_file", Description: "Format one .wick file using the canonical HCL formatter. A replacement requires the current SHA-256."}, svc.formatFile)
	mcp.AddTool(server, &mcp.Tool{Name: "lamplight_lint_project", Description: "Validate the complete project and report .wick files that are not canonically formatted. Does not execute tests or modify files."}, svc.lint)
	mcp.AddTool(server, &mcp.Tool{Name: "lamplight_run_tests", Description: "Run all tests, one named test, or one tag and return the structured Lamplight JSON result."}, svc.run)
	return server
}

type emptyInput struct{}

type testSummary struct {
	Name               string   `json:"name"`
	Tags               []string `json:"tags"`
	File               string   `json:"file"`
	SHA256             string   `json:"sha256"`
	RequiresDatasource bool     `json:"requires_datasource"`
}

type listOutput struct {
	Tests       []testSummary      `json:"tests"`
	Diagnostics []model.Diagnostic `json:"diagnostics,omitempty"`
}

func (s *service) listTests(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, listOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, diags := s.load()
	out := listOutput{Diagnostics: diags}
	if hasErrors(diags) || def == nil {
		return toolError("project validation failed", out), out, nil
	}
	for _, test := range def.Tests {
		data, err := os.ReadFile(test.File)
		if err != nil {
			return nil, out, err
		}
		out.Tests = append(out.Tests, testSummary{Name: test.Name, Tags: test.Tags, File: relative(def.BaseDir, test.File), SHA256: digest(data), RequiresDatasource: needsDatasource(test)})
	}
	sort.Slice(out.Tests, func(i, j int) bool { return out.Tests[i].Name < out.Tests[j].Name })
	return nil, out, nil
}

type pathInput struct {
	Path string `json:"path" jsonschema:"relative .wick path below project.base_dir"`
}

type fileOutput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

func (s *service) readFile(_ context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, fileOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, base, err := s.safePath(in.Path)
	if err != nil {
		return toolError(err.Error(), fileOutput{}), fileOutput{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return toolError(err.Error(), fileOutput{}), fileOutput{}, nil
	}
	out := fileOutput{Path: relative(base, path), Content: string(data), SHA256: digest(data)}
	return nil, out, nil
}

type writeInput struct {
	Path           string `json:"path" jsonschema:"relative .wick path below project.base_dir"`
	Content        string `json:"content" jsonschema:"complete Lamplight HCL file content"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"required when replacing an existing file; use the value returned by read or list"`
}

type mutationOutput struct {
	Path        string             `json:"path"`
	SHA256      string             `json:"sha256,omitempty"`
	Changed     bool               `json:"changed"`
	Diagnostics []model.Diagnostic `json:"diagnostics,omitempty"`
}

func (s *service) writeFile(_ context.Context, _ *mcp.CallToolRequest, in writeInput) (*mcp.CallToolResult, mutationOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, base, err := s.safePath(in.Path)
	if err != nil {
		return toolError(err.Error(), mutationOutput{}), mutationOutput{}, nil
	}
	old, readErr := os.ReadFile(path)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, mutationOutput{}, readErr
	}
	if exists && (in.ExpectedSHA256 == "" || in.ExpectedSHA256 != digest(old)) {
		out := mutationOutput{Path: relative(base, path), SHA256: digest(old)}
		return toolError("expected_sha256 is missing or does not match the current file", out), out, nil
	}
	if !exists && in.ExpectedSHA256 != "" {
		return toolError("expected_sha256 must be empty when creating a file", mutationOutput{}), mutationOutput{}, nil
	}
	formatted := hclwrite.Format([]byte(in.Content))
	if bytes.Equal(old, formatted) {
		return nil, mutationOutput{Path: relative(base, path), SHA256: digest(old), Changed: false}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, mutationOutput{}, err
	}
	if err := atomicWrite(path, formatted, modeOrDefault(path)); err != nil {
		return nil, mutationOutput{}, err
	}
	_, diags := s.load()
	if hasErrors(diags) {
		if exists {
			_ = atomicWrite(path, old, modeOrDefault(path))
		} else {
			_ = os.Remove(path)
		}
		out := mutationOutput{Path: relative(base, path), Diagnostics: diags}
		return toolError("change was rolled back because project validation failed", out), out, nil
	}
	return nil, mutationOutput{Path: relative(base, path), SHA256: digest(formatted), Changed: true, Diagnostics: diags}, nil
}

type deleteInput struct {
	Path           string `json:"path" jsonschema:"relative .wick path below project.base_dir"`
	ExpectedSHA256 string `json:"expected_sha256" jsonschema:"current SHA-256 returned by read or list"`
}

func (s *service) deleteFile(_ context.Context, _ *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, mutationOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, base, err := s.safePath(in.Path)
	if err != nil {
		return toolError(err.Error(), mutationOutput{}), mutationOutput{}, nil
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return toolError(err.Error(), mutationOutput{}), mutationOutput{}, nil
	}
	if in.ExpectedSHA256 == "" || in.ExpectedSHA256 != digest(old) {
		out := mutationOutput{Path: relative(base, path), SHA256: digest(old)}
		return toolError("expected_sha256 is missing or does not match the current file", out), out, nil
	}
	mode := modeOrDefault(path)
	if err := os.Remove(path); err != nil {
		return nil, mutationOutput{}, err
	}
	_, diags := s.load()
	if hasErrors(diags) {
		_ = atomicWrite(path, old, mode)
		out := mutationOutput{Path: relative(base, path), SHA256: digest(old), Diagnostics: diags}
		return toolError("deletion was rolled back because project validation failed", out), out, nil
	}
	return nil, mutationOutput{Path: relative(base, path), Changed: true, Diagnostics: diags}, nil
}

func (s *service) formatFile(ctx context.Context, req *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, mutationOutput, error) {
	s.mu.Lock()
	path, _, err := s.safePath(in.Path)
	if err != nil {
		s.mu.Unlock()
		return toolError(err.Error(), mutationOutput{}), mutationOutput{}, nil
	}
	data, err := os.ReadFile(path)
	s.mu.Unlock()
	if err != nil {
		return toolError(err.Error(), mutationOutput{}), mutationOutput{}, nil
	}
	return s.writeFile(ctx, req, writeInput{Path: in.Path, Content: string(hclwrite.Format(data)), ExpectedSHA256: in.ExpectedSHA256})
}

type lintOutput struct {
	Valid            bool               `json:"valid"`
	Files            int                `json:"files"`
	UnformattedFiles []string           `json:"unformatted_files,omitempty"`
	Diagnostics      []model.Diagnostic `json:"diagnostics,omitempty"`
}

func (s *service) lint(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, lintOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, diags := s.load()
	out := lintOutput{Valid: !hasErrors(diags), Diagnostics: diags}
	if def == nil {
		return toolError("project validation failed", out), out, nil
	}
	files, err := discovery.Discover(def.BaseDir)
	if err != nil {
		return nil, out, err
	}
	out.Files = len(files)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, out, err
		}
		if !bytes.Equal(data, hclwrite.Format(data)) {
			out.UnformattedFiles = append(out.UnformattedFiles, relative(def.BaseDir, path))
		}
	}
	if !out.Valid {
		return toolError("project validation failed", out), out, nil
	}
	return nil, out, nil
}

type runInput struct {
	Test      string            `json:"test,omitempty" jsonschema:"one exact test name; omit to run all tests"`
	Tag       string            `json:"tag,omitempty" jsonschema:"select tests by tag; mutually exclusive with test"`
	Variables map[string]string `json:"variables,omitempty" jsonschema:"runtime variable values; prefer MCP process environment for secrets"`
}

type runOutput struct {
	ExitCode int    `json:"exit_code"`
	Result   any    `json:"result,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

func (s *service) run(ctx context.Context, _ *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, runOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Test != "" && in.Tag != "" {
		return toolError("test and tag are mutually exclusive", runOutput{}), runOutput{}, nil
	}
	args := []string{"run", "--output", "json"}
	if s.options.ConfigPath != "" {
		args = append(args, "--config", s.options.ConfigPath)
	}
	if s.options.WorkingDir != "" {
		args = append(args, "--working-dir", s.options.WorkingDir)
	}
	if in.Tag != "" {
		args = append(args, "--tag", in.Tag)
	}
	for _, name := range sortedKeys(in.Variables) {
		args = append(args, "--var", name+"="+in.Variables[name])
	}
	if in.Test != "" {
		args = append(args, in.Test)
	}
	if s.options.RunCLI == nil {
		return nil, runOutput{}, fmt.Errorf("MCP server has no CLI runner configured")
	}
	exitCode, stdout, stderr := s.options.RunCLI(ctx, args)
	out := runOutput{ExitCode: exitCode, Stderr: string(stderr)}
	if json.Valid(stdout) {
		_ = json.Unmarshal(stdout, &out.Result)
	}
	if exitCode == 1 {
		return toolError("Lamplight run ended with a technical error", out), out, nil
	}
	return nil, out, nil
}

func (s *service) load() (*model.ProjectDefinition, []model.Diagnostic) {
	return hclloader.Loader{}.LoadProject(config.Options{ConfigPath: s.options.ConfigPath, WorkingDir: s.options.WorkingDir})
}

func (s *service) safePath(input string) (string, string, error) {
	if input == "" || filepath.IsAbs(input) || filepath.Ext(input) != discovery.DefinitionSuffix {
		return "", "", fmt.Errorf("path must be a relative %s file", discovery.DefinitionSuffix)
	}
	root, diags := config.Load(config.Options{ConfigPath: s.options.ConfigPath, WorkingDir: s.options.WorkingDir})
	if root == nil || hasErrors(diags) {
		return "", "", fmt.Errorf("cannot resolve project.base_dir while the project is invalid")
	}
	base, err := filepath.EvalSymlinks(root.BaseDir)
	if err != nil {
		return "", "", err
	}
	path := filepath.Clean(filepath.Join(base, input))
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes project.base_dir")
	}
	for current := filepath.Dir(path); current != base; current = filepath.Dir(current) {
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("path traverses a symbolic link")
		}
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("symbolic-link files are not allowed")
	}
	return path, base, nil
}

func toolError(message string, value any) *mcp.CallToolResult {
	data, _ := json.Marshal(value)
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message + "\n" + string(data)}}}
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lamplight-mcp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode.Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func modeOrDefault(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode()
	}
	return 0o644
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func relative(base, path string) string {
	rel, _ := filepath.Rel(base, path)
	return filepath.ToSlash(rel)
}
func hasErrors(diags []model.Diagnostic) bool {
	for _, diag := range diags {
		if diag.Severity == "error" {
			return true
		}
	}
	return false
}
func needsDatasource(test model.TestDefinition) bool {
	for _, step := range test.Steps {
		for _, check := range step.Checks {
			if check.Spans != nil {
				return true
			}
		}
	}
	return false
}
func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
