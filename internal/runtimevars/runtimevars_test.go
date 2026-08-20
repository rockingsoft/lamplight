package runtimevars

import (
	"math/big"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"tracetest/internal/model"
)

func TestResolvePrecedenceAndSelectedClosure(t *testing.T) {
	expression, diags := hclsyntax.ParseExpression([]byte("var.PORT"), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	definitions := map[string]model.VariableDefinition{
		"PORT":  {Name: "PORT", Type: "int", Default: cty.NumberIntVal(80), HasDefault: true},
		"OTHER": {Name: "OTHER", Type: "string"},
	}
	values, diagnostics := Resolve(definitions, Input{Vars: map[string]string{"PORT": "8080"}, Environment: map[string]string{"TRACETEST_VAR_PORT": "9090"}}, expression)
	if len(diagnostics) != 0 {
		t.Fatalf("Resolve diagnostics: %#v", diagnostics)
	}
	port, _ := values["PORT"].Value.AsBigFloat().Int64()
	if port != 8080 {
		t.Fatalf("PORT = %d, want 8080", port)
	}
	if _, found := values["OTHER"]; found {
		t.Fatal("unreferenced required variable was resolved")
	}
}

func TestResolveSourcesErrorsAndDefaults(t *testing.T) {
	expression, diags := hclsyntax.ParseExpression([]byte("var.NAME + var.MISSING"), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	definitions := map[string]model.VariableDefinition{
		"NAME":    {Name: "NAME", Type: "string", Sensitive: true, Range: model.SourceRange{File: "vars.hcl", StartLine: 2, StartColumn: 3, EndLine: 2, EndColumn: 8}},
		"MISSING": {Name: "MISSING", Type: "int", Range: model.SourceRange{File: "vars.hcl", StartLine: 3, StartColumn: 3, EndLine: 3, EndColumn: 9}},
		"COUNT":   {Name: "COUNT", Type: "int", Default: cty.NumberIntVal(2), HasDefault: true},
		"WAIT":    {Name: "WAIT", Type: "duration", Default: cty.NumberIntVal(int64(time.Second)), HasDefault: true},
	}
	values, diagnostics := Resolve(definitions, Input{Vars: map[string]string{"NAME": "from-cli", "UNDEFINED": "x"}, Environment: map[string]string{"TRACETEST_VAR_MISSING": "bad"}}, expression)
	if len(values) != 1 || values["NAME"].Value.AsString() != "from-cli" || !values["NAME"].Sensitive || len(diagnostics) != 3 {
		t.Fatalf("Resolve() values=%#v diagnostics=%#v", values, diagnostics)
	}
	all, diagnostics := Resolve(definitions, Input{Environment: map[string]string{"TRACETEST_VAR_NAME": "from-env", "TRACETEST_VAR_MISSING": "4"}})
	if len(diagnostics) != 0 || all["NAME"].Value.AsString() != "from-env" || all["MISSING"].Value.AsBigFloat().Cmp(big.NewFloat(4)) != 0 || all["COUNT"].Value.AsBigFloat().Cmp(big.NewFloat(2)) != 0 || all["WAIT"].Value.AsBigFloat().Cmp(big.NewFloat(float64(time.Second))) != 0 {
		t.Fatalf("Resolve(all) values=%#v diagnostics=%#v", all, diagnostics)
	}
}

func TestResolveInvalidAndSensitiveValues(t *testing.T) {
	definitions := map[string]model.VariableDefinition{
		"INT":    {Type: "int"},
		"DUR":    {Type: "duration"},
		"SECRET": {Type: "int", Sensitive: true},
	}
	values, diagnostics := Resolve(definitions, Input{Vars: map[string]string{"INT": "no", "DUR": "no", "SECRET": "no"}})
	if len(values) != 0 || len(diagnostics) != 4 {
		t.Fatalf("Resolve() values=%#v diagnostics=%#v", values, diagnostics)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "invalid_variable" && diagnostic.Message == "invalid int value for variable \"SECRET\" from --var: strconv.ParseInt: parsing \"no\": invalid syntax" {
			t.Fatal("sensitive value leaked in diagnostic")
		}
	}
}

func TestParseAndDefaultValue(t *testing.T) {
	tests := []struct {
		name     string
		typeName string
		raw      string
		wantErr  bool
	}{
		{name: "string", typeName: "string", raw: "hello"},
		{name: "implicit string", raw: "hello"},
		{name: "int", typeName: "int", raw: "42"},
		{name: "duration", typeName: "duration", raw: "2s"},
		{name: "bad int", typeName: "int", raw: "x", wantErr: true},
		{name: "bad duration", typeName: "duration", raw: "x", wantErr: true},
		{name: "unsupported", typeName: "other", raw: "x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(tt.raw, tt.typeName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parse() error=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
	valid := []model.VariableDefinition{
		{Type: "string", Default: cty.StringVal("default")},
		{Type: "int", Default: cty.NumberIntVal(3)},
		{Type: "duration", Default: cty.NumberIntVal(int64(time.Second))},
	}
	for _, definition := range valid {
		if _, err := defaultValue(definition); err != nil {
			t.Fatalf("defaultValue(%q): %v", definition.Type, err)
		}
	}
	for _, definition := range []model.VariableDefinition{
		{Type: "string", Default: cty.NumberIntVal(1)},
		{Type: "int", Default: cty.StringVal("1")},
		{Type: "int", Default: cty.NumberVal(big.NewFloat(1.5))},
		{Type: "other", Default: cty.StringVal("x")},
		{Type: "string", Default: cty.NullVal(cty.String)},
		{Type: "string", Default: cty.UnknownVal(cty.String)},
	} {
		if _, err := defaultValue(definition); err == nil {
			t.Fatalf("defaultValue(%q) unexpectedly succeeded", definition.Type)
		}
	}
}

func TestReferencedRequiredEnvironmentAndEvaluation(t *testing.T) {
	parse := func(source string) hcl.Expression {
		expression, diags := hclsyntax.ParseExpression([]byte(source), "test.hcl", hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
		return expression
	}
	if got := Required(nil, parse("var.ONE"), parse("var.TWO"), parse("other.value"), parse("var[\"INDEX\"]")); len(got) != 2 {
		t.Fatalf("Required() = %#v", got)
	}
	if got := sourceRange(model.VariableDefinition{Range: model.SourceRange{File: "x", StartLine: 1, StartColumn: 2, EndLine: 3, EndColumn: 4}}); got.Filename != "x" || got.Start.Line != 1 || got.End.Column != 4 {
		t.Fatalf("sourceRange() = %#v", got)
	}
	if got := environmentMap([]string{"A=1", "B=two=parts", "without-equals"}); got["A"] != "1" || got["B"] != "two=parts" || len(got) != 2 {
		t.Fatalf("environmentMap() = %#v", got)
	}
	value, diagnostics := EvaluateDefault(parse(`duration("3s")`))
	if diagnostics.HasErrors() || value.AsBigFloat().Cmp(big.NewFloat(float64(3*time.Second))) != 0 {
		t.Fatalf("EvaluateDefault() = %s, %s", value.GoString(), diagnostics.Error())
	}
}
