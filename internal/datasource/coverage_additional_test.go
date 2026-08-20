package datasource

import (
	"context"
	"errors"
	"testing"

	"tracetest/internal/model"
)

func TestFakeConnectionAndObservationEdgeCases(t *testing.T) {
	connectionErr := errors.New("unavailable")
	fake := &Fake{ConnectionErr: connectionErr}
	if err := fake.TestConnection(context.Background()); !errors.Is(err, connectionErr) {
		t.Fatalf("TestConnection error = %v", err)
	}
	observation, err := fake.Observe(context.Background(), "empty")
	if err != nil || observation.Found || observation.Valid || observation.Partial || observation.Complete || len(observation.Spans) != 0 || len(observation.Raw) != 0 || fake.Calls != 0 || len(fake.TraceIDs) != 1 {
		t.Fatalf("empty script observation=%#v calls=%d ids=%#v err=%v", observation, fake.Calls, fake.TraceIDs, err)
	}
	observeErr := errors.New("observe failed")
	fake = &Fake{Script: []ScriptedObservation{{Err: observeErr, Observation: model.TraceObservation{Found: true}}}}
	observation, err = fake.Observe(context.Background(), "trace")
	if !errors.Is(err, observeErr) || !observation.Found || fake.Calls != 1 {
		t.Fatalf("script error observation=%#v calls=%d err=%v", observation, fake.Calls, err)
	}
	if _, err := fake.Observe(context.Background(), "trace-2"); !errors.Is(err, observeErr) || fake.Calls != 2 {
		t.Fatalf("repeated script calls=%d err=%v", fake.Calls, err)
	}
}

func TestFakeConnectionSuccess(t *testing.T) {
	if err := (&Fake{}).TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
}
