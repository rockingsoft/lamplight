package hclloader

import (
	"os"
	"path/filepath"
	"testing"

	"lamplight/internal/config"
)

func TestLoadTargetsAndRootVariables(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := `project {
  base_dir = "tests"
  default_target = "compose"
}
variable "BASE_URL" { type = string }
variable "WAIT" { type = duration }
target "compose" {
  runtime = "docker_compose"
  variables = { BASE_URL = "http://api:8080", WAIT = duration("2s") }
  docker_compose {
    project = "shop"
    services = ["api", "tempo"]
  }
}
target "cluster" {
  runtime = "kubernetes"
  kubernetes {
    context = "prod"
    namespace = "shop"
    service_account = "runner"
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	project, diags := (Loader{}).LoadProject(config.Options{WorkingDir: dir})
	if len(diags) != 0 {
		t.Fatalf("diagnostics: %#v", diags)
	}
	if project.DefaultTarget != "compose" || len(project.Targets) != 2 || len(project.Variables) != 2 {
		t.Fatalf("project: %#v", project)
	}
	if got := project.Targets["compose"]; got.Runtime != "docker_compose" || got.Compose.Project != "shop" || len(got.Compose.Services) != 2 {
		t.Fatalf("compose target: %#v", got)
	}
	if got := project.Targets["cluster"].Kubernetes; got.Context != "prod" || got.Namespace != "shop" || got.ServiceAccount != "runner" {
		t.Fatalf("kubernetes target: %#v", got)
	}
}

func TestLoadRejectsInvalidDefaultAndTargetVariables(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := `project {
  base_dir = "tests"
  default_target = "missing"
}
target "bad" {
  runtime = "docker_compose"
  variables = { UNKNOWN = "x" }
}
`
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	_, diags := (Loader{}).LoadProject(config.Options{WorkingDir: dir})
	if len(diags) < 2 {
		t.Fatalf("expected diagnostics, got %#v", diags)
	}
}
