package engine

import (
	"time"

	"lamplight/internal/model"
)

// ProgressEvent describes a stable execution milestone. Callers may use these
// events for live output without coupling execution to a specific renderer.
type ProgressEvent struct {
	Kind              ProgressEventKind
	RunID             string
	TestName          string
	StepName          string
	Trigger           model.TriggerKind
	Status            model.Status
	DurationMS        int64
	TestsTotal        int
	ObservationWindow time.Duration
	Attempt           int
	SpanCount         int
	MetricCount       int
	Found             bool
	Complete          bool
	RetryError        string
	Checks            []ProgressCheck
	StatusCode        int
	RemotePhase       string
	RemoteExecution   string
	RemoteLogURI      string
	CompletedShards   int
	TotalShards       int
	Elapsed           time.Duration
}

type ProgressCheck struct {
	Name       string
	MatchCount int
	Status     model.Status
	Reason     string
	Rule       model.QuantityRule
}

type ProgressEventKind string

const (
	ProgressRunStarted          ProgressEventKind = "run_started"
	ProgressDatasourceStarted   ProgressEventKind = "datasource_started"
	ProgressDatasourceCompleted ProgressEventKind = "datasource_completed"
	ProgressTestStarted         ProgressEventKind = "test_started"
	ProgressTestCompleted       ProgressEventKind = "test_completed"
	ProgressStepStarted         ProgressEventKind = "step_started"
	ProgressStepCompleted       ProgressEventKind = "step_completed"
	ProgressTriggerStarted      ProgressEventKind = "trigger_started"
	ProgressTriggerCompleted    ProgressEventKind = "trigger_completed"
	ProgressRemoteTrigger       ProgressEventKind = "remote_trigger"
	ProgressTracePolling        ProgressEventKind = "trace_polling"
	ProgressTraceObserved       ProgressEventKind = "trace_observed"
	ProgressMetricPolling       ProgressEventKind = "metric_polling"
	ProgressMetricObserved      ProgressEventKind = "metric_observed"
)

type ProgressFunc func(ProgressEvent)
