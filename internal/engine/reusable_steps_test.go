package engine

import (
	"context"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/model"
)

type parameterHTTP struct{ requests []model.HTTPRequest }

func (h *parameterHTTP) Execute(_ context.Context, request model.HTTPRequest, _ model.HTTPClientConfig, _ *model.TestTraceContext) (model.Response, error) {
	h.requests = append(h.requests, request)
	return model.Response{StatusCode: 200, Body: "token", Headers: map[string][]string{}}, nil
}

func TestParameterizedStepFeedsLaterStep(t *testing.T) {
	login := model.StepDefinition{
		Name:          "auth",
		ParamDefaults: map[string]hcl.Expression{"email": parseExpr(t, `var.EMAIL`)},
		Params:        map[string]hcl.Expression{"email": parseExpr(t, `"other@example.test"`)},
		HTTP:          model.HTTPRequestDefinition{Method: parseExpr(t, `"POST"`), URL: parseExpr(t, `"http://example.test/login"`), Body: parseExpr(t, `params.email`)},
		Outputs:       map[string]hcl.Expression{"token": parseExpr(t, `response.body`)},
		Checks:        []model.CheckDefinition{{Name: "ok", Response: map[string]hcl.Expression{"status": parseExpr(t, `response.status_code == 200`)}}},
	}
	order := model.StepDefinition{Name: "order", HTTP: model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test/orders"`), Headers: map[string]hcl.Expression{"Authorization": parseExpr(t, `"Bearer ${steps.auth.outputs.token}"`)}}}
	def := &model.ProjectDefinition{HTTPClient: model.DefaultHTTPClientConfig()}
	project := &model.Project{Definition: def, Variables: map[string]model.SensitiveValue{"EMAIL": {Value: cty.StringVal("original@example.test")}}, Tests: []model.TestDefinition{{Name: "checkout", Steps: []model.StepDefinition{login, order}}}, HTTPClient: def.HTTPClient}
	http := &parameterHTTP{}
	result := (&Engine{HTTP: http}).Run(context.Background(), project)
	if result.Status != model.StatusPassed || len(http.requests) != 2 {
		t.Fatalf("run=%#v requests=%#v", result, http.requests)
	}
	if http.requests[0].Body != "other@example.test" || http.requests[1].Headers["Authorization"] != "Bearer token" {
		t.Fatalf("requests=%#v", http.requests)
	}
}
