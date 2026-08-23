package model

import (
	"context"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

type Status string

const (
	StatusPassed    Status = "passed"
	StatusFailed    Status = "failed"
	StatusError     Status = "error"
	StatusCancelled Status = "cancelled"
	StatusSkipped   Status = "skipped"
)

type SourceRange struct {
	File        string `json:"file,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	StartColumn int    `json:"start_column,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
	EndColumn   int    `json:"end_column,omitempty"`
}

func Range(r hcl.Range) SourceRange {
	return SourceRange{File: r.Filename, StartLine: r.Start.Line, StartColumn: r.Start.Column, EndLine: r.End.Line, EndColumn: r.End.Column}
}

type Diagnostic struct {
	Severity          string      `json:"severity"`
	Code              string      `json:"code"`
	Message           string      `json:"message"`
	File              string      `json:"file,omitempty"`
	Range             SourceRange `json:"range,omitempty"`
	Suggestion        string      `json:"suggestion,omitempty"`
	SensitiveRedacted bool        `json:"sensitive_redacted,omitempty"`
}

type VariableDefinition struct {
	Name       string
	Type       string
	Default    cty.Value
	HasDefault bool
	Sensitive  bool
	Range      SourceRange
}

type HTTPClientConfig struct {
	Timeout              time.Duration
	FollowRedirects      bool
	MaxRequestBodyBytes  int64
	MaxResponseBodyBytes int64
	Proxy                string
	TLSSkipVerify        bool
}

func DefaultHTTPClientConfig() HTTPClientConfig {
	return HTTPClientConfig{Timeout: 30 * time.Second, FollowRedirects: true, MaxRequestBodyBytes: 1 << 20, MaxResponseBodyBytes: 10 << 20}
}

type DatasourceDefinition struct {
	Kind              string
	Endpoint          hcl.Expression
	Headers           map[string]hcl.Expression
	BearerToken       hcl.Expression
	TLSSkipVerify     bool
	ObservationWindow time.Duration
	SettleWindow      time.Duration
	PollingInterval   time.Duration
}

type InstrumentationDefinition struct {
	Kind               string
	Image              string
	OpenPorts          []int
	ContextPropagation string
	Range              SourceRange
}

// SupportedDatasourceKinds is the public set of tracing backends inherited
// from Tracetest. SaaS backends use Lamplight's local OTLP ingestion adapter;
// the remaining backends are queried directly.
var SupportedDatasourceKinds = []string{
	"awsxray", "azureappinsights", "dash0", "datadog", "dynatrace",
	"elasticapm", "honeycomb", "instana", "jaeger", "lightstep",
	"newrelic", "opensearch", "otlp", "signalfx", "signoz",
	"sumologic", "tempo",
}

func IsSupportedDatasourceKind(kind string) bool {
	for _, supported := range SupportedDatasourceKinds {
		if kind == supported {
			return true
		}
	}
	return false
}

type HTTPRequestDefinition struct {
	Method  hcl.Expression
	URL     hcl.Expression
	Headers map[string]hcl.Expression
	Body    hcl.Expression
}

type TriggerKind string

const (
	TriggerHTTP             TriggerKind = "http"
	TriggerGRPC             TriggerKind = "grpc"
	TriggerGraphQL          TriggerKind = "graphql"
	TriggerKafka            TriggerKind = "kafka"
	TriggerTraceID          TriggerKind = "traceid"
	TriggerCypress          TriggerKind = "cypress"
	TriggerPlaywright       TriggerKind = "playwright"
	TriggerArtillery        TriggerKind = "artillery"
	TriggerK6               TriggerKind = "k6"
	TriggerPlaywrightEngine TriggerKind = "playwrightengine"
)

type TriggerDefinition struct {
	Kind       TriggerKind
	Attributes map[string]hcl.Expression
}

type TriggerRequest struct {
	Kind       TriggerKind    `json:"kind"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type TriggerExecutor interface {
	Execute(context.Context, TriggerRequest, HTTPClientConfig, *TestTraceContext) (Response, error)
}

type QuantityRule struct {
	Kind  string
	Value int
}

type SpanCheckDefinition struct {
	Matching          hcl.Expression
	Assertions        map[string]hcl.Expression
	Rule              QuantityRule
	ObservationWindow time.Duration
}

type CheckDefinition struct {
	Name     string
	Response map[string]hcl.Expression
	Spans    *SpanCheckDefinition
	Range    SourceRange
}

type StepDefinition struct {
	Name    string
	HTTP    HTTPRequestDefinition
	Trigger TriggerDefinition
	Outputs map[string]hcl.Expression
	Checks  []CheckDefinition
	Range   SourceRange
}

type TestDefinition struct {
	Name    string
	Tags    []string
	Steps   []StepDefinition
	Outputs map[string]hcl.Expression
	File    string
	Range   SourceRange
}

type ProjectDefinition struct {
	ConfigPath      string
	BaseDir         string
	Output          string
	HTTPClient      HTTPClientConfig
	HTTPProxy       hcl.Expression
	Datasource      *DatasourceDefinition
	Instrumentation *InstrumentationDefinition
	Variables       map[string]VariableDefinition
	Tests           map[string]TestDefinition
	DefaultTarget   string
	Targets         map[string]TargetDefinition
}

type TargetDefinition struct {
	Name       string
	Runtime    string
	Variables  map[string]hcl.Expression
	Compose    DockerComposeTarget
	Kubernetes KubernetesTarget
	Range      SourceRange
}

type DockerComposeTarget struct {
	Project  string
	Services []string
}

type KubernetesTarget struct {
	Context        string
	Namespace      string
	ServiceAccount string
}

type SensitiveValue struct {
	Value     cty.Value
	Sensitive bool
}

type Project struct {
	Definition *ProjectDefinition
	Variables  map[string]SensitiveValue
	Tests      []TestDefinition
	Datasource DataStore
	HTTPClient HTTPClientConfig
}

type TraceID string

type TestTraceContext struct {
	TraceID    TraceID
	SpanID     string
	TraceFlags byte
	TraceState string
}

func (t TestTraceContext) TraceParent() string {
	return "00-" + string(t.TraceID) + "-" + t.SpanID + "-01"
}

type Span struct {
	TraceID       string         `json:"trace_id"`
	SpanID        string         `json:"span_id"`
	ParentSpanID  string         `json:"parent_span_id,omitempty"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind,omitempty"`
	Status        string         `json:"status,omitempty"`
	StatusMessage string         `json:"status_message,omitempty"`
	Duration      time.Duration  `json:"duration"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Resource      map[string]any `json:"resource,omitempty"`
}

type TraceObservation struct {
	Found       bool
	Valid       bool
	Partial     bool
	Complete    bool
	Spans       []Span
	Raw         []byte
	Fingerprint string
}

type ObservationError struct {
	Err        error
	Retriable  bool
	RetryAfter time.Duration
}

func (e *ObservationError) Error() string { return e.Err.Error() }
func (e *ObservationError) Unwrap() error { return e.Err }

type DataStore interface {
	TestConnection(context.Context) error
	Observe(context.Context, TraceID) (TraceObservation, error)
}

type TraceContextFactory interface {
	New() (TestTraceContext, error)
}

type Response struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	JSON       any                 `json:"json,omitempty"`
}

type HTTPRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type AssertionEvidence struct {
	Name   string      `json:"name"`
	Passed bool        `json:"passed"`
	Value  any         `json:"value,omitempty"`
	Error  string      `json:"error,omitempty"`
	Source SourceRange `json:"source,omitempty"`
}

type SpanEvidence struct {
	Rule       QuantityRule        `json:"rule"`
	MatchCount int                 `json:"match_count"`
	Reason     string              `json:"reason,omitempty"`
	Assertions []AssertionEvidence `json:"assertions,omitempty"`
}

type CheckResult struct {
	Name             string              `json:"name"`
	Status           Status              `json:"status"`
	ResponseEvidence []AssertionEvidence `json:"response_evidence,omitempty"`
	SpanEvidence     *SpanEvidence       `json:"span_evidence,omitempty"`
	Reason           string              `json:"reason,omitempty"`
}

type StepResult struct {
	Name        string              `json:"name"`
	ExecutionID string              `json:"execution_id"`
	Status      Status              `json:"status"`
	DurationMS  int64               `json:"duration_ms"`
	TraceID     string              `json:"trace_id,omitempty"`
	Request     *HTTPRequest        `json:"request,omitempty"`
	Trigger     *TriggerRequest     `json:"trigger,omitempty"`
	Response    *Response           `json:"response,omitempty"`
	Outputs     map[string]any      `json:"outputs,omitempty"`
	Checks      []CheckResult       `json:"checks"`
	Error       *Diagnostic         `json:"error,omitempty"`
	Artifacts   []ArtifactReference `json:"artifacts,omitempty"`
}

type TestResult struct {
	Name       string       `json:"name"`
	Tags       []string     `json:"tags"`
	File       string       `json:"file"`
	Status     Status       `json:"status"`
	DurationMS int64        `json:"duration_ms"`
	Steps      []StepResult `json:"steps"`
	Error      *Diagnostic  `json:"error,omitempty"`
}

type RunSummary struct {
	TestsTotal  int `json:"tests_total"`
	TestsPassed int `json:"tests_passed"`
	TestsFailed int `json:"tests_failed"`
	Errors      int `json:"errors"`
}

type ArtifactReference struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type RunResult struct {
	SchemaVersion int                 `json:"schema_version"`
	RunID         string              `json:"run_id"`
	Status        Status              `json:"status"`
	StartedAt     time.Time           `json:"started_at"`
	DurationMS    int64               `json:"duration_ms"`
	Selection     map[string]any      `json:"selection,omitempty"`
	Tests         []TestResult        `json:"tests"`
	Summary       RunSummary          `json:"summary"`
	Artifacts     []ArtifactReference `json:"artifacts,omitempty"`
}
