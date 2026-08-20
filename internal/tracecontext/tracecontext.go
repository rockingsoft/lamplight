// Package tracecontext creates and propagates the W3C trace context used to
// correlate a step request with its datasource observation.
package tracecontext

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"lamplight/internal/model"
)

const (
	traceParentHeader   = "traceparent"
	traceStateHeader    = "tracestate"
	lamplightTraceState = "lamplight=true"
)

// Factory produces independent, sampled W3C trace contexts.
type Factory struct{}

// NewFactory returns a cryptographically secure trace context factory.
func NewFactory() Factory { return Factory{} }

// New creates a version 00 traceparent with non-zero 128-bit trace and 64-bit
// span IDs.  It deliberately does not create an SDK span or exporter.
func (Factory) New() (model.TestTraceContext, error) {
	traceID, err := randomHex(16)
	if err != nil {
		return model.TestTraceContext{}, err
	}
	spanID, err := randomHex(8)
	if err != nil {
		return model.TestTraceContext{}, err
	}
	return model.TestTraceContext{TraceID: model.TraceID(traceID), SpanID: spanID, TraceFlags: 1, TraceState: lamplightTraceState}, nil
}

func randomHex(size int) (string, error) {
	bytes := make([]byte, size)
	for {
		if _, err := rand.Read(bytes); err != nil {
			return "", err
		}
		allZero := true
		for _, value := range bytes {
			if value != 0 {
				allZero = false
				break
			}
		}
		if !allZero {
			return hex.EncodeToString(bytes), nil
		}
	}
}

// Inject removes user-controlled W3C propagation values and injects context.
// A nil context only removes those headers.
func Inject(headers http.Header, trace *model.TestTraceContext) {
	headers.Del(traceParentHeader)
	headers.Del(traceStateHeader)
	if trace == nil {
		return
	}
	headers.Set(traceParentHeader, trace.TraceParent())
	if trace.TraceState != "" {
		headers.Set(traceStateHeader, trace.TraceState)
	}
}

// ParseTraceParent validates a W3C version 00 traceparent and returns its
// trace context. It is useful when testing an HTTP receiver's propagation.
func ParseTraceParent(value string) (model.TestTraceContext, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return model.TestTraceContext{}, errors.New("invalid traceparent")
	}
	for _, part := range parts {
		if _, err := hex.DecodeString(part); err != nil {
			return model.TestTraceContext{}, errors.New("invalid traceparent")
		}
	}
	if allZeroHex(parts[1]) || allZeroHex(parts[2]) {
		return model.TestTraceContext{}, errors.New("invalid traceparent")
	}
	flags, _ := hex.DecodeString(parts[3])
	return model.TestTraceContext{TraceID: model.TraceID(parts[1]), SpanID: parts[2], TraceFlags: flags[0]}, nil
}

func allZeroHex(value string) bool {
	for _, char := range value {
		if char != '0' {
			return false
		}
	}
	return true
}
