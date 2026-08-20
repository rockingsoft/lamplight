package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

func TestRangeCopiesHCLRange(t *testing.T) {
	rangeValue := hcl.Range{Filename: "test.hcl", Start: hcl.Pos{Line: 2, Column: 3}, End: hcl.Pos{Line: 4, Column: 5}}
	got := Range(rangeValue)
	want := SourceRange{File: "test.hcl", StartLine: 2, StartColumn: 3, EndLine: 4, EndColumn: 5}
	if got != want {
		t.Fatalf("Range()=%#v, want %#v", got, want)
	}
}

func TestDefaultHTTPClientConfig(t *testing.T) {
	got := DefaultHTTPClientConfig()
	if got.Timeout != 30*time.Second || !got.FollowRedirects || got.MaxRequestBodyBytes != 1<<20 || got.MaxResponseBodyBytes != 10<<20 {
		t.Fatalf("unexpected defaults: %#v", got)
	}
}

func TestTraceParent(t *testing.T) {
	got := (TestTraceContext{TraceID: "trace", SpanID: "span"}).TraceParent()
	if got != "00-trace-span-01" {
		t.Fatalf("TraceParent()=%q", got)
	}
}

func TestObservationErrorImplementsErrorAndUnwrap(t *testing.T) {
	want := errors.New("observe failed")
	observationError := &ObservationError{Err: want, Retriable: true, RetryAfter: time.Second}
	if observationError.Error() != want.Error() || !errors.Is(observationError, want) || !errors.Is(observationError.Unwrap(), want) {
		t.Fatalf("unexpected observation error: %v", observationError)
	}
}

func TestInterfacesRemainUsableBySimpleFakes(t *testing.T) {
	var _ DataStore = modelDataStoreFake{}
	var _ TraceContextFactory = traceFactoryFake{}
	_ = context.Background()
	_ = cty.StringVal("value")
}

type modelDataStoreFake struct{}

func (modelDataStoreFake) TestConnection(context.Context) error { return nil }
func (modelDataStoreFake) Observe(context.Context, TraceID) (TraceObservation, error) {
	return TraceObservation{}, nil
}

type traceFactoryFake struct{}

func (traceFactoryFake) New() (TestTraceContext, error) { return TestTraceContext{}, nil }
