package engine

import (
	"context"
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
