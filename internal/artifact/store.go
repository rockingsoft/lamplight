// Package artifact persists redacted run snapshots using restrictive,
// per-run directories.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"tracetest/internal/model"
	"tracetest/internal/result"
)

const (
	directoryMode os.FileMode = 0o700
	fileMode      os.FileMode = 0o600
)

// Config configures a local artifact store. Directory defaults to the OS
// temporary directory. All files written by the store are redacted first.
type Config struct {
	Directory string
	Redactor  result.Redactor
}

// Store writes exactly one isolated run directory. It is safe for sequential
// calls from the engine and rejects concurrent mutation with a mutex.
type Store struct {
	directory string
	redactor  result.Redactor

	mu     sync.Mutex
	runDir string
}

// New creates a store from its explicit configuration.
func New(config Config) (*Store, error) {
	directory := config.Directory
	if directory == "" {
		directory = os.TempDir()
	}
	info, err := os.Stat(directory)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat artifact directory: %w", err)
		}
		if err := os.MkdirAll(directory, directoryMode); err != nil {
			return nil, fmt.Errorf("create artifact directory: %w", err)
		}
	} else if !info.IsDir() {
		return nil, fmt.Errorf("artifact directory %q is not a directory", directory)
	}
	return &Store{directory: directory, redactor: config.Redactor}, nil
}

// NewStore is a convenient constructor for the common artifacts-dir case.
func NewStore(directory string, redactors ...result.Redactor) (*Store, error) {
	config := Config{Directory: directory}
	if len(redactors) > 0 {
		config.Redactor = redactors[0]
	}
	return New(config)
}

// RunDirectory returns the active run directory, or an empty string after a
// successful run is cleaned up.
func (s *Store) RunDirectory() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runDir
}

// WriteRun writes a complete, redacted snapshot atomically. The returned
// reference points at the run directory so callers do not need to know the
// internal file layout.
func (s *Store) WriteRun(ctx context.Context, run model.RunResult) ([]model.ArtifactReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.ensureRunDirectory(run.RunID); err != nil {
		return nil, err
	}
	if err := s.writeSnapshot(ctx, run); err != nil {
		return nil, err
	}
	return s.references(), nil
}

// Finalize persists the final snapshot and enforces the retention policy.
// keep preserves successful runs; failed, errored, and cancelled runs are
// always retained. The boolean is intentionally the CLI --keep-artifacts
// value required by model.ArtifactStore.
func (s *Store) Finalize(ctx context.Context, run model.RunResult, keep bool) ([]model.ArtifactReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.ensureRunDirectory(run.RunID); err != nil {
		return nil, err
	}
	if err := s.writeSnapshot(ctx, run); err != nil {
		return nil, err
	}
	if keep || run.Status != model.StatusPassed {
		return s.references(), nil
	}
	directory := s.runDir
	if err := os.RemoveAll(directory); err != nil {
		return nil, fmt.Errorf("remove successful run artifacts: %w", err)
	}
	s.runDir = ""
	return nil, nil
}

func (s *Store) ensureRunDirectory(runID string) error {
	if s.runDir != "" {
		return nil
	}
	prefix := "tracetest-run-"
	if safe := safeName(runID); safe != "" {
		prefix += safe + "-"
	}
	directory, err := os.MkdirTemp(s.directory, prefix)
	if err != nil {
		return fmt.Errorf("create run artifact directory: %w", err)
	}
	if err := os.Chmod(directory, directoryMode); err != nil {
		_ = os.RemoveAll(directory)
		return fmt.Errorf("restrict run artifact directory: %w", err)
	}
	s.runDir = directory
	return nil
}

func (s *Store) writeSnapshot(ctx context.Context, run model.RunResult) error {
	metadata := struct {
		SchemaVersion int              `json:"schema_version"`
		RunID         string           `json:"run_id"`
		Status        model.Status     `json:"status"`
		StartedAt     string           `json:"started_at"`
		DurationMS    int64            `json:"duration_ms"`
		Selection     map[string]any   `json:"selection,omitempty"`
		Summary       model.RunSummary `json:"summary"`
	}{
		SchemaVersion: run.SchemaVersion,
		RunID:         run.RunID,
		Status:        run.Status,
		StartedAt:     run.StartedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		DurationMS:    run.DurationMS,
		Selection:     run.Selection,
		Summary:       run.Summary,
	}
	checks := collectChecks(run)
	files := []struct {
		name string
		data any
		v1   bool
	}{
		{name: "metadata.json", data: metadata},
		{name: "step-results.json", data: run.Tests},
		{name: "checks.json", data: checks},
		{name: "result.json", data: run, v1: true},
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		var (
			encoded []byte
			err     error
		)
		if file.v1 {
			encoded, err = s.redactor.MarshalJSONV1(run, "  ")
		} else {
			encoded, err = s.redactor.Marshal(file.data, "  ")
		}
		if err != nil {
			return fmt.Errorf("encode %s: %w", file.name, err)
		}
		encoded = append(encoded, '\n')
		if err := atomicWrite(filepath.Join(s.runDir, file.name), encoded); err != nil {
			return err
		}
	}
	return nil
}

type checkSnapshot struct {
	Test   string            `json:"test"`
	Step   string            `json:"step"`
	Result model.CheckResult `json:"result"`
}

func collectChecks(run model.RunResult) []checkSnapshot {
	var checks []checkSnapshot
	for _, test := range run.Tests {
		for _, step := range test.Steps {
			for _, check := range step.Checks {
				checks = append(checks, checkSnapshot{Test: test.Name, Step: step.Name, Result: check})
			}
		}
	}
	return checks
}

func (s *Store) references() []model.ArtifactReference {
	if s.runDir == "" {
		return nil
	}
	return []model.ArtifactReference{{Kind: "run", Path: s.runDir}}
}

func atomicWrite(target string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary artifact %s: %w", filepath.Base(target), err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(fileMode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("restrict temporary artifact %s: %w", filepath.Base(target), err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write artifact %s: %w", filepath.Base(target), err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync artifact %s: %w", filepath.Base(target), err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close artifact %s: %w", filepath.Base(target), err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("commit artifact %s: %w", filepath.Base(target), err)
	}
	return os.Chmod(target, fileMode)
}

func safeName(value string) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, value)
	if len(value) > 48 {
		return value[:48]
	}
	return value
}

var _ model.ArtifactStore = (*Store)(nil)
