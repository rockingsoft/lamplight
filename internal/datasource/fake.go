// Package datasource provides datasource fakes for engine and poller tests.
package datasource

import (
	"context"
	"sync"

	"lamplight/internal/model"
)

// ScriptedObservation is one result returned by Fake.Observe. Once the script
// is exhausted, the final entry is repeated, modeling a stable datasource.
type ScriptedObservation struct {
	Observation model.TraceObservation
	Err         error
}

// Fake is a deterministic, concurrency-safe DataStore implementation.
type Fake struct {
	ConnectionErr error
	Script        []ScriptedObservation

	mu       sync.Mutex
	Calls    int
	TraceIDs []model.TraceID
}

func (f *Fake) TestConnection(context.Context) error { return f.ConnectionErr }

func (f *Fake) Observe(_ context.Context, traceID model.TraceID) (model.TraceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.TraceIDs = append(f.TraceIDs, traceID)
	if len(f.Script) == 0 {
		return model.TraceObservation{}, nil
	}
	index := f.Calls
	f.Calls++
	if index >= len(f.Script) {
		index = len(f.Script) - 1
	}
	entry := f.Script[index]
	return entry.Observation, entry.Err
}

var _ model.DataStore = (*Fake)(nil)
