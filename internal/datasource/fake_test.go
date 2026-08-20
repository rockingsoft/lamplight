package datasource

import (
	"context"
	"testing"

	"lamplight/internal/model"
)

func TestFakeRepeatsFinalScriptEntry(t *testing.T) {
	fake := &Fake{Script: []ScriptedObservation{{Observation: model.TraceObservation{Found: false}}, {Observation: model.TraceObservation{Found: true, Complete: true}}}}
	for range 3 {
		observation, err := fake.Observe(context.Background(), "trace")
		if err != nil || (fake.Calls > 1 && !observation.Found) {
			t.Fatalf("observation=%#v err=%v", observation, err)
		}
	}
	if len(fake.TraceIDs) != 3 {
		t.Fatalf("calls=%d", len(fake.TraceIDs))
	}
}
