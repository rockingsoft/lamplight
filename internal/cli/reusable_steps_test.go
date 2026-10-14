package cli

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"lamplight/internal/model"
	"lamplight/internal/runtimevars"
)

func TestOverriddenStepParamDoesNotRequireDefaultVariable(t *testing.T) {
	parse := func(source string) hcl.Expression {
		expression, diags := hclsyntax.ParseExpression([]byte(source), "test.wick", hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
		return expression
	}
	step := model.StepDefinition{
		ParamDefaults: map[string]hcl.Expression{"email": parse("var.SOURCE_EMAIL")},
		Params:        map[string]hcl.Expression{"email": parse("var.CALLER_EMAIL")},
	}
	expressions := collectExpressions(&model.ProjectDefinition{}, []model.TestDefinition{{Steps: []model.StepDefinition{step}}})
	required := runtimevars.Required(expressions...)
	if len(required) != 1 || required[0] != "CALLER_EMAIL" {
		t.Fatalf("required variables = %v", required)
	}
}
