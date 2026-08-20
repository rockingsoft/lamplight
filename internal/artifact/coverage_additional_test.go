package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lamplight/internal/model"
	"lamplight/internal/result"
)

func TestNewCoversDirectorySelectionAndValidation(t *testing.T) {
	store, err := New(Config{Redactor: result.NewRedactor("secret")})
	if err != nil || store.directory != os.TempDir() {
		t.Fatalf("default store = %#v, err = %v", store, err)
	}
	created := filepath.Join(t.TempDir(), "nested", "artifacts")
	store, err = New(Config{Directory: created})
	if err != nil {
		t.Fatal(err)
	}
	if store.directory != created {
		t.Fatalf("directory = %q, want %q", store.directory, created)
	}
	info, err := os.Stat(created)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != directoryMode {
		t.Fatalf("created directory mode = %o, want %o", info.Mode().Perm(), directoryMode)
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Directory: file}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file directory error = %v", err)
	}
	if _, err := NewStore(t.TempDir(), result.NewRedactor("secret"), result.NewRedactor("ignored")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRunAndFinalizeHonorContextAndReuseDirectory(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := failedRun()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.WriteRun(cancelled, run); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteRun error = %v", err)
	}
	if store.RunDirectory() != "" {
		t.Fatal("cancelled WriteRun created a run directory")
	}
	references, err := store.WriteRun(context.Background(), run)
	if err != nil || len(references) != 1 {
		t.Fatalf("WriteRun references = %#v, err = %v", references, err)
	}
	runDirectory := store.RunDirectory()
	if _, err := store.WriteRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if store.RunDirectory() != runDirectory {
		t.Fatalf("run directory changed: %q -> %q", runDirectory, store.RunDirectory())
	}
	cancelled, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := store.Finalize(cancelled, run, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("Finalize error = %v", err)
	}
	if got := (&Store{}).references(); got != nil {
		t.Fatalf("empty references = %#v", got)
	}
}

type countingContext struct {
	checks int
	err    error
}

func (c *countingContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *countingContext) Done() <-chan struct{}       { return nil }
func (c *countingContext) Err() error {
	c.checks++
	if c.checks >= 2 {
		return c.err
	}
	return nil
}
func (c *countingContext) Value(any) any { return nil }

func TestWriteSnapshotReportsEncodingAndMidWriteCancellation(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := failedRun()
	run.Selection = map[string]any{"not_json": func() {}}
	if _, err := store.WriteRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "encode metadata.json") {
		t.Fatalf("encoding error = %v", err)
	}
	store, err = NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := &countingContext{err: errors.New("stopped")}
	if _, err := store.WriteRun(ctx, failedRun()); !errors.Is(err, ctx.err) {
		t.Fatalf("mid-write error = %v", err)
	}
}

func TestAtomicWriteAndSafeNameErrors(t *testing.T) {
	if _, err := New(Config{Directory: "\x00"}); err == nil || !strings.Contains(err.Error(), "stat artifact directory") {
		t.Fatalf("invalid directory stat error = %v", err)
	}
	parentFile := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parentFile, []byte("x"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Directory: filepath.Join(parentFile, "child")}); err == nil || !strings.Contains(err.Error(), "stat artifact directory") {
		t.Fatalf("mkdir error = %v", err)
	}
	readonly := t.TempDir()
	if err := os.Chmod(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Directory: filepath.Join(readonly, "child")}); err == nil || !strings.Contains(err.Error(), "create artifact directory") {
		t.Fatalf("readonly mkdir error = %v", err)
	}
	if err := os.Chmod(readonly, directoryMode); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing", "artifact.json")
	if err := atomicWrite(missing, []byte("x")); err == nil || !strings.Contains(err.Error(), "create temporary artifact") {
		t.Fatalf("missing parent error = %v", err)
	}
	directory := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.Mkdir(directory, directoryMode); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(directory, []byte("x")); err == nil || !strings.Contains(err.Error(), "commit artifact") {
		t.Fatalf("directory target error = %v", err)
	}
	if got := safeName("a/b c?世界"); got != "abc" {
		t.Fatalf("safeName = %q", got)
	}
	if got := safeName(strings.Repeat("x", 60)); len(got) != 48 {
		t.Fatalf("safeName length = %d", len(got))
	}
	if got := safeName(""); got != "" {
		t.Fatalf("empty safeName = %q", got)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := store.directory
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteRun(context.Background(), failedRun()); err == nil || !strings.Contains(err.Error(), "create run artifact directory") {
		t.Fatalf("run directory error = %v", err)
	}
}

func TestFinalizeReportsSnapshotEncodingErrors(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := failedRun()
	run.Selection = map[string]any{"function": func() {}}
	if _, err := store.Finalize(context.Background(), run, true); err == nil || !strings.Contains(err.Error(), "encode metadata.json") {
		t.Fatalf("Finalize encoding error = %v", err)
	}
}

func TestFinalizeReportsSuccessfulArtifactRemovalErrors(t *testing.T) {
	parent := t.TempDir()
	runDirectory := filepath.Join(parent, "run")
	if err := os.Mkdir(runDirectory, directoryMode); err != nil {
		t.Fatal(err)
	}
	store := &Store{directory: parent, runDir: runDirectory}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	run := model.RunResult{SchemaVersion: 1, RunID: "passed", Status: model.StatusPassed}
	_, err := store.Finalize(context.Background(), run, false)
	if err == nil || !strings.Contains(err.Error(), "remove successful run artifacts") {
		t.Fatalf("removal error = %v", err)
	}
	if err := os.Chmod(parent, directoryMode); err != nil {
		t.Fatal(err)
	}
}

var _ context.Context = (*countingContext)(nil)
