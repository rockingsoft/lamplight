// Package expr builds the deliberately small, pure HCL evaluation surface.
package expr

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"lamplight/internal/model"
)

// Functions is the whitelist used for all DSL expressions. It intentionally
// excludes filesystem, environment, time, network, and shell primitives.
func Functions() map[string]function.Function {
	return map[string]function.Function{
		"lower": stdlib.LowerFunc, "upper": stdlib.UpperFunc,
		"trim": stdlib.TrimFunc, "trimspace": stdlib.TrimSpaceFunc,
		"substr": stdlib.SubstrFunc, "replace": stdlib.ReplaceFunc,
		"split": stdlib.SplitFunc, "join": stdlib.JoinFunc,
		"contains":   stdlib.ContainsFunc,
		"startswith": stringPredicate("startswith", strings.HasPrefix),
		"endswith":   stringPredicate("endswith", strings.HasSuffix),
		"matches":    matchesFunction(),
		"tostring":   stdlib.MakeToFunc(cty.String),
		"tonumber":   stdlib.MakeToFunc(cty.Number),
		"tobool":     stdlib.MakeToFunc(cty.Bool),
		"tolist":     stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)),
		"toset":      stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)),
		"tomap":      stdlib.MakeToFunc(cty.Map(cty.DynamicPseudoType)),
		"duration":   durationFunction(),
		"jsonencode": stdlib.JSONEncodeFunc,
		"jsondecode": stdlib.JSONDecodeFunc,
	}
}

func stringPredicate(name string, fn func(string, string) bool) function.Function {
	return function.New(&function.Spec{Params: []function.Parameter{{Name: "value", Type: cty.String}, {Name: "prefix", Type: cty.String}}, Type: function.StaticReturnType(cty.Bool), Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		return cty.BoolVal(fn(args[0].AsString(), args[1].AsString())), nil
	}})
}

func matchesFunction() function.Function {
	return function.New(&function.Spec{Params: []function.Parameter{{Name: "pattern", Type: cty.String}, {Name: "value", Type: cty.String}}, Type: function.StaticReturnType(cty.Bool), Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		pattern := args[0].AsString()
		re, err := regexp.Compile("^(?:" + pattern + ")$")
		if err != nil {
			return cty.NilVal, err
		}
		return cty.BoolVal(re.MatchString(args[1].AsString())), nil
	}})
}

func durationFunction() function.Function {
	return function.New(&function.Spec{Params: []function.Parameter{{Name: "value", Type: cty.String}}, Type: function.StaticReturnType(cty.Number), Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		d, err := time.ParseDuration(args[0].AsString())
		if err != nil {
			return cty.NilVal, err
		}
		return cty.NumberIntVal(int64(d)), nil
	}})
}

// StaticContext validates function calls and syntax without requiring runtime
// variable values. Dynamic roots allow normal HCL traversal type checking.
func StaticContext(roots ...string) *hcl.EvalContext {
	variables := map[string]cty.Value{}
	for _, root := range roots {
		variables[root] = cty.DynamicVal
	}
	return &hcl.EvalContext{Variables: variables, Functions: Functions()}
}

func Validate(expression hcl.Expression, roots ...string) hcl.Diagnostics {
	_, diags := expression.Value(StaticContext(roots...))
	return diags
}

func Evaluate(expression hcl.Expression, context *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	if context == nil {
		context = &hcl.EvalContext{}
	}
	if context.Functions == nil {
		context.Functions = Functions()
	}
	return expression.Value(context)
}

func Variables(values map[string]model.SensitiveValue) cty.Value {
	entries := make(map[string]cty.Value, len(values))
	for name, value := range values {
		entries[name] = value.Value
	}
	return cty.ObjectVal(entries)
}

func ResponseValue(response model.Response) cty.Value {
	headers := make(map[string]cty.Value, len(response.Headers))
	for name, values := range response.Headers {
		items := make([]cty.Value, len(values))
		for i, value := range values {
			items[i] = cty.StringVal(value)
		}
		headers[name] = cty.TupleVal(items)
	}
	jsonValue := cty.NullVal(cty.DynamicPseudoType)
	if response.JSON != nil {
		jsonValue = valueFromAny(response.JSON)
	}
	return cty.ObjectVal(map[string]cty.Value{"status_code": cty.NumberIntVal(int64(response.StatusCode)), "headers": cty.ObjectVal(headers), "body": cty.StringVal(response.Body), "json": jsonValue})
}

func SpanValue(span model.Span) cty.Value {
	attributes := make(map[string]any, len(span.Attributes)+1)
	for name, value := range span.Attributes {
		attributes[name] = value
	}
	if _, exists := attributes["tracetest.span.type"]; !exists {
		attributes["tracetest.span.type"] = spanType(attributes)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"trace_id": cty.StringVal(span.TraceID), "span_id": cty.StringVal(span.SpanID), "parent_span_id": cty.StringVal(span.ParentSpanID),
		"name": cty.StringVal(span.Name), "kind": cty.StringVal(span.Kind), "status": cty.StringVal(span.Status), "status_message": cty.StringVal(span.StatusMessage),
		"duration": cty.NumberIntVal(int64(span.Duration)), "attributes": valueFromAny(attributes),
	})
}

func spanType(attributes map[string]any) string {
	if _, exists := attributes["http.method"]; exists {
		return "http"
	}
	if _, exists := attributes["db.system"]; exists {
		return "database"
	}
	if _, exists := attributes["messaging.system"]; exists {
		return "messaging"
	}
	if _, exists := attributes["rpc.system"]; exists {
		return "rpc"
	}
	return "general"
}

func ResourceValue(resource map[string]any) cty.Value { return valueFromAny(resource) }

func ResponseContext(response model.Response, variables map[string]model.SensitiveValue, steps cty.Value) *hcl.EvalContext {
	return context(map[string]cty.Value{"response": ResponseValue(response), "var": Variables(variables), "steps": normalizeSteps(steps)})
}

func OutputContext(response model.Response, variables map[string]model.SensitiveValue, steps cty.Value) *hcl.EvalContext {
	return ResponseContext(response, variables, steps)
}

func SpanContext(span model.Span, response model.Response, variables map[string]model.SensitiveValue, steps cty.Value) *hcl.EvalContext {
	return context(map[string]cty.Value{"span": SpanValue(span), "resource": ResourceValue(span.Resource), "response": ResponseValue(response), "var": Variables(variables), "steps": normalizeSteps(steps)})
}

func context(values map[string]cty.Value) *hcl.EvalContext {
	return &hcl.EvalContext{Variables: values, Functions: Functions()}
}
func normalizeSteps(steps cty.Value) cty.Value {
	if steps == cty.NilVal {
		return cty.EmptyObjectVal
	}
	return steps
}

func valueFromAny(value any) cty.Value {
	if value == nil {
		return cty.NullVal(cty.DynamicPseudoType)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return cty.DynamicVal
	}
	var parsed ctyjson.SimpleJSONValue
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		return cty.DynamicVal
	}
	return parsed.Value
}

// RequireBool makes predicate errors consistent across response and span checks.
func RequireBool(value cty.Value) (bool, error) {
	if !value.IsKnown() || value.IsNull() || value.Type() != cty.Bool {
		return false, fmt.Errorf("expression must evaluate to a known boolean")
	}
	return value.True(), nil
}
