package hclloader

import (
	"path/filepath"
	"testing"

	"tracetest/internal/config"
)

func TestLoadProjectParsesAndValidatesFixture(t *testing.T) {
	root := filepath.Join("testdata", "project")
	project, diagnostics := (Loader{}).LoadProject(config.Options{WorkingDir: root})
	if len(diagnostics) != 0 {
		t.Fatalf("LoadProject diagnostics: %#v", diagnostics)
	}
	if project.Output != "json" || project.Datasource == nil || len(project.Variables) != 2 || len(project.Tests) != 1 {
		t.Fatalf("unexpected project: %#v", project)
	}
	test := project.Tests["checkout"]
	if len(test.Steps) != 2 || test.Steps[1].Name != "order" {
		t.Fatalf("unexpected steps: %#v", test.Steps)
	}
}
