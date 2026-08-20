// Package result contains the result-level rules shared by output consumers.
package result

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"lamplight/internal/model"
)

const (
	ExitSuccess        = 0
	ExitTechnicalError = 1
	ExitCheckFailure   = 2

	Redacted = "[REDACTED]"
)

// Aggregate returns the final run status and summary implied by tests. A
// technical error takes precedence over check failures, as does cancellation.
func Aggregate(tests []model.TestResult) (model.Status, model.RunSummary) {
	summary := model.RunSummary{TestsTotal: len(tests)}
	status := model.StatusPassed
	hasError := false
	hasCancellation := false
	hasFailure := false

	for _, test := range tests {
		switch test.Status {
		case model.StatusPassed:
			summary.TestsPassed++
		case model.StatusFailed:
			summary.TestsFailed++
			hasFailure = true
		case model.StatusCancelled:
			summary.Errors++
			hasCancellation = true
		case model.StatusError:
			summary.Errors++
			hasError = true
		}
	}

	switch {
	case hasError:
		status = model.StatusError
	case hasCancellation:
		status = model.StatusCancelled
	case hasFailure:
		status = model.StatusFailed
	}
	return status, summary
}

// AggregateRun returns a copy with its status and summary calculated from the
// test results. It deliberately does not mutate the caller's result.
func AggregateRun(run model.RunResult) model.RunResult {
	run.Status, run.Summary = Aggregate(run.Tests)
	return run
}

// ExitCode maps a completed run to the stable CLI contract. It inspects nested
// results as a defensive measure while callers are being integrated.
func ExitCode(run model.RunResult) int {
	hasFailure := false
	for _, test := range run.Tests {
		if code := statusExitCode(test.Status); code == ExitTechnicalError {
			return ExitTechnicalError
		} else if code == ExitCheckFailure {
			hasFailure = true
		}
		for _, step := range test.Steps {
			if code := statusExitCode(step.Status); code == ExitTechnicalError {
				return ExitTechnicalError
			} else if code == ExitCheckFailure {
				hasFailure = true
			}
			for _, check := range step.Checks {
				if code := statusExitCode(check.Status); code == ExitTechnicalError {
					return ExitTechnicalError
				} else if code == ExitCheckFailure {
					hasFailure = true
				}
			}
		}
	}
	if code := statusExitCode(run.Status); code == ExitTechnicalError {
		return ExitTechnicalError
	} else if code == ExitCheckFailure {
		hasFailure = true
	}
	if hasFailure {
		return ExitCheckFailure
	}
	return ExitSuccess
}

func statusExitCode(status model.Status) int {
	switch status {
	case model.StatusError, model.StatusCancelled:
		return ExitTechnicalError
	case model.StatusFailed:
		return ExitCheckFailure
	default:
		return ExitSuccess
	}
}

// Redactor removes known sensitive values and conventional credential fields
// from values which are rendered or persisted.
type Redactor struct {
	values      []string
	replacement string
}

// NewRedactor creates a redactor for runtime-sensitive values. Empty values
// are ignored so an unset secret can never redact arbitrary text.
func NewRedactor(values ...string) Redactor {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return len(filtered[i]) > len(filtered[j]) })
	return Redactor{values: filtered, replacement: Redacted}
}

// WithReplacement returns a copy that uses replacement for redacted values.
func (r Redactor) WithReplacement(replacement string) Redactor {
	if replacement != "" {
		r.replacement = replacement
	}
	return r
}

func (r Redactor) marker() string {
	if r.replacement == "" {
		return Redacted
	}
	return r.replacement
}

// RedactString removes known values and credentials embedded in URLs.
func (r Redactor) RedactString(value string) string {
	for _, secret := range r.values {
		value = strings.ReplaceAll(value, secret, r.marker())
	}
	if parsed, err := url.Parse(value); err == nil && parsed.RawQuery != "" {
		query := parsed.Query()
		changed := false
		for key := range query {
			if IsSensitiveKey(key) {
				query[key] = []string{r.marker()}
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = query.Encode()
			value = parsed.String()
		}
	}
	return value
}

// IsSensitiveKey recognizes credential-bearing keys case-insensitively.
func IsSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", "-"), " ", "-"))
	for _, token := range []string{
		"authorization", "cookie", "password", "passwd", "secret", "credential",
		"api-key", "apikey", "token", "session", "private-key",
	} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

// RedactValue returns a recursively redacted JSON-like value without mutating
// maps or slices supplied by the caller.
func (r Redactor) RedactValue(value any) any {
	return r.redactValue("", value)
}

func (r Redactor) redactValue(key string, value any) any {
	if IsSensitiveKey(key) {
		return r.marker()
	}
	switch typed := value.(type) {
	case string:
		if strings.EqualFold(key, "body") {
			return r.redactJSONDocument(typed)
		}
		return r.RedactString(typed)
	case map[string]any:
		copy := make(map[string]any, len(typed))
		for childKey, child := range typed {
			copy[childKey] = r.redactValue(childKey, child)
		}
		return copy
	case []any:
		copy := make([]any, len(typed))
		for i, child := range typed {
			copy[i] = r.redactValue(key, child)
		}
		return copy
	default:
		return value
	}
}

func (r Redactor) redactJSONDocument(value string) string {
	var document any
	if err := json.Unmarshal([]byte(value), &document); err != nil {
		return r.RedactString(value)
	}
	redacted, err := json.Marshal(r.RedactValue(document))
	if err != nil {
		return r.RedactString(value)
	}
	return string(redacted)
}

// Marshal encodes arbitrary data after it has been converted to JSON and
// recursively redacted. It is suitable for artifact-only auxiliary files.
func (r Redactor) Marshal(value any, indent string) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	if indent == "" {
		return json.Marshal(r.RedactValue(decoded))
	}
	return json.MarshalIndent(r.RedactValue(decoded), "", indent)
}

// MarshalJSONV1 projects the internal result onto the frozen v1 schema before
// redacting it. In particular, StepResult.DurationMS is intentionally omitted:
// it is useful internally but is not a v1 schema field.
func (r Redactor) MarshalJSONV1(run model.RunResult, indent string) ([]byte, error) {
	return r.Marshal(runResultV1From(run), indent)
}

type runResultV1 struct {
	SchemaVersion int                       `json:"schema_version"`
	RunID         string                    `json:"run_id"`
	Status        model.Status              `json:"status"`
	StartedAt     modelTime                 `json:"started_at"`
	DurationMS    int64                     `json:"duration_ms"`
	Selection     map[string]any            `json:"selection,omitempty"`
	Tests         []testResultV1            `json:"tests"`
	Summary       model.RunSummary          `json:"summary"`
	Artifacts     []model.ArtifactReference `json:"artifacts,omitempty"`
}

// modelTime preserves time.Time's JSON representation while keeping the v1
// projection explicit and independent from model's internal layout.
type modelTime struct{ value string }

func (t modelTime) MarshalJSON() ([]byte, error) { return json.Marshal(t.value) }

type testResultV1 struct {
	Name       string            `json:"name"`
	Tags       []string          `json:"tags"`
	File       string            `json:"file"`
	Status     model.Status      `json:"status"`
	DurationMS int64             `json:"duration_ms"`
	Steps      []stepResultV1    `json:"steps"`
	Error      *model.Diagnostic `json:"error,omitempty"`
}

type stepResultV1 struct {
	Name        string                    `json:"name"`
	ExecutionID string                    `json:"execution_id"`
	Status      model.Status              `json:"status"`
	TraceID     string                    `json:"trace_id,omitempty"`
	Request     *model.HTTPRequest        `json:"request,omitempty"`
	Response    *model.Response           `json:"response,omitempty"`
	Outputs     map[string]any            `json:"outputs,omitempty"`
	Checks      []model.CheckResult       `json:"checks"`
	Error       *model.Diagnostic         `json:"error,omitempty"`
	Artifacts   []model.ArtifactReference `json:"artifacts,omitempty"`
}

func runResultV1From(run model.RunResult) runResultV1 {
	tests := make([]testResultV1, len(run.Tests))
	for i, test := range run.Tests {
		steps := make([]stepResultV1, len(test.Steps))
		for j, step := range test.Steps {
			steps[j] = stepResultV1{
				Name: step.Name, ExecutionID: step.ExecutionID, Status: step.Status,
				TraceID: step.TraceID, Request: step.Request, Response: step.Response,
				Outputs: step.Outputs, Checks: step.Checks, Error: step.Error, Artifacts: step.Artifacts,
			}
		}
		tests[i] = testResultV1{Name: test.Name, Tags: test.Tags, File: test.File, Status: test.Status, DurationMS: test.DurationMS, Steps: steps, Error: test.Error}
	}
	return runResultV1{
		SchemaVersion: run.SchemaVersion,
		RunID:         run.RunID,
		Status:        run.Status,
		StartedAt:     modelTime{value: run.StartedAt.Format("2006-01-02T15:04:05.999999999Z07:00")},
		DurationMS:    run.DurationMS,
		Selection:     run.Selection,
		Tests:         tests,
		Summary:       run.Summary,
		Artifacts:     run.Artifacts,
	}
}
