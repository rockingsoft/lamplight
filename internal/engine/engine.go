// Package engine executes Lamplight test definitions.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"lamplight/internal/debuglog"
	"lamplight/internal/expr"
	"lamplight/internal/model"
	"lamplight/internal/poller"
	"lamplight/internal/result"
)

type Engine struct {
	HTTP         model.HTTPExecutor
	Triggers     model.TriggerExecutor
	TraceFactory model.TraceContextFactory
	Clock        model.Clock
	FailFast     bool
	Progress     ProgressFunc
}

func (e *Engine) Run(ctx context.Context, project *model.Project) model.RunResult {
	started := time.Now()
	run := model.RunResult{SchemaVersion: 1, RunID: randomID(), StartedAt: started, Tests: []model.TestResult{}}
	debuglog.Debug(ctx, "engine run started", "run_id", run.RunID)
	e.progress(ProgressEvent{Kind: ProgressRunStarted, RunID: run.RunID, TestsTotal: len(projectTests(project))})
	if project == nil || project.Definition == nil || e.HTTP == nil {
		run.Status = model.StatusError
		run.Summary.Errors = 1
		return run
	}
	if needsDatasource(project.Tests) {
		debuglog.Debug(ctx, "testing datasource connection")
		e.progress(ProgressEvent{Kind: ProgressDatasourceStarted, RunID: run.RunID})
		if project.Datasource == nil {
			e.progress(ProgressEvent{Kind: ProgressDatasourceCompleted, RunID: run.RunID, Status: model.StatusError})
			return technicalRun(run, "datasource_required", "selected tests contain span checks but no datasource is configured")
		}
		if err := project.Datasource.TestConnection(ctx); err != nil {
			e.progress(ProgressEvent{Kind: ProgressDatasourceCompleted, RunID: run.RunID, Status: model.StatusError})
			return technicalRun(run, "datasource_connection", err.Error())
		}
		e.progress(ProgressEvent{Kind: ProgressDatasourceCompleted, RunID: run.RunID, Status: model.StatusPassed})
	}
	for index, test := range project.Tests {
		if ctx.Err() != nil {
			run.Tests = append(run.Tests, cancelledTests(project.Tests[index:])...)
			break
		}
		debuglog.Debug(ctx, "test started", "name", test.Name, "steps", len(test.Steps))
		e.progress(ProgressEvent{Kind: ProgressTestStarted, RunID: run.RunID, TestName: test.Name})
		tr, _ := e.runTest(ctx, project, test)
		debuglog.Debug(ctx, "test completed", "name", test.Name, "status", tr.Status, "duration_ms", tr.DurationMS)
		e.progress(ProgressEvent{Kind: ProgressTestCompleted, RunID: run.RunID, TestName: test.Name, Status: tr.Status, DurationMS: tr.DurationMS})
		run.Tests = append(run.Tests, tr)
		if tr.Status == model.StatusCancelled || (e.FailFast && tr.Status != model.StatusPassed) {
			run.Tests = append(run.Tests, skippedTests(project.Tests[index+1:])...)
			break
		}
	}
	run.DurationMS = time.Since(started).Milliseconds()
	run = result.AggregateRun(run)
	return run
}

func projectTests(project *model.Project) []model.TestDefinition {
	if project == nil {
		return nil
	}
	return project.Tests
}

func (e *Engine) progress(event ProgressEvent) {
	if e.Progress != nil {
		e.Progress(event)
	}
}

func (e *Engine) runTest(ctx context.Context, project *model.Project, test model.TestDefinition) (model.TestResult, bool) {
	started := time.Now()
	tr := model.TestResult{Name: test.Name, Tags: test.Tags, File: test.File, Status: model.StatusPassed, Steps: []model.StepResult{}}
	stepOutputs := map[string]map[string]cty.Value{}
	for index, step := range test.Steps {
		sr, technical := e.runStep(ctx, project, step, stepOutputs)
		tr.Steps = append(tr.Steps, sr)
		if sr.Status != model.StatusPassed {
			tr.Status = sr.Status
			tr.Steps = append(tr.Steps, skippedStepResults(test.Steps[index+1:])...)
			tr.DurationMS = time.Since(started).Milliseconds()
			return tr, technical
		}
	}
	tr.DurationMS = time.Since(started).Milliseconds()
	return tr, false
}

func (e *Engine) runStep(ctx context.Context, project *model.Project, step model.StepDefinition, previous map[string]map[string]cty.Value) (model.StepResult, bool) {
	started := time.Now()
	sr := model.StepResult{Name: step.Name, ExecutionID: randomID(), Status: model.StatusPassed, Checks: []model.CheckResult{}}
	debuglog.Debug(ctx, "step started", "name", step.Name, "execution_id", sr.ExecutionID, "trigger", step.Trigger.Kind)
	e.progress(ProgressEvent{Kind: ProgressStepStarted, StepName: step.Name, Trigger: step.Trigger.Kind})
	finish := func(status model.Status) (model.StepResult, bool) {
		sr.Status = status
		sr.DurationMS = time.Since(started).Milliseconds()
		debuglog.Debug(ctx, "step completed", "name", step.Name, "status", status, "duration_ms", sr.DurationMS, "checks", len(sr.Checks))
		e.progress(ProgressEvent{Kind: ProgressStepCompleted, StepName: step.Name, Trigger: step.Trigger.Kind, Status: status, DurationMS: sr.DurationMS})
		return sr, status == model.StatusError || status == model.StatusCancelled
	}
	if ctx.Err() != nil {
		return finish(model.StatusCancelled)
	}
	var trace *model.TestTraceContext
	if project.Datasource != nil {
		if e.TraceFactory == nil {
			sr.Error = diagnostic("trace_context", "trace context factory is not configured")
			return finish(model.StatusError)
		}
		created, err := e.TraceFactory.New()
		if err != nil {
			sr.Error = diagnostic("trace_context", err.Error())
			return finish(model.StatusError)
		}
		trace = &created
		sr.TraceID = string(created.TraceID)
	}
	stepsValue := stepsCTY(previous)
	var response model.Response
	var err error
	var triggerStarted time.Time
	executionCode := "trigger_execution"
	if step.Trigger.Kind == "" || step.Trigger.Kind == model.TriggerHTTP {
		executionCode = "http_execution"
		req, evalErr := evaluateRequest(step.HTTP, project.Variables, stepsValue)
		if evalErr != nil {
			sr.Error = diagnostic("request_evaluation", evalErr.Error())
			return finish(model.StatusError)
		}
		sr.Request = &req
		triggerStarted = time.Now()
		e.progress(ProgressEvent{Kind: ProgressTriggerStarted, StepName: step.Name, Trigger: model.TriggerHTTP})
		response, err = e.HTTP.Execute(ctx, req, project.HTTPClient, trace)
	} else {
		if e.Triggers == nil {
			sr.Error = diagnostic("trigger_execution", "trigger executor is not configured")
			return finish(model.StatusError)
		}
		req, evalErr := evaluateTrigger(step.Trigger, project.Variables, stepsValue)
		if evalErr != nil {
			sr.Error = diagnostic("trigger_evaluation", evalErr.Error())
			return finish(model.StatusError)
		}
		if trace != nil && isTraceIDTrigger(req.Kind) {
			id, _ := req.Attributes["id"].(string)
			if len(id) != 32 || !validHex(id) {
				sr.Error = diagnostic("trigger_evaluation", fmt.Sprintf("%s.id must be a 32-character trace ID", req.Kind))
				return finish(model.StatusError)
			}
			trace.TraceID = model.TraceID(id)
			sr.TraceID = id
		}
		sr.Trigger = &req
		triggerStarted = time.Now()
		e.progress(ProgressEvent{Kind: ProgressTriggerStarted, StepName: step.Name, Trigger: req.Kind})
		response, err = e.Triggers.Execute(ctx, req, project.HTTPClient, trace)
	}
	triggerStatus := model.StatusPassed
	if err != nil {
		triggerStatus = model.StatusError
	}
	e.progress(ProgressEvent{Kind: ProgressTriggerCompleted, StepName: step.Name, Trigger: triggerKind(step), Status: triggerStatus, StatusCode: response.StatusCode, DurationMS: time.Since(triggerStarted).Milliseconds()})
	if err != nil {
		sr.Error = diagnostic(executionCode, err.Error())
		return finish(model.StatusError)
	}
	debuglog.Debug(ctx, "step request completed", "name", step.Name, "status_code", response.StatusCode, "response_bytes", len(response.Body))
	sr.Response = &response

	outputs := map[string]cty.Value{}
	for _, name := range sortedExpressions(step.Outputs) {
		value, diags := expr.Evaluate(step.Outputs[name], expr.OutputContext(response, project.Variables, stepsValue))
		if diags.HasErrors() || !value.IsKnown() {
			sr.Error = diagnostic("output_evaluation", fmt.Sprintf("output %q: %s", name, diags.Error()))
			return finish(model.StatusError)
		}
		outputs[name] = value
	}
	previous[step.Name] = outputs
	sr.Outputs = ctyMapAny(outputs)

	spanChecks := []poller.SpanCheck{}
	responseFailed := false
	for _, check := range step.Checks {
		cr := model.CheckResult{Name: check.Name, Status: model.StatusPassed}
		for _, name := range sortedExpressions(check.Response) {
			value, diags := expr.Evaluate(check.Response[name], expr.ResponseContext(response, project.Variables, stepsValue))
			ev := model.AssertionEvidence{Name: name, Source: model.Range(check.Response[name].Range())}
			if diags.HasErrors() {
				ev.Error = diags.Error()
				cr.ResponseEvidence = append(cr.ResponseEvidence, ev)
				sr.Checks = append(sr.Checks, cr)
				sr.Error = diagnostic("check_evaluation", fmt.Sprintf("check %q: %s", check.Name, diags.Error()))
				return finish(model.StatusError)
			}
			passed, err := expr.RequireBool(value)
			if err != nil {
				sr.Error = diagnostic("check_type", fmt.Sprintf("check %q condition %q: %v", check.Name, name, err))
				return finish(model.StatusError)
			}
			ev.Passed, ev.Value = passed, passed
			cr.ResponseEvidence = append(cr.ResponseEvidence, ev)
			if !passed {
				cr.Status, cr.Reason, responseFailed = model.StatusFailed, "response_condition_failed", true
			}
		}
		sr.Checks = append(sr.Checks, cr)
		if check.Spans != nil {
			definition := check
			spanChecks = append(spanChecks, poller.SpanCheck{Name: check.Name, Rule: check.Spans.Rule, Match: spanMatcher(definition, response, project.Variables, stepsValue), Assertions: spanAssertions(definition, response, project.Variables, stepsValue)})
		}
	}
	if responseFailed {
		return finish(model.StatusFailed)
	}
	if len(spanChecks) > 0 {
		window, settle := traceWindows(project.Definition.Datasource, step.Checks)
		debuglog.Debug(ctx, "polling trace", "trace_id", trace.TraceID, "checks", len(spanChecks), "observation_window", window, "settle_window", settle)
		e.progress(ProgressEvent{Kind: ProgressTracePolling, StepName: step.Name, ObservationWindow: window})
		interval := time.Second
		if project.Definition.Datasource != nil && project.Definition.Datasource.PollingInterval > 0 {
			interval = project.Definition.Datasource.PollingInterval
		}
		polled, err := poller.Poll(ctx, project.Datasource, trace.TraceID, poller.Config{ObservationWindow: window, SettleWindow: settle, Interval: interval, Clock: e.Clock, Progress: func(progress poller.Progress) {
			checks := make([]ProgressCheck, len(progress.Checks))
			for index, check := range progress.Checks {
				checks[index] = ProgressCheck{Name: check.Name, MatchCount: check.MatchCount, Status: check.Status}
			}
			e.progress(ProgressEvent{Kind: ProgressTraceObserved, StepName: step.Name, Attempt: progress.Attempt, SpanCount: progress.SpanCount, Found: progress.Found, Complete: progress.Complete, RetryError: progress.RetryError, Checks: checks})
		}}, spanChecks)
		if err != nil {
			sr.Error = diagnostic("trace_observation", err.Error())
			return finish(model.StatusError)
		}
		for _, observed := range polled.Checks {
			if observed.Reason == "trace_not_observed" {
				sr.Error = diagnostic("trace_not_observed", "trace was not observed before the observation window elapsed")
				return finish(model.StatusError)
			}
			mergeCheck(&sr.Checks, observed)
			if observed.Status == model.StatusFailed {
				sr.Status = model.StatusFailed
			} else if observed.Status == model.StatusCancelled {
				return finish(model.StatusCancelled)
			}
		}
	}
	return finish(sr.Status)
}

func triggerKind(step model.StepDefinition) model.TriggerKind {
	if step.Trigger.Kind == "" {
		return model.TriggerHTTP
	}
	return step.Trigger.Kind
}

func validHex(value string) bool { _, err := hex.DecodeString(value); return err == nil }

func isTraceIDTrigger(kind model.TriggerKind) bool {
	switch kind {
	case model.TriggerTraceID, model.TriggerCypress, model.TriggerPlaywright, model.TriggerArtillery, model.TriggerK6:
		return true
	default:
		return false
	}
}

func evaluateTrigger(def model.TriggerDefinition, variables map[string]model.SensitiveValue, steps cty.Value) (model.TriggerRequest, error) {
	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{"var": expr.Variables(variables), "steps": steps}, Functions: expr.Functions()}
	request := model.TriggerRequest{Kind: def.Kind, Attributes: map[string]any{}}
	for _, name := range sortedExpressions(def.Attributes) {
		value, diags := expr.Evaluate(def.Attributes[name], ctx)
		if diags.HasErrors() || !value.IsKnown() || value.IsNull() {
			return request, fmt.Errorf("%s must evaluate to a known non-null value: %s", name, diags.Error())
		}
		encoded, err := ctyjson.Marshal(value, value.Type())
		if err != nil {
			return request, fmt.Errorf("encode %s: %w", name, err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return request, fmt.Errorf("decode %s: %w", name, err)
		}
		request.Attributes[name] = decoded
	}
	return request, nil
}

func evaluateRequest(def model.HTTPRequestDefinition, variables map[string]model.SensitiveValue, steps cty.Value) (model.HTTPRequest, error) {
	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{"var": expr.Variables(variables), "steps": steps}, Functions: expr.Functions()}
	stringValue := func(name string, expression hcl.Expression, required bool) (string, error) {
		if expression == nil {
			if required {
				return "", fmt.Errorf("%s is required", name)
			}
			return "", nil
		}
		value, diags := expr.Evaluate(expression, ctx)
		if diags.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.String {
			return "", fmt.Errorf("%s must evaluate to string: %s", name, diags.Error())
		}
		return value.AsString(), nil
	}
	method, err := stringValue("method", def.Method, true)
	if err != nil {
		return model.HTTPRequest{}, err
	}
	url, err := stringValue("url", def.URL, true)
	if err != nil {
		return model.HTTPRequest{}, err
	}
	body, err := stringValue("body", def.Body, false)
	if err != nil {
		return model.HTTPRequest{}, err
	}
	headers := map[string]string{}
	for _, name := range sortedExpressions(def.Headers) {
		headers[name], err = stringValue("header "+name, def.Headers[name], true)
		if err != nil {
			return model.HTTPRequest{}, err
		}
	}
	return model.HTTPRequest{Method: method, URL: url, Headers: headers, Body: body}, nil
}

func spanMatcher(check model.CheckDefinition, response model.Response, variables map[string]model.SensitiveValue, steps cty.Value) func(model.Span) (bool, error) {
	return func(span model.Span) (bool, error) {
		ctx := expr.SpanContext(span, response, variables, steps)
		if check.Spans.Matching == nil {
			return true, nil
		}
		value, diags := expr.Evaluate(check.Spans.Matching, ctx)
		if diags.HasErrors() {
			return false, fmt.Errorf("span predicate: %s", diags.Error())
		}
		return expr.RequireBool(value)
	}
}

func spanAssertions(check model.CheckDefinition, response model.Response, variables map[string]model.SensitiveValue, steps cty.Value) []poller.SpanAssertion {
	assertions := make([]poller.SpanAssertion, 0, len(check.Spans.Assertions))
	for _, name := range sortedExpressions(check.Spans.Assertions) {
		expression := check.Spans.Assertions[name]
		assertions = append(assertions, poller.SpanAssertion{Name: name, Source: model.Range(expression.Range()), Evaluate: func(span model.Span) (bool, error) {
			value, diags := expr.Evaluate(expression, expr.SpanContext(span, response, variables, steps))
			if diags.HasErrors() {
				return false, fmt.Errorf("span assertion: %s", diags.Error())
			}
			return expr.RequireBool(value)
		}})
	}
	return assertions
}

func traceWindows(datasource *model.DatasourceDefinition, checks []model.CheckDefinition) (time.Duration, time.Duration) {
	window, settle := 30*time.Second, 2*time.Second
	if datasource != nil {
		if datasource.ObservationWindow > 0 {
			window = datasource.ObservationWindow
		}
		if datasource.SettleWindow > 0 {
			settle = datasource.SettleWindow
		}
	}
	for _, check := range checks {
		if check.Spans != nil && check.Spans.ObservationWindow > window {
			window = check.Spans.ObservationWindow
		}
	}
	return window, settle
}

func stepsCTY(values map[string]map[string]cty.Value) cty.Value {
	if len(values) == 0 {
		return cty.EmptyObjectVal
	}
	steps := map[string]cty.Value{}
	for step, outputs := range values {
		object := cty.EmptyObjectVal
		if len(outputs) > 0 {
			object = cty.ObjectVal(outputs)
		}
		steps[step] = cty.ObjectVal(map[string]cty.Value{"outputs": object})
	}
	return cty.ObjectVal(steps)
}

func ctyMapAny(values map[string]cty.Value) map[string]any {
	result := map[string]any{}
	for name, value := range values {
		encoded, err := ctyjson.Marshal(value, value.Type())
		if err == nil {
			var decoded any
			if json.Unmarshal(encoded, &decoded) == nil {
				result[name] = decoded
			}
		}
	}
	return result
}

func sortedExpressions(values map[string]hcl.Expression) []string {
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
func mergeCheck(values *[]model.CheckResult, observed model.CheckResult) {
	for i := range *values {
		if (*values)[i].Name == observed.Name {
			observed.ResponseEvidence = (*values)[i].ResponseEvidence
			*values = append((*values)[:i], append([]model.CheckResult{observed}, (*values)[i+1:]...)...)
			return
		}
	}
	*values = append(*values, observed)
}
func needsDatasource(tests []model.TestDefinition) bool {
	for _, t := range tests {
		for _, s := range t.Steps {
			for _, c := range s.Checks {
				if c.Spans != nil {
					return true
				}
			}
		}
	}
	return false
}
func skippedStepResults(steps []model.StepDefinition) []model.StepResult {
	out := make([]model.StepResult, len(steps))
	for i, s := range steps {
		out[i] = model.StepResult{Name: s.Name, ExecutionID: randomID(), Status: model.StatusSkipped, Checks: []model.CheckResult{}}
	}
	return out
}
func skippedTests(tests []model.TestDefinition) []model.TestResult {
	out := make([]model.TestResult, len(tests))
	for i, t := range tests {
		out[i] = model.TestResult{Name: t.Name, Tags: t.Tags, File: t.File, Status: model.StatusSkipped, Steps: skippedStepResults(t.Steps)}
	}
	return out
}
func cancelledTests(tests []model.TestDefinition) []model.TestResult {
	out := skippedTests(tests)
	if len(out) > 0 {
		out[0].Status = model.StatusCancelled
	}
	return out
}
func diagnostic(code, message string) *model.Diagnostic {
	return &model.Diagnostic{Severity: "error", Code: code, Message: message}
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
func technicalRun(run model.RunResult, code, message string) model.RunResult {
	run.Status = model.StatusError
	run.Summary.Errors = 1
	run.Tests = []model.TestResult{{Name: "run", Status: model.StatusError, Error: diagnostic(code, message), Steps: []model.StepResult{}}}
	return run
}
