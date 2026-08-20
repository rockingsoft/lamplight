package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lamplight/internal/model"
	"lamplight/internal/result"
)

func TestStorePersistsRedactedRestrictiveArtifacts(t *testing.T) {
	store, err := NewStore(t.TempDir(), result.NewRedactor("top-secret"))
	if err != nil {
		t.Fatal(err)
	}
	run := failedRun()
	references, err := store.Finalize(context.Background(), run, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].Kind != "run" {
		t.Fatalf("references = %#v", references)
	}
	directory := references[0].Path
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != directoryMode {
		t.Fatalf("directory mode = %o, want %o", info.Mode().Perm(), directoryMode)
	}
	for _, name := range []string{"metadata.json", "step-results.json", "checks.json", "result.json"} {
		path := filepath.Join(directory, name)
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), "top-secret") {
			t.Fatalf("%s leaked secret: %s", name, contents)
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fileInfo.Mode().Perm() != fileMode {
			t.Fatalf("%s mode = %o, want %o", name, fileInfo.Mode().Perm(), fileMode)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary artifact remains: %s", entry.Name())
		}
	}
}

func TestStoreCleansSuccessfulRunUnlessKept(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := failedRun()
	run.Status = model.StatusPassed
	run.Tests[0].Status = model.StatusPassed
	run.Summary = model.RunSummary{TestsTotal: 1, TestsPassed: 1}
	references, err := store.Finalize(context.Background(), run, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 0 || store.RunDirectory() != "" {
		t.Fatalf("successful artifacts should be removed: %#v / %q", references, store.RunDirectory())
	}
	kept, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	references, err = kept.Finalize(context.Background(), run, true)
	if err != nil || len(references) != 1 {
		t.Fatalf("kept references = %#v, err = %v", references, err)
	}
}

func failedRun() model.RunResult {
	return model.RunResult{
		SchemaVersion: 1, RunID: "run/unsafe", Status: model.StatusFailed,
		StartedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		Summary:   model.RunSummary{TestsTotal: 1, TestsFailed: 1},
		Tests: []model.TestResult{{
			Name: "login", Tags: []string{"smoke"}, File: "login.hcl", Status: model.StatusFailed,
			Steps: []model.StepResult{{
				Name: "call", ExecutionID: "step-1", Status: model.StatusFailed,
				Request: &model.HTTPRequest{Headers: map[string]string{"authorization": "Bearer top-secret"}, Body: `{"password":"top-secret"}`},
				Checks:  []model.CheckResult{{Name: "status", Status: model.StatusFailed, Reason: "top-secret"}},
			}},
		}},
	}
}
