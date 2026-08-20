// Package tracetestmigrate converts legacy Tracetest Test resources into the
// Lamplight HCL DSL. It intentionally rejects constructs whose semantics
// cannot be represented safely instead of silently dropping them.
package tracetestmigrate

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type document struct {
	Type string   `yaml:"type"`
	Spec testSpec `yaml:"spec"`
}

type testSpec struct {
	Name    string   `yaml:"name"`
	Trigger trigger  `yaml:"trigger"`
	Specs   []spec   `yaml:"specs"`
	Tags    []string `yaml:"tags"`
}

type trigger struct {
	Type        string      `yaml:"type"`
	HTTPRequest httpRequest `yaml:"httpRequest"`
}

type httpRequest struct {
	URL     string   `yaml:"url"`
	Method  string   `yaml:"method"`
	Headers []header `yaml:"headers"`
	Body    string   `yaml:"body"`
}

type header struct{ Key, Value string }
type spec struct {
	Name       string   `yaml:"name"`
	Selector   string   `yaml:"selector"`
	Assertions []string `yaml:"assertions"`
}

type Result struct {
	Name     string
	HCL      []byte
	Warnings []string
}

var (
	variablePattern          = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	selectorPattern          = regexp.MustCompile(`^\s*span(?:\[([^]]*)\])?\s*(?::(?:first|last|nth_child\([^)]*\)))?\s*$`)
	conditionPattern         = regexp.MustCompile(`^\s*([A-Za-z0-9_.:-]+)\s*(=|!=|>=|<=|>|<)\s*(.+?)\s*$`)
	selectorConditionPattern = regexp.MustCompile(`([A-Za-z0-9_.:-]+)\s*(=|!=|>=|<=|>|<)\s*("(?:[^"\\]|\\.)*"|'[^']*'|[^\s]+)`)
	countPattern             = regexp.MustCompile(`^(?:attr:)?tracetest\.selected_spans\.count\s*(=|!=|>=|<=|>|<)\s*([0-9]+)$`)
)

// Convert converts exactly one Tracetest resource. Multi-document YAML is
// rejected so callers can point users at the exact source file.
func Convert(source []byte) (Result, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(false)
	var doc document
	if err := decoder.Decode(&doc); err != nil {
		return Result{}, fmt.Errorf("decode Tracetest YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil && extra != nil {
		return Result{}, fmt.Errorf("multi-document YAML is not supported; split resources into separate files")
	}
	if doc.Type != "Test" {
		return Result{}, fmt.Errorf("resource type %q is not supported; expected Test", doc.Type)
	}
	if strings.TrimSpace(doc.Spec.Name) == "" {
		return Result{}, fmt.Errorf("spec.name is required")
	}
	if doc.Spec.Trigger.Type != "http" {
		return Result{}, fmt.Errorf("trigger type %q is not supported; Lamplight migration currently supports http", doc.Spec.Trigger.Type)
	}
	request := doc.Spec.Trigger.HTTPRequest
	if request.URL == "" || request.Method == "" {
		return Result{}, fmt.Errorf("http trigger requires method and url")
	}

	variables := map[string]struct{}{}
	collectVariables(variables, request.URL, request.Method, request.Body)
	for _, h := range request.Headers {
		if h.Key == "" {
			return Result{}, fmt.Errorf("http trigger contains a header without a key")
		}
		collectVariables(variables, h.Value)
	}

	var out strings.Builder
	variableNames := sortedKeys(variables)
	for _, name := range variableNames {
		fmt.Fprintf(&out, "variable %q {\n  type = string\n}\n\n", name)
	}
	fmt.Fprintf(&out, "test %q {\n", doc.Spec.Name)
	if len(doc.Spec.Tags) > 0 {
		out.WriteString("  tags = [")
		for index, tag := range doc.Spec.Tags {
			if index > 0 {
				out.WriteString(", ")
			}
			out.WriteString(strconv.Quote(tag))
		}
		out.WriteString("]\n\n")
	}
	out.WriteString("  step \"request\" {\n    http_request {\n")
	fmt.Fprintf(&out, "      method = %s\n      url    = %s\n", templateString(request.Method), templateString(request.URL))
	if len(request.Headers) > 0 {
		out.WriteString("      headers = {\n")
		for _, h := range request.Headers {
			fmt.Fprintf(&out, "        %s = %s\n", strconv.Quote(h.Key), templateString(h.Value))
		}
		out.WriteString("      }\n")
	}
	if request.Body != "" {
		fmt.Fprintf(&out, "      body = %s\n", templateString(request.Body))
	}
	out.WriteString("    }\n")

	result := Result{Name: doc.Spec.Name}
	for index, oldSpec := range doc.Spec.Specs {
		converted, warnings, err := convertSpec(oldSpec, index)
		if err != nil {
			return Result{}, fmt.Errorf("spec %d (%q): %w", index+1, oldSpec.Name, err)
		}
		result.Warnings = append(result.Warnings, warnings...)
		out.WriteString(converted)
	}
	out.WriteString("  }\n}\n")
	result.HCL = []byte(out.String())
	return result, nil
}

func convertSpec(old spec, index int) (string, []string, error) {
	name := strings.TrimSpace(old.Name)
	if name == "" {
		name = fmt.Sprintf("tracetest spec %d", index+1)
	}
	selector, err := convertSelector(old.Selector)
	if err != nil {
		return "", nil, err
	}
	responses := map[string]string{}
	spanAssertions := map[string]string{}
	quantityName, quantityValue := "at_least", 1
	warnings := []string{}
	for assertionIndex, raw := range old.Assertions {
		raw = strings.TrimSpace(raw)
		if match := countPattern.FindStringSubmatch(raw); match != nil {
			value, _ := strconv.Atoi(match[2])
			switch match[1] {
			case "=":
				quantityName, quantityValue = "exactly", value
			case ">=":
				quantityName, quantityValue = "at_least", value
			case ">":
				quantityName, quantityValue = "at_least", value+1
			case "<=":
				quantityName, quantityValue = "at_most", value
			case "<":
				if value == 0 {
					return "", nil, fmt.Errorf("invalid count assertion %q", raw)
				}
				quantityName, quantityValue = "at_most", value-1
			default:
				return "", nil, fmt.Errorf("count assertion %q cannot be represented", raw)
			}
			continue
		}
		expression, response, err := convertAssertion(raw)
		if err != nil {
			return "", nil, err
		}
		assertionName := fmt.Sprintf("assertion %d", assertionIndex+1)
		if response {
			responses[assertionName] = expression
		} else {
			spanAssertions[assertionName] = expression
		}
	}
	if len(spanAssertions) > 0 {
		warnings = append(warnings, fmt.Sprintf("%s: Tracetest applies assertions to every selected span; Lamplight will require the configured number of spans to satisfy them", name))
	}
	var out strings.Builder
	fmt.Fprintf(&out, "\n    check %q {\n", name)
	if len(responses) > 0 {
		out.WriteString("      response = {\n")
		writeExpressionMap(&out, responses, 8)
		out.WriteString("      }\n")
	}
	if old.Selector != "" || len(spanAssertions) > 0 || len(responses) == 0 {
		out.WriteString("      spans {\n")
		fmt.Fprintf(&out, "        matching = %s\n", selector)
		if len(spanAssertions) > 0 {
			out.WriteString("        span_assertions = {\n")
			writeExpressionMap(&out, spanAssertions, 10)
			out.WriteString("        }\n")
		}
		fmt.Fprintf(&out, "        %-8s = %d\n", quantityName, quantityValue)
		out.WriteString("      }\n")
	}
	out.WriteString("    }\n")
	return out.String(), warnings, nil
}

func convertSelector(raw string) (string, error) {
	match := selectorPattern.FindStringSubmatch(raw)
	if match == nil {
		return "", fmt.Errorf("selector %q is not a single span selector", raw)
	}
	if strings.TrimSpace(match[1]) == "" {
		return "true", nil
	}
	body := strings.TrimSpace(match[1])
	locations := selectorConditionPattern.FindAllStringIndex(body, -1)
	if len(locations) == 0 {
		return "", fmt.Errorf("selector %q has no supported conditions", raw)
	}
	converted := make([]string, 0, len(locations))
	position := 0
	for _, location := range locations {
		separator := strings.TrimSpace(body[position:location[0]])
		if separator != "" && separator != "and" && separator != "&&" {
			return "", fmt.Errorf("selector %q contains unsupported syntax %q", raw, separator)
		}
		expression, _, err := convertCondition(body[location[0]:location[1]])
		if err != nil {
			return "", fmt.Errorf("selector: %w", err)
		}
		converted = append(converted, expression)
		position = location[1]
	}
	if tail := strings.TrimSpace(body[position:]); tail != "" {
		return "", fmt.Errorf("selector %q contains unsupported syntax %q", raw, tail)
	}
	return strings.Join(converted, " && "), nil
}

func convertAssertion(raw string) (string, bool, error) {
	return convertCondition(raw)
}

func convertCondition(raw string) (string, bool, error) {
	match := conditionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return "", false, fmt.Errorf("assertion %q is not supported", raw)
	}
	left, response := attribute(match[1])
	if left == "" {
		return "", false, fmt.Errorf("attribute %q is not supported", match[1])
	}
	op := match[2]
	if op == "=" {
		op = "=="
	}
	right := strings.TrimSpace(match[3])
	if regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h)$`).MatchString(right) {
		right = "duration(" + strconv.Quote(right) + ")"
	} else if !regexp.MustCompile(`^(?:true|false|null|-?[0-9]+(?:\.[0-9]+)?|"(?:[^"\\]|\\.)*"|'[^']*')$`).MatchString(right) {
		return "", false, fmt.Errorf("right-hand value %q is not supported", right)
	} else if strings.HasPrefix(right, "'") {
		right = strconv.Quote(strings.Trim(right, "'"))
	}
	return left + " " + op + " " + right, response, nil
}

func attribute(raw string) (string, bool) {
	raw = strings.TrimPrefix(raw, "attr:")
	switch raw {
	case "tracetest.response.status", "tracetest.response.status_code":
		return "response.status_code", true
	case "tracetest.response.body":
		return "response.body", true
	case "tracetest.span.duration":
		return "span.duration", false
	case "tracetest.span.name", "name":
		return "span.name", false
	case "tracetest.span.status":
		return "span.status", false
	case "tracetest.span.type":
		return `span.attributes["tracetest.span.type"]`, false
	case "service.name":
		return `resource["service.name"]`, false
	}
	if strings.HasPrefix(raw, "resource.") {
		return "resource[" + strconv.Quote(strings.TrimPrefix(raw, "resource.")) + "]", false
	}
	if strings.Contains(raw, ".") {
		return "span.attributes[" + strconv.Quote(raw) + "]", false
	}
	return "", false
}

func templateString(value string) string {
	return strconv.Quote(variablePattern.ReplaceAllString(value, `${var.$1}`))
}

func collectVariables(target map[string]struct{}, values ...string) {
	for _, value := range values {
		for _, match := range variablePattern.FindAllStringSubmatch(value, -1) {
			target[match[1]] = struct{}{}
		}
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeExpressionMap(out *strings.Builder, values map[string]string, indent int) {
	padding := strings.Repeat(" ", indent)
	for _, key := range sortedKeys(values) {
		fmt.Fprintf(out, "%s%s = %s\n", padding, strconv.Quote(key), values[key])
	}
}
