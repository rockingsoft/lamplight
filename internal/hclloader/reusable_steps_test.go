package hclloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lamplight/internal/config"
)

func reusableProject(t *testing.T, source string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "tests")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), []byte("project { base_dir = \"tests\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "steps.wick"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	_, diags := (Loader{}).LoadProject(config.Options{WorkingDir: dir})
	messages := make([]string, len(diags))
	for i, d := range diags {
		messages[i] = d.Message
	}
	return dir, messages
}

func TestReusableStepExpandsWithParamsAndLocalOutputs(t *testing.T) {
	source := `variable "BASE_URL" { default = "http://example.test" }
variable "EMAIL" { default = "one@example.test" }
test "auth" {
 step "login" {
  params = { email = var.EMAIL }
  http_request {
   method = "POST"
   url = "${var.BASE_URL}/login"
   body = params.email
  }
  outputs = { token = response.body }
 }
}
test "checkout" {
 step "authenticate" {
  ref = auth.steps.login
  params = { email = "two@example.test" }
 }
 step "order" {
  http_request {
   method = "GET"
   url = "${var.BASE_URL}/orders/${steps.authenticate.outputs.token}"
  }
 }
}`
	dir, messages := reusableProject(t, source)
	if len(messages) != 0 {
		t.Fatal(messages)
	}
	project, _ := (Loader{}).LoadProject(config.Options{WorkingDir: dir})
	step := project.Tests["checkout"].Steps[0]
	if step.Name != "authenticate" || step.Ref != "auth.login" || step.HTTP.Method == nil || len(step.Checks) != 0 || len(step.ParamDefaults) != 1 || len(step.Params) != 1 {
		t.Fatalf("expanded step: %#v", step)
	}
}

func TestReusableStepRejectsInvalidReferences(t *testing.T) {
	cases := []struct{ name, invocation, want string }{
		{"missing test", `ref = missing.steps.login`, `test "missing" does not exist`},
		{"missing step", `ref = auth.steps.missing`, `step "missing" does not exist`},
		{"unknown param", `ref = auth.steps.login
 params = { other = "x" }`, `parameter "other" is not declared`},
		{"malformed", `ref = auth.login`, `ref must be TEST.steps.STEP`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := `test "auth" {
 step "login" {
  params = { email = "a" }
  http_request {
   method = "GET"
   url = "http://example.test"
  }
 }
}
test "checkout" {
 step "auth" {
 ` + tc.invocation + `
 }
}`
			_, messages := reusableProject(t, source)
			if !strings.Contains(strings.Join(messages, "\n"), tc.want) {
				t.Fatalf("diagnostics = %v, want %q", messages, tc.want)
			}
		})
	}
}
