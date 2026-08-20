package result

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"tracetest/internal/model"
)

func TestAggregateRunAndAllAggregateStates(t *testing.T) {
	tests := []struct {
		name   string
		status model.Status
		want   model.Status
	}{
		{name: "empty", want: model.StatusPassed},
		{name: "failure", status: model.StatusFailed, want: model.StatusFailed},
		{name: "cancelled", status: model.StatusCancelled, want: model.StatusCancelled},
		{name: "error", status: model.StatusError, want: model.StatusError},
		{name: "skipped", status: model.StatusSkipped, want: model.StatusPassed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, _ := Aggregate([]model.TestResult{{Status: test.status}})
			if status != test.want {
				t.Fatalf("Aggregate status = %q, want %q", status, test.want)
			}
		})
	}
	original := model.RunResult{Status: model.StatusError, Tests: []model.TestResult{{Status: model.StatusPassed}}}
	updated := AggregateRun(original)
	if updated.Status != model.StatusPassed || updated.Summary.TestsPassed != 1 || original.Status != model.StatusError {
		t.Fatalf("AggregateRun updated=%#v original=%#v", updated, original)
	}
}

func TestExitCodeCoversNestedFailureAndTerminalStatuses(t *testing.T) {
	cases := []struct {
		name string
		run  model.RunResult
		want int
	}{
		{name: "success", run: model.RunResult{Status: model.StatusPassed}, want: ExitSuccess},
		{name: "test failure", run: model.RunResult{Tests: []model.TestResult{{Status: model.StatusFailed}}}, want: ExitCheckFailure},
		{name: "step failure", run: model.RunResult{Tests: []model.TestResult{{Steps: []model.StepResult{{Status: model.StatusFailed}}}}}, want: ExitCheckFailure},
		{name: "check failure", run: model.RunResult{Tests: []model.TestResult{{Steps: []model.StepResult{{Checks: []model.CheckResult{{Status: model.StatusFailed}}}}}}}, want: ExitCheckFailure},
		{name: "run error", run: model.RunResult{Status: model.StatusError}, want: ExitTechnicalError},
		{name: "cancelled", run: model.RunResult{Status: model.StatusCancelled}, want: ExitTechnicalError},
		{name: "check error", run: model.RunResult{Tests: []model.TestResult{{Steps: []model.StepResult{{Checks: []model.CheckResult{{Status: model.StatusError}}}}}}}, want: ExitTechnicalError},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := ExitCode(test.run); got != test.want {
				t.Fatalf("ExitCode = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRedactorConfigurationAndSensitiveKeyVocabulary(t *testing.T) {
	redactor := NewRedactor("", "x", "long-secret", "secret").WithReplacement("<hidden>")
	if got := redactor.RedactString("long-secret secret"); got != "<hidden> <hidden>" {
		t.Fatalf("overlapping secrets = %q", got)
	}
	if got := NewRedactor("secret").WithReplacement("").RedactString("secret"); got != Redacted {
		t.Fatalf("empty replacement = %q", got)
	}
	for _, key := range []string{"Authorization", "cookie_value", "password", "passwd", "client secret", "credential_id", "api-key", "apikey", "access token", "session_id", "private_key"} {
		if !IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = false", key)
		}
	}
	for _, key := range []string{"name", "status", "body"} {
		if IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = true", key)
		}
	}
	if got := NewRedactor().RedactString("https://example.test/path"); got != "https://example.test/path" {
		t.Fatalf("plain URL = %q", got)
	}
	if got := NewRedactor().RedactString("not a URL: %%%"); got != "not a URL: %%%" {
		t.Fatalf("invalid URL = %q", got)
	}
}

func TestRedactorHandlesJSONBodiesSlicesScalarsAndCopying(t *testing.T) {
	redactor := NewRedactor("needle")
	original := map[string]any{
		"body":   `not-json needle`,
		"items":  []any{"needle", map[string]any{"token": "secret"}},
		"number": 42,
	}
	redacted := redactor.RedactValue(original).(map[string]any)
	if redacted["body"] != "not-json [REDACTED]" {
		t.Fatalf("invalid body = %#v", redacted["body"])
	}
	items := redacted["items"].([]any)
	if items[0] != Redacted || items[1].(map[string]any)["token"] != Redacted || redacted["number"] != 42 {
		t.Fatalf("nested values = %#v", redacted)
	}
	if original["body"] != `not-json needle` {
		t.Fatal("RedactValue mutated input")
	}
	if got := redactor.RedactValue("needle"); got != Redacted {
		t.Fatalf("scalar = %#v", got)
	}
	if got := redactor.RedactValue([]any{"needle"}).([]any)[0]; got != Redacted {
		t.Fatalf("top-level slice = %#v", got)
	}
	valid := redactor.RedactValue(map[string]any{"body": `{"token":"needle","ok":true}`}).(map[string]any)["body"].(string)
	if strings.Contains(valid, "needle") || !strings.Contains(valid, Redacted) {
		t.Fatalf("valid body = %q", valid)
	}
}

func TestMarshalModesAndErrors(t *testing.T) {
	redactor := NewRedactor("secret")
	compact, err := redactor.Marshal(map[string]any{"value": "secret"}, "")
	if err != nil || string(compact) != `{"value":"[REDACTED]"}` {
		t.Fatalf("compact = %s, err = %v", compact, err)
	}
	pretty, err := redactor.Marshal(map[string]any{"value": "secret"}, "  ")
	if err != nil || !strings.Contains(string(pretty), "\n  \"value\"") {
		t.Fatalf("pretty = %s, err = %v", pretty, err)
	}
	if _, err := redactor.Marshal(func() {}, ""); err == nil {
		t.Fatal("unsupported value was marshaled")
	}
	if _, err := redactor.MarshalJSONV1(sampleRun(), "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := (modelTime{value: "x"}).MarshalJSON(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(redactor.RedactValue(nil), nil) {
		t.Fatal("nil value changed")
	}
	var decoded any
	if err := json.Unmarshal(compact, &decoded); err != nil {
		t.Fatal(err)
	}
}
