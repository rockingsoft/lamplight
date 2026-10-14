package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"lamplight/internal/config"
	"lamplight/internal/expr"
	"lamplight/internal/hclloader"
	"lamplight/internal/model"
)

type checkCapability struct {
	ResponseContext       []string `json:"response_context"`
	SpanContext           []string `json:"span_context"`
	MetricContext         []string `json:"metric_context"`
	QuantityRules         []string `json:"quantity_rules"`
	ObservationWindowType string   `json:"observation_window_type"`
	Notes                 []string `json:"notes"`
}

type capabilitiesOutput struct {
	Triggers        []hclloader.TriggerCapability `json:"triggers"`
	StepReuse       stepReuseCapability           `json:"step_reuse"`
	Checks          checkCapability               `json:"checks"`
	VariableTypes   []string                      `json:"variable_types"`
	TargetRuntimes  []string                      `json:"target_runtimes"`
	DatasourceKinds []string                      `json:"datasource_kinds"`
	MetricsKinds    []string                      `json:"metrics_kinds"`
	Functions       []string                      `json:"functions"`
	Unsupported     []string                      `json:"unsupported"`
}

type stepReuseCapability struct {
	ReferenceSyntax string   `json:"reference_syntax"`
	DefaultsContext []string `json:"defaults_context"`
	OverrideContext []string `json:"override_context"`
	BodyContext     []string `json:"body_context"`
	Notes           []string `json:"notes"`
}

func (s *service) capabilities(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, capabilitiesOutput, error) {
	functions := make([]string, 0, len(expr.Functions()))
	for name := range expr.Functions() {
		functions = append(functions, name)
	}
	sort.Strings(functions)
	datasources := append([]string(nil), model.SupportedDatasourceKinds...)
	sort.Strings(datasources)
	out := capabilitiesOutput{
		Triggers: hclloader.TriggerCapabilities(),
		StepReuse: stepReuseCapability{
			ReferenceSyntax: "ref = TEST.steps.STEP",
			DefaultsContext: []string{"var"},
			OverrideContext: []string{"var", "steps"},
			BodyContext:     []string{"params", "var", "steps"},
			Notes: []string{
				"Declare params = { NAME = EXPRESSION } on a concrete step to provide defaults.",
				"A reference executes a fresh step under its local name, with its own trace, checks, and outputs.",
				"A referencing step contains only ref and optional params; references to another reference are unsupported.",
			},
		},
		Checks: checkCapability{
			ResponseContext:       []string{"response.status_code", "response.headers", "response.body", "response.json", "var", "steps", "params"},
			SpanContext:           []string{"span.trace_id", "span.span_id", "span.parent_span_id", "span.name", "span.kind", "span.status", "span.status_message", "span.duration", "span.attributes", "resource", "response", "var", "steps", "params"},
			MetricContext:         []string{"metric.name", "metric.type", "metric.labels", "metric.attributes", "metric.resource", "metric.previous_value", "metric.value", "metric.delta", "response", "var", "steps", "params"},
			QuantityRules:         []string{"at_least", "at_most", "exactly"},
			ObservationWindowType: "duration",
			Notes: []string{
				"A check contains response, spans, metrics, or a combination.",
				"A spans block requires matching and exactly one quantity rule.",
				"A metrics block requires one PromQL query plus exactly one quantity rule; metric.delta compares pre-trigger and post-trigger query results.",
				"A datasource \"otlp\" receives traces and metrics; no separate OTLP metrics source is configured.",
				"Keep span identity in matching and behavior in named assertions.",
				"Use observed normalized attribute names and types; do not invent selectors.",
			},
		},
		VariableTypes:   []string{"string", "int", "duration"},
		TargetRuntimes:  []string{"local", "docker_compose", "kubernetes"},
		DatasourceKinds: datasources,
		MetricsKinds:    []string{"prometheus", "prometheus_scrape"},
		Functions:       functions,
		Unsupported: []string{
			"Terraform providers, modules, locals, and env()",
			"TraceQL or Tracetest selector syntax in matching",
			"span relationships, events, links, and aggregate queries",
		},
	}
	return nil, out, nil
}

type referenceInput struct {
	Topic string `json:"topic,omitempty" jsonschema:"one of overview, variables, steps, triggers, checks, expressions, targets, workflow; defaults to overview"`
}

type referenceOutput struct {
	Topic           string   `json:"topic"`
	AvailableTopics []string `json:"available_topics"`
	Reference       string   `json:"reference"`
}

var referenceTopics = map[string]string{
	"overview":    "A project has one .lamplight file and recursively discovered .wick files below project.base_dir. Tests contain ordered steps; a concrete step has one trigger, optional outputs, and zero or more named checks. A ref step reuses a concrete step definition.",
	"variables":   "Declare variable blocks in .lamplight or discovered .wick files. Supported types are string, int, and duration. Mark credentials sensitive = true and supply them through LAMPLIGHT_VAR_NAME. Runtime precedence is --var, environment, selected target, then default.",
	"steps":       "Steps execute in source order. A concrete step may declare params = { NAME = var.VALUE }; its trigger, outputs, and checks can use params.NAME. A caller declares a step with ref = TEST.steps.STEP and optional params overrides. ref executes a fresh step under the caller's local name, and later steps use steps.LOCAL.outputs.NAME. Defaults use var; overrides use var or earlier steps. A ref step cannot declare a trigger, checks, or outputs, and cannot reference another ref step.",
	"triggers":    "Call lamplight_get_capabilities for the authoritative trigger inventory, required and optional attributes, propagation mode, and a complete example for every supported trigger block.",
	"checks":      "Response expressions use response, var, steps, and params. Span matching and assertions also use span and resource. Metric checks always select series with PromQL; assertions use metric, whose delta is the post-trigger query value minus the pre-trigger query value. Span and metric blocks each require exactly one of at_least, at_most, or exactly.",
	"expressions": "Lamplight exposes a small pure HCL expression surface. Call lamplight_get_capabilities for the current function list. Attribute maps use bracket access for semantic keys, for example resource[\"service.name\"] and span.attributes[\"http.request.method\"]. Numeric and boolean telemetry values are not strings.",
	"targets":     "The implicit local target runs in the MCP process environment. docker_compose and kubernetes start ephemeral remote executors. Target values may set declared non-sensitive variables. Executable k6 scripts use the local target; an explicit cloud_run executor may distribute the k6 process from that target.",
	"workflow":    "Inspect capabilities and existing files, scaffold or draft the smallest observable contract, validate content without writing, write with the current SHA-256 precondition, lint the project, then run one selected test. Derive span predicates from representative trace evidence and report static validation separately from live execution.",
}

func (s *service) reference(_ context.Context, _ *mcp.CallToolRequest, in referenceInput) (*mcp.CallToolResult, referenceOutput, error) {
	topics := make([]string, 0, len(referenceTopics))
	for topic := range referenceTopics {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	topic := in.Topic
	if topic == "" {
		topic = "overview"
	}
	reference, exists := referenceTopics[topic]
	out := referenceOutput{Topic: topic, AvailableTopics: topics, Reference: reference}
	if !exists {
		return toolError(fmt.Sprintf("unknown reference topic %q", topic), out), out, nil
	}
	return nil, out, nil
}

type scaffoldInput struct {
	Trigger  string `json:"trigger,omitempty" jsonschema:"trigger block returned by lamplight_get_capabilities; use this or ref"`
	Ref      string `json:"ref,omitempty" jsonschema:"static TEST.steps.STEP reference; use this or trigger"`
	TestName string `json:"test_name,omitempty" jsonschema:"test label; defaults to example"`
	StepName string `json:"step_name,omitempty" jsonschema:"valid HCL identifier; defaults to trigger"`
}

type scaffoldOutput struct {
	Trigger string   `json:"trigger"`
	Ref     string   `json:"ref,omitempty"`
	Content string   `json:"content"`
	Notes   []string `json:"notes"`
}

func (s *service) scaffold(_ context.Context, _ *mcp.CallToolRequest, in scaffoldInput) (*mcp.CallToolResult, scaffoldOutput, error) {
	if (in.Trigger == "") == (in.Ref == "") {
		out := scaffoldOutput{Trigger: in.Trigger, Ref: in.Ref}
		return toolError("supply exactly one of trigger or ref", out), out, nil
	}
	var capability *hclloader.TriggerCapability
	if in.Trigger != "" {
		for _, candidate := range hclloader.TriggerCapabilities() {
			if candidate.Block == in.Trigger {
				copy := candidate
				capability = &copy
				break
			}
		}
		if capability == nil {
			out := scaffoldOutput{Trigger: in.Trigger}
			return toolError(fmt.Sprintf("unsupported trigger %q; call lamplight_get_capabilities", in.Trigger), out), out, nil
		}
	} else {
		parts := strings.Split(in.Ref, ".")
		if len(parts) != 3 || parts[1] != "steps" || !hclsyntax.ValidIdentifier(parts[0]) || !hclsyntax.ValidIdentifier(parts[2]) {
			out := scaffoldOutput{Ref: in.Ref}
			return toolError("ref must be TEST.steps.STEP", out), out, nil
		}
	}
	testName := strings.TrimSpace(in.TestName)
	if testName == "" {
		testName = "example"
	}
	stepName := strings.TrimSpace(in.StepName)
	if stepName == "" {
		stepName = "trigger"
	}
	if !hclsyntax.ValidIdentifier(stepName) {
		out := scaffoldOutput{Trigger: in.Trigger}
		return toolError("step_name must be a valid HCL identifier", out), out, nil
	}
	quotedTestName := strings.ReplaceAll(strings.ReplaceAll(testName, "\\", "\\\\"), "\"", "\\\"")
	stepBody := "    ref = " + in.Ref
	if capability != nil {
		stepBody = indent(capability.Example, "    ")
	}
	source := fmt.Sprintf("test \"%s\" {\n  tags = [\"draft\"]\n\n  step \"%s\" {\n%s\n  }\n}\n", quotedTestName, stepName, stepBody)
	formatted := hclwrite.Format([]byte(source))
	out := scaffoldOutput{
		Trigger: in.Trigger,
		Ref:     in.Ref,
		Content: string(formatted),
		Notes: []string{
			"Replace placeholder endpoints, payloads, IDs, and scripts with non-sensitive test values.",
			"Add response checks for the external contract on a concrete step; derive span checks from trace evidence and metric checks from the actual Prometheus exposition.",
			"For ref steps, add params overrides only for parameters declared by the source step.",
			"Validate this content with lamplight_validate_test_content before writing it.",
		},
	}
	return nil, out, nil
}

type validateContentInput struct {
	Path    string `json:"path" jsonschema:"prospective relative .wick path below project.base_dir"`
	Content string `json:"content" jsonschema:"complete prospective Lamplight HCL file content"`
}

type validateContentOutput struct {
	Path             string             `json:"path"`
	Valid            bool               `json:"valid"`
	FormattedContent string             `json:"formatted_content"`
	AlreadyFormatted bool               `json:"already_formatted"`
	Tests            []string           `json:"tests,omitempty"`
	Variables        []string           `json:"variables,omitempty"`
	Diagnostics      []model.Diagnostic `json:"diagnostics,omitempty"`
}

func (s *service) validateContent(_ context.Context, _ *mcp.CallToolRequest, in validateContentInput) (*mcp.CallToolResult, validateContentOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, base, err := s.safePath(in.Path)
	if err != nil {
		out := validateContentOutput{Path: in.Path}
		return toolError(err.Error(), out), out, nil
	}
	formatted := hclwrite.Format([]byte(in.Content))
	definition, diags := (hclloader.Loader{}).ValidateTestContent(config.Options{ConfigPath: s.options.ConfigPath, WorkingDir: s.options.WorkingDir}, path, []byte(in.Content))
	out := validateContentOutput{Path: relative(base, path), Valid: !hasErrors(diags), FormattedContent: string(formatted), AlreadyFormatted: bytes.Equal([]byte(in.Content), formatted), Diagnostics: diags}
	if definition != nil {
		for name, test := range definition.Tests {
			if test.File == path {
				out.Tests = append(out.Tests, name)
			}
		}
		for name, variable := range definition.Variables {
			if variable.Range.File == path {
				out.Variables = append(out.Variables, name)
			}
		}
		sort.Strings(out.Tests)
		sort.Strings(out.Variables)
	}
	if !out.Valid {
		return toolError("prospective test content is invalid", out), out, nil
	}
	return nil, out, nil
}

func indent(value, prefix string) string {
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = prefix + lines[index]
	}
	return strings.Join(lines, "\n")
}
