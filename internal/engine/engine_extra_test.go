package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/datasource"
	"lamplight/internal/model"
)

type traceFactory struct {
	context model.TestTraceContext
	err     error
}

func (f traceFactory) New() (model.TestTraceContext, error) {
	if f.err != nil {
		return model.TestTraceContext{}, f.err
	}
	return f.context, nil
}

type recordingHTTP struct {
	response model.Response
	err      error
	requests []model.HTTPRequest
	cancel   context.CancelFunc
}

func (f *recordingHTTP) Execute(ctx context.Context, request model.HTTPRequest, _ model.HTTPClientConfig, _ *model.TestTraceContext) (model.Response, error) {
	f.requests = append(f.requests, request)
	if f.cancel != nil {
		f.cancel()
	}
	return f.response, f.err
}

type engineClock struct{ now time.Time }

func (c *engineClock) Now() time.Time { return c.now }
func (c *engineClock) After(duration time.Duration) <-chan time.Time {
	c.now = c.now.Add(duration)
	result := make(chan time.Time, 1)
	result <- c.now
	return result
}

func engineProject(step model.StepDefinition) *model.Project {
	definition := &model.ProjectDefinition{HTTPClient: model.DefaultHTTPClientConfig()}
	return &model.Project{Definition: definition, Variables: map[string]model.SensitiveValue{}, Tests: []model.TestDefinition{{Name: "test", Steps: []model.StepDefinition{step}}}, HTTPClient: definition.HTTPClient}
}

func spanStep(t *testing.T, rule model.QuantityRule, matching string) model.StepDefinition {
	t.Helper()
	return model.StepDefinition{
		Name:   "trace",
		HTTP:   model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test"`)},
		Checks: []model.CheckDefinition{{Name: "span check", Spans: &model.SpanCheckDefinition{Matching: parseExpr(t, matching), Rule: rule}}},
	}
}

func TestRunRejectsInvalidProjectsAndDatasourceFailures(t *testing.T) {
	tests := []struct {
		name string
		call func() model.RunResult
		code string
	}{
		{name: "nil project", call: func() model.RunResult { return (&Engine{HTTP: fakeHTTP{}}).Run(context.Background(), nil) }, code: ""},
		{name: "nil definition", call: func() model.RunResult { return (&Engine{HTTP: fakeHTTP{}}).Run(context.Background(), &model.Project{}) }, code: ""},
		{name: "nil HTTP executor", call: func() model.RunResult {
			return (&Engine{}).Run(context.Background(), &model.Project{Definition: &model.ProjectDefinition{}})
		}, code: ""},
		{name: "datasource required", call: func() model.RunResult {
			project := engineProject(spanStep(t, model.QuantityRule{Kind: "exactly", Value: 0}, `true`))
			return (&Engine{HTTP: fakeHTTP{}}).Run(context.Background(), project)
		}, code: "datasource_required"},
		{name: "datasource connection", call: func() model.RunResult {
			project := engineProject(spanStep(t, model.QuantityRule{Kind: "exactly", Value: 0}, `true`))
			project.Datasource = &datasource.Fake{ConnectionErr: errors.New("offline")}
			return (&Engine{HTTP: fakeHTTP{}}).Run(context.Background(), project)
		}, code: "datasource_connection"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := test.call()
			if run.Status != model.StatusError || run.Summary.Errors != 1 {
				t.Fatalf("run=%#v", run)
			}
			if test.code != "" && (len(run.Tests) != 1 || run.Tests[0].Error == nil || run.Tests[0].Error.Code != test.code) {
				t.Fatalf("run=%#v", run)
			}
		})
	}
}

func TestRunCancellationAndTechnicalErrorSkipRemainingTests(t *testing.T) {
	t.Run("cancelled tests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		project := engineProject(model.StepDefinition{Name: "first"})
		project.Tests = []model.TestDefinition{{Name: "first", Steps: []model.StepDefinition{{Name: "one"}}}, {Name: "second", Steps: []model.StepDefinition{{Name: "two"}}}}
		run := (&Engine{HTTP: fakeHTTP{}}).Run(ctx, project)
		if run.Status != model.StatusCancelled || len(run.Tests) != 2 || run.Tests[0].Status != model.StatusCancelled || run.Tests[1].Status != model.StatusSkipped {
			t.Fatalf("run=%#v", run)
		}
	})

	t.Run("technical error skips later tests", func(t *testing.T) {
		step := model.StepDefinition{Name: "one", HTTP: model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test"`)}, Checks: []model.CheckDefinition{{Name: "needs trace", Spans: &model.SpanCheckDefinition{Rule: model.QuantityRule{Kind: "exactly", Value: 0}}}}}
		project := engineProject(step)
		project.Datasource = &datasource.Fake{}
		project.Tests = []model.TestDefinition{{Name: "first", Steps: []model.StepDefinition{step}}, {Name: "second", Steps: []model.StepDefinition{{Name: "later"}}}}
		run := (&Engine{HTTP: fakeHTTP{}}).Run(context.Background(), project)
		if run.Status != model.StatusError || len(run.Tests) != 2 || run.Tests[0].Status != model.StatusError || run.Tests[1].Status != model.StatusSkipped {
			t.Fatalf("run=%#v", run)
		}
	})
}

func TestRunExecutesVariablesOutputsAndSpanChecks(t *testing.T) {
	first := model.StepDefinition{Name: "first", HTTP: model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test/start"`)}, Outputs: map[string]hcl.Expression{"next": parseExpr(t, `response.body`)}}
	second := spanStep(t, model.QuantityRule{Kind: "at_least", Value: 1}, `span.name == "target"`)
	second.HTTP.URL = parseExpr(t, `steps.first.outputs.next`)
	second.HTTP.Headers = map[string]hcl.Expression{"X-Value": parseExpr(t, `var.VALUE`)}
	project := engineProject(first)
	project.Tests[0].Steps = []model.StepDefinition{first, second}
	project.Variables = map[string]model.SensitiveValue{"VALUE": {Value: cty.StringVal("secret")}}
	project.Datasource = &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: []model.Span{{Name: "target"}}}}}}
	http := &recordingHTTP{response: model.Response{StatusCode: 200, Body: "http://example.test/next", Headers: map[string][]string{}}}
	run := (&Engine{HTTP: http, TraceFactory: traceFactory{context: model.TestTraceContext{TraceID: "trace-id"}}, Clock: &engineClock{now: time.Unix(0, 0)}}).Run(context.Background(), project)
	if run.Status != model.StatusPassed || len(http.requests) != 2 || http.requests[1].URL != "http://example.test/next" || http.requests[1].Headers["X-Value"] != "secret" {
		t.Fatalf("run=%#v requests=%#v", run, http.requests)
	}
	if run.Tests[0].Steps[0].Outputs["next"] != "http://example.test/next" || run.Tests[0].Steps[1].Checks[0].Status != model.StatusPassed {
		t.Fatalf("steps=%#v", run.Tests[0].Steps)
	}
}

func TestRunStepErrorAndCancellationBranches(t *testing.T) {
	base := model.StepDefinition{Name: "step", HTTP: model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test"`)}}
	tests := []struct {
		name  string
		setup func(*model.Project, *recordingHTTP)
		want  model.Status
		code  string
	}{
		{name: "cancelled before request", setup: func(project *model.Project, _ *recordingHTTP) {}, want: model.StatusCancelled},
		{name: "missing trace factory", setup: func(project *model.Project, _ *recordingHTTP) { project.Datasource = &datasource.Fake{} }, want: model.StatusError, code: "trace_context"},
		{name: "trace factory error", setup: func(project *model.Project, _ *recordingHTTP) {
			project.Datasource = &datasource.Fake{}
			projectDefinitionFactory(project, errors.New("factory"))
		}, want: model.StatusError, code: "trace_context"},
		{name: "request evaluation", setup: func(project *model.Project, _ *recordingHTTP) {
			project.Tests[0].Steps[0].HTTP.Method = parseExpr(t, `var.MISSING`)
		}, want: model.StatusError, code: "request_evaluation"},
		{name: "HTTP execution", setup: func(_ *model.Project, http *recordingHTTP) { http.err = errors.New("network") }, want: model.StatusError, code: "http_execution"},
		{name: "output evaluation", setup: func(project *model.Project, _ *recordingHTTP) {
			project.Tests[0].Steps[0].Outputs = map[string]hcl.Expression{"bad": parseExpr(t, `unknown`)}
		}, want: model.StatusError, code: "output_evaluation"},
		{name: "check evaluation", setup: func(project *model.Project, _ *recordingHTTP) {
			project.Tests[0].Steps[0].Checks = []model.CheckDefinition{{Name: "bad", Response: map[string]hcl.Expression{"value": parseExpr(t, `response.missing == true`)}}}
		}, want: model.StatusError, code: "check_evaluation"},
		{name: "check type", setup: func(project *model.Project, _ *recordingHTTP) {
			project.Tests[0].Steps[0].Checks = []model.CheckDefinition{{Name: "bad", Response: map[string]hcl.Expression{"value": parseExpr(t, `response.status_code`)}}}
		}, want: model.StatusError, code: "check_type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := engineProject(base)
			http := &recordingHTTP{response: model.Response{StatusCode: 200, Headers: map[string][]string{}}}
			ctx := context.Background()
			if test.name == "cancelled before request" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			test.setup(project, http)
			if test.name == "trace factory error" {
				projectDefinitionFactory(project, errors.New("factory"))
			}
			eng := &Engine{HTTP: http}
			if factory, ok := projectFactory(project); ok {
				eng.TraceFactory = factory
			}
			result, _ := eng.runStep(ctx, project, project.Tests[0].Steps[0], map[string]map[string]cty.Value{})
			if result.Status != test.want || (test.code != "" && (result.Error == nil || result.Error.Code != test.code)) {
				t.Fatalf("result=%#v", result)
			}
		})
	}

	t.Run("cancellation during observation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		project := engineProject(spanStep(t, model.QuantityRule{Kind: "at_least", Value: 1}, `true`))
		project.Datasource = &datasource.Fake{Script: []datasource.ScriptedObservation{{Observation: model.TraceObservation{Found: true, Valid: true}}}}
		http := &recordingHTTP{response: model.Response{Headers: map[string][]string{}}, cancel: cancel}
		result, _ := (&Engine{HTTP: http, TraceFactory: traceFactory{context: model.TestTraceContext{TraceID: "trace"}}, Clock: &engineClock{now: time.Unix(0, 0)}}).runStep(ctx, project, project.Tests[0].Steps[0], map[string]map[string]cty.Value{})
		if result.Status != model.StatusCancelled {
			t.Fatalf("result=%#v", result)
		}
	})
}

// These helpers keep the table tests focused on the branch under test while
// still providing the engine with the only injectable dependency it needs.
var factoryByProject = map[*model.Project]model.TraceContextFactory{}

func projectDefinitionFactory(project *model.Project, err error) {
	factoryByProject[project] = traceFactory{err: err}
}

func projectFactory(project *model.Project) (model.TraceContextFactory, bool) {
	factory, ok := factoryByProject[project]
	delete(factoryByProject, project)
	return factory, ok
}

func TestEvaluateRequestAndInternalHelpers(t *testing.T) {
	request, err := evaluateRequest(model.HTTPRequestDefinition{
		Method: parseExpr(t, `"POST"`), URL: parseExpr(t, `steps.first.outputs.url`), Body: parseExpr(t, `var.BODY`),
		Headers: map[string]hcl.Expression{"X-Test": parseExpr(t, `var.HEADER`)},
	}, map[string]model.SensitiveValue{"BODY": {Value: cty.StringVal("body")}, "HEADER": {Value: cty.StringVal("header")}}, stepsCTY(map[string]map[string]cty.Value{"first": {"url": cty.StringVal("http://example.test")}}))
	if err != nil || request.Method != "POST" || request.URL != "http://example.test" || request.Body != "body" || request.Headers["X-Test"] != "header" {
		t.Fatalf("request=%#v err=%v", request, err)
	}
	for _, definition := range []model.HTTPRequestDefinition{{URL: parseExpr(t, `"url"`)}, {Method: parseExpr(t, `"GET"`)}, {Method: parseExpr(t, `1`), URL: parseExpr(t, `"url"`)}, {Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"url"`), Headers: map[string]hcl.Expression{"X": parseExpr(t, `1`)}}} {
		if _, err := evaluateRequest(definition, nil, cty.EmptyObjectVal); err == nil {
			t.Fatalf("expected request evaluation error for %#v", definition)
		}
	}
	if got := stepsCTY(nil); !got.RawEquals(cty.EmptyObjectVal) {
		t.Fatalf("empty steps=%#v", got)
	}
	if got := stepsCTY(map[string]map[string]cty.Value{"empty": {}, "values": {"count": cty.NumberIntVal(2)}}); !got.IsKnown() {
		t.Fatalf("steps value is unknown: %#v", got)
	}
	if got := ctyMapAny(map[string]cty.Value{"number": cty.NumberIntVal(2), "text": cty.StringVal("ok")}); got["text"] != "ok" {
		t.Fatalf("cty map=%#v", got)
	}
	if !needsDatasource([]model.TestDefinition{{Steps: []model.StepDefinition{{Checks: []model.CheckDefinition{{Spans: &model.SpanCheckDefinition{}}}}}}}) || needsDatasource(nil) {
		t.Fatal("unexpected datasource requirement")
	}
	if got := diagnostic("code", "message"); got.Code != "code" || got.Message != "message" {
		t.Fatalf("diagnostic=%#v", got)
	}
	gotWindow, gotSettle := traceWindows(nil, nil)
	if gotWindow != 30*time.Second || gotSettle != 2*time.Second {
		t.Fatalf("default windows=%v,%v", gotWindow, gotSettle)
	}
	window, settle := traceWindows(&model.DatasourceDefinition{ObservationWindow: time.Second, SettleWindow: 3 * time.Second}, []model.CheckDefinition{{Spans: &model.SpanCheckDefinition{ObservationWindow: 5 * time.Second}}})
	if window != 5*time.Second || settle != 3*time.Second {
		t.Fatalf("windows=%v,%v", window, settle)
	}
}

func TestSpanMatcherAndCheckMerging(t *testing.T) {
	check := model.CheckDefinition{Name: "span", Spans: &model.SpanCheckDefinition{Matching: parseExpr(t, `span.name == "wanted"`), Assertions: map[string]hcl.Expression{"kind": parseExpr(t, `span.kind == "server"`)}}}
	matcher := spanMatcher(check, model.Response{}, nil, cty.EmptyObjectVal)
	matched, err := matcher(model.Span{Name: "wanted", Kind: "server"})
	if err != nil || !matched {
		t.Fatalf("matched=%t err=%v", matched, err)
	}
	matched, err = matcher(model.Span{Name: "other", Kind: "server"})
	if err != nil || matched {
		t.Fatalf("unmatched=%t err=%v", matched, err)
	}
	bad := model.CheckDefinition{Name: "bad", Spans: &model.SpanCheckDefinition{Matching: parseExpr(t, `response.status_code`)}}
	if _, err := spanMatcher(bad, model.Response{}, nil, cty.EmptyObjectVal)(model.Span{}); err == nil {
		t.Fatal("expected span predicate error")
	}
	checks := []model.CheckResult{{Name: "span", Status: model.StatusPassed, ResponseEvidence: []model.AssertionEvidence{{Name: "response"}}}}
	mergeCheck(&checks, model.CheckResult{Name: "span", Status: model.StatusFailed, Reason: "count"})
	if checks[0].Status != model.StatusFailed || len(checks[0].ResponseEvidence) != 1 {
		t.Fatalf("merged checks=%#v", checks)
	}
	mergeCheck(&checks, model.CheckResult{Name: "new", Status: model.StatusPassed})
	if len(checks) != 2 {
		t.Fatalf("checks=%#v", checks)
	}
}

func TestSkippedAndCancelledResultHelpers(t *testing.T) {
	tests := []model.TestDefinition{{Name: "one", Steps: []model.StepDefinition{{Name: "step"}}}, {Name: "two"}}
	skipped := skippedTests(tests)
	if len(skipped) != 2 || skipped[0].Status != model.StatusSkipped || skipped[0].Steps[0].Status != model.StatusSkipped {
		t.Fatalf("skipped=%#v", skipped)
	}
	cancelled := cancelledTests(tests)
	if cancelled[0].Status != model.StatusCancelled || cancelled[1].Status != model.StatusSkipped {
		t.Fatalf("cancelled=%#v", cancelled)
	}
	if !strings.HasPrefix(randomID(), "") {
		t.Fatal("randomID returned no value")
	}
}
