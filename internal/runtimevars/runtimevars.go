// Package runtimevars resolves typed, sensitive runtime inputs.
package runtimevars

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/diagnostic"
	"lamplight/internal/expr"
	"lamplight/internal/model"
)

type Input struct {
	// Vars contains NAME=VALUE inputs already parsed by the CLI. Duplicate CLI
	// flags are intentionally rejected instead of silently taking the last one.
	Vars map[string]string
	// Environment is read once by the caller. Nil uses the process environment.
	Environment map[string]string
}

// Resolve resolves exactly the variables referenced by expressions. Supplying
// no expressions resolves all definitions, which is convenient for callers
// that do not have a selection phase yet.
func Resolve(definitions map[string]model.VariableDefinition, input Input, expressions ...hcl.Expression) (map[string]model.SensitiveValue, []model.Diagnostic) {
	required := referenced(expressions)
	if len(expressions) == 0 {
		for name := range definitions {
			required[name] = struct{}{}
		}
	}
	result := make(map[string]model.SensitiveValue, len(required))
	var diags []model.Diagnostic
	for name := range input.Vars {
		if _, exists := definitions[name]; !exists {
			diags = append(diags, model.Diagnostic{Severity: diagnostic.SeverityError, Code: diagnostic.CodeVariable, Message: fmt.Sprintf("--var references undefined variable %q", name)})
			continue
		}
		if definitions[name].Sensitive {
			diags = append(diags, model.Diagnostic{Severity: diagnostic.SeverityWarning, Code: diagnostic.CodeSensitivity, Message: fmt.Sprintf("sensitive variable %q was supplied through --var", name), Suggestion: "prefer LAMPLIGHT_VAR_" + name})
		}
	}
	for name := range required {
		definition, exists := definitions[name]
		if !exists {
			diags = append(diags, model.Diagnostic{Severity: diagnostic.SeverityError, Code: diagnostic.CodeVariable, Message: fmt.Sprintf("referenced variable %q is not declared", name)})
			continue
		}
		value, found, source := rawValue(name, input)
		if !found && !definition.HasDefault {
			diags = append(diags, diagnostic.Error(diagnostic.CodeVariable, fmt.Sprintf("required variable %q has no value", name), sourceRange(definition), "set --var "+name+"=… or LAMPLIGHT_VAR_"+name))
			continue
		}
		var parsed cty.Value
		var err error
		if found {
			parsed, err = parse(value, definition.Type)
		} else {
			source = "default"
			parsed, err = defaultValue(definition)
		}
		if err != nil {
			message := fmt.Sprintf("invalid %s value for variable %q from %s: %v", definition.Type, name, source, err)
			if definition.Sensitive {
				message = fmt.Sprintf("invalid %s value for sensitive variable %q from %s", definition.Type, name, source)
			}
			diags = append(diags, diagnostic.Error(diagnostic.CodeVariable, message, sourceRange(definition), "provide a value matching the declared type"))
			continue
		}
		result[name] = model.SensitiveValue{Value: parsed, Sensitive: definition.Sensitive}
	}
	return result, diags
}

func rawValue(name string, input Input) (string, bool, string) {
	if value, exists := input.Vars[name]; exists {
		return value, true, "--var"
	}
	environment := input.Environment
	if environment == nil {
		environment = environmentMap(os.Environ())
	}
	if value, exists := environment["LAMPLIGHT_VAR_"+name]; exists {
		return value, true, "environment"
	}
	return "", false, ""
}

func referenced(expressions []hcl.Expression) map[string]struct{} {
	result := map[string]struct{}{}
	for _, expression := range expressions {
		if expression == nil {
			continue
		}
		for _, traversal := range expression.Variables() {
			if len(traversal) < 2 || traversal.RootName() != "var" {
				continue
			}
			if attribute, ok := traversal[1].(hcl.TraverseAttr); ok {
				result[attribute.Name] = struct{}{}
			}
		}
	}
	return result
}

func parse(raw, typeName string) (cty.Value, error) {
	switch typeName {
	case "", "string":
		return cty.StringVal(raw), nil
	case "int":
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return cty.NilVal, err
		}
		return cty.NumberIntVal(value), nil
	case "duration":
		value, err := time.ParseDuration(raw)
		if err != nil {
			return cty.NilVal, err
		}
		return cty.NumberIntVal(int64(value)), nil
	default:
		return cty.NilVal, fmt.Errorf("unsupported type %q", typeName)
	}
}

func defaultValue(definition model.VariableDefinition) (cty.Value, error) {
	value := definition.Default
	if !value.IsKnown() || value.IsNull() {
		return cty.NilVal, fmt.Errorf("default is null or unknown")
	}
	switch definition.Type {
	case "", "string":
		if value.Type() != cty.String {
			return cty.NilVal, fmt.Errorf("default must be a string")
		}
	case "int", "duration":
		if value.Type() != cty.Number {
			return cty.NilVal, fmt.Errorf("default must be a number")
		}
		if _, accuracy := value.AsBigFloat().Int64(); accuracy != 0 {
			return cty.NilVal, fmt.Errorf("default must be an integer")
		}
	default:
		return cty.NilVal, fmt.Errorf("unsupported type %q", definition.Type)
	}
	return value, nil
}

func sourceRange(definition model.VariableDefinition) hcl.Range {
	return hcl.Range{Filename: definition.Range.File, Start: hcl.Pos{Line: definition.Range.StartLine, Column: definition.Range.StartColumn}, End: hcl.Pos{Line: definition.Range.EndLine, Column: definition.Range.EndColumn}}
}

func environmentMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if found {
			result[name] = value
		}
	}
	return result
}

// Required returns the referenced variable names for selection logic.
func Required(expressions ...hcl.Expression) []string {
	names := referenced(expressions)
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	return result
}

// EvaluateDefault is shared by the loader after it has supplied only pure DSL
// functions. Defaults cannot depend on runtime variables.
func EvaluateDefault(expression hcl.Expression) (cty.Value, hcl.Diagnostics) {
	return expr.Evaluate(expression, &hcl.EvalContext{Functions: expr.Functions()})
}
