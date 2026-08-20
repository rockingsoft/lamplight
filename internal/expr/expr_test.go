package expr

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/model"
)

func TestPureFunctionsAndFullRegexMatch(t *testing.T) {
	expression, diags := hclsyntax.ParseExpression([]byte("lower(\"UP\") == \"up\" && matches(\"a+\", \"aaa\") && !matches(\"a+\", \"baaa\")"), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	value, diags := Evaluate(expression, &hcl.EvalContext{Functions: Functions()})
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	if value != cty.True {
		t.Fatalf("expression = %s, want true", value.GoString())
	}
}

func parseExpression(t *testing.T, source string) hcl.Expression {
	t.Helper()
	expression, diags := hclsyntax.ParseExpression([]byte(source), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parse %q: %s", source, diags.Error())
	}
	return expression
}

func TestWhitelistedFunctions(t *testing.T) {
	for _, source := range []string{
		`upper("up") == "UP"`, `trim(" x ", " ") == "x"`, `trimspace(" x ") == "x"`,
		`substr("abc", 1, 1) == "b"`, `replace("abc", "b", "x") == "axc"`,
		`split(",", "a,b")[0] == "a"`, `join(",", ["a", "b"]) == "a,b"`,
		`contains(["a", "b"], "b")`, `startswith("abc", "a")`, `endswith("abc", "c")`,
		`tostring(1) == "1"`, `tonumber("1") == 1`, `tobool("true")`,
		`tolist(["a"])[0] == "a"`, `toset(["a"]) == toset(["a"])`, `tomap({a = 1}).a == 1`,
		`duration("2s") == 2000000000`, `jsondecode(jsonencode({a = 1})).a == 1`,
	} {
		value, diags := Evaluate(parseExpression(t, source), nil)
		if diags.HasErrors() || !value.IsKnown() || !value.True() {
			t.Fatalf("Evaluate(%q) = %s, diagnostics=%s", source, value.GoString(), diags.Error())
		}
	}
}

func TestFunctionErrorsAndContexts(t *testing.T) {
	if _, diags := Evaluate(parseExpression(t, `matches("[", "x")`), nil); !diags.HasErrors() {
		t.Fatal("invalid regex did not produce diagnostics")
	}
	if _, diags := Evaluate(parseExpression(t, `duration("not-a-duration")`), nil); !diags.HasErrors() {
		t.Fatal("invalid duration did not produce diagnostics")
	}
	if len(Validate(parseExpression(t, `var.PORT`), "var")) != 0 || !Validate(parseExpression(t, `unknown.value`), "var").HasErrors() {
		t.Fatal("Validate() did not distinguish allowed and unknown roots")
	}
	if _, diags := Evaluate(parseExpression(t, `1 + 1`), &hcl.EvalContext{}); diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	if got := StaticContext("var").Variables["var"]; got != cty.DynamicVal {
		t.Fatalf("StaticContext() = %s", got.GoString())
	}
}

func TestValueContexts(t *testing.T) {
	response := model.Response{StatusCode: 201, Headers: map[string][]string{"X-Test": {"one", "two"}}, Body: "body", JSON: map[string]any{"ok": true}}
	variables := map[string]model.SensitiveValue{"TOKEN": {Value: cty.StringVal("secret"), Sensitive: true}}
	steps := cty.ObjectVal(map[string]cty.Value{"login": cty.ObjectVal(map[string]cty.Value{"outputs": cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("1")})})})
	value := ResponseValue(response)
	if value.GetAttr("status_code").AsBigFloat().Cmp(cty.NumberIntVal(201).AsBigFloat()) != 0 || value.GetAttr("json").IsNull() {
		t.Fatalf("ResponseValue() = %s", value.GoString())
	}
	if !ResponseValue(model.Response{Headers: map[string][]string{}, Body: ""}).GetAttr("json").IsNull() {
		t.Fatal("nil JSON should remain null")
	}
	span := model.Span{TraceID: "trace", SpanID: "span", ParentSpanID: "parent", Name: "request", Kind: "client", Status: "ok", Duration: time.Second, Attributes: map[string]any{"http.status_code": 201}, Resource: map[string]any{"service.name": "test"}}
	if SpanValue(span).GetAttr("name").AsString() != "request" || ResourceValue(span.Resource).GetAttr("service.name").AsString() != "test" {
		t.Fatal("span/resource context was not converted")
	}
	for _, context := range []*hcl.EvalContext{ResponseContext(response, variables, cty.NilVal), OutputContext(response, variables, steps), SpanContext(span, response, variables, steps)} {
		if context.Functions == nil || !context.Variables["steps"].IsKnown() {
			t.Fatal("context was not initialized")
		}
	}
	if Variables(variables).GetAttr("TOKEN").AsString() != "secret" {
		t.Fatal("Variables() lost the variable value")
	}
}

func TestRequireBoolAndValueConversionErrors(t *testing.T) {
	tests := []struct {
		name  string
		value cty.Value
		want  bool
		err   bool
	}{
		{name: "true", value: cty.True, want: true},
		{name: "false", value: cty.False},
		{name: "null", value: cty.NullVal(cty.Bool), err: true},
		{name: "unknown", value: cty.UnknownVal(cty.Bool), err: true},
		{name: "wrong type", value: cty.StringVal("true"), err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RequireBool(tt.value)
			if (err != nil) != tt.err || got != tt.want {
				t.Fatalf("RequireBool() = %v, %v", got, err)
			}
		})
	}
	if !valueFromAny(nil).IsNull() || valueFromAny(make(chan int)).IsKnown() || valueFromAny(math.Inf(1)).IsKnown() {
		t.Fatal("valueFromAny() did not handle nil and marshal errors")
	}
	if !valueFromAny(json.RawMessage(`{"valid":true}`)).GetAttr("valid").True() {
		t.Fatal("valueFromAny() did not decode JSON")
	}
}

func TestSpanValueProvidesLegacySemanticSpanType(t *testing.T) {
	for _, test := range []struct {
		attribute string
		want      string
	}{
		{attribute: "http.method", want: "http"},
		{attribute: "db.system", want: "database"},
		{attribute: "messaging.system", want: "messaging"},
		{attribute: "rpc.system", want: "rpc"},
		{want: "general"},
	} {
		attributes := map[string]any{}
		if test.attribute != "" {
			attributes[test.attribute] = "value"
		}
		span := model.Span{Attributes: attributes}
		got := SpanValue(span).GetAttr("attributes").GetAttr("tracetest.span.type").AsString()
		if got != test.want {
			t.Errorf("attribute %q type=%q want %q", test.attribute, got, test.want)
		}
		if _, mutated := attributes["tracetest.span.type"]; mutated {
			t.Fatal("SpanValue mutated the source attributes")
		}
	}
}
