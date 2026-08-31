package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/model"
)

type fakeHTTP struct {
	response model.Response
	err      error
}

func TestPrepareK6ScriptResolvesInsideProject(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "load.js")
	if err := os.WriteFile(script, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{"script": "load.js"}}
	if err := prepareTriggerRequest(&request, &model.ProjectDefinition{BaseDir: directory}); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(script)
	if err != nil {
		t.Fatal(err)
	}
	if request.Attributes["script"] != resolved {
		t.Fatalf("script=%q", request.Attributes["script"])
	}
}

func TestPrepareK6ScriptRejectsEscape(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "tests")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside.js"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{"script": "../outside.js"}}
	err := prepareTriggerRequest(&request, &model.ProjectDefinition{BaseDir: base})
	if err == nil || !strings.Contains(err.Error(), "escapes project.base_dir") {
		t.Fatalf("err=%v", err)
	}
}

func TestPrepareK6CloudRunFilesResolveInsideProject(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "load.js")
	helper := filepath.Join(directory, "lib.js")
	for _, path := range []string{script, helper} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	request := model.TriggerRequest{Kind: model.TriggerK6, Attributes: map[string]any{"script": "load.js", "files": []any{"lib.js"}, "executor": map[string]any{"kind": "cloud_run"}}}
	if err := prepareTriggerRequest(&request, &model.ProjectDefinition{BaseDir: directory}); err != nil {
		t.Fatal(err)
	}
	files := request.Attributes["files"].([]any)
	resolvedHelper, err := filepath.EvalSymlinks(helper)
	if err != nil {
		t.Fatal(err)
	}
	if request.Attributes["bundle_root"] == "" || len(files) != 1 || files[0] != resolvedHelper {
		t.Fatalf("attributes=%#v", request.Attributes)
	}
}

func (f fakeHTTP) Execute(context.Context, model.HTTPRequest, model.HTTPClientConfig, *model.TestTraceContext) (model.Response, error) {
	return f.response, f.err
}

func parseExpr(t *testing.T, source string) hcl.Expression {
	t.Helper()
	e, d := hclsyntax.ParseExpression([]byte(source), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if d.HasErrors() {
		t.Fatal(d.Error())
	}
	return e
}

func TestRunResponseCheck(t *testing.T) {
	step := model.StepDefinition{Name: "get", HTTP: model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test"`)}, Checks: []model.CheckDefinition{{Name: "ok", Response: map[string]hcl.Expression{"status": parseExpr(t, "response.status_code == 200")}}}}
	def := &model.ProjectDefinition{Tests: map[string]model.TestDefinition{}, HTTPClient: model.DefaultHTTPClientConfig()}
	project := &model.Project{Definition: def, Variables: map[string]model.SensitiveValue{"X": {Value: cty.StringVal("x")}}, Tests: []model.TestDefinition{{Name: "health", Steps: []model.StepDefinition{step}}}, HTTPClient: def.HTTPClient}
	run := (&Engine{HTTP: fakeHTTP{response: model.Response{StatusCode: 200, Headers: map[string][]string{}, Body: "ok"}}}).Run(context.Background(), project)
	if run.Status != model.StatusPassed || run.Summary.TestsPassed != 1 {
		t.Fatalf("unexpected run %#v", run)
	}
}

func TestRunCheckFailure(t *testing.T) {
	step := model.StepDefinition{Name: "get", HTTP: model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test"`)}, Checks: []model.CheckDefinition{{Name: "ok", Response: map[string]hcl.Expression{"status": parseExpr(t, "response.status_code == 200")}}}}
	def := &model.ProjectDefinition{HTTPClient: model.DefaultHTTPClientConfig()}
	project := &model.Project{Definition: def, Variables: map[string]model.SensitiveValue{}, Tests: []model.TestDefinition{{Name: "health", Steps: []model.StepDefinition{step}}}, HTTPClient: def.HTTPClient}
	run := (&Engine{HTTP: fakeHTTP{response: model.Response{StatusCode: 500, Headers: map[string][]string{}, Body: "bad"}}}).Run(context.Background(), project)
	if run.Status != model.StatusFailed {
		t.Fatalf("expected failed, got %s", run.Status)
	}
}
