// Package checks contains deterministic helpers for response checks.
package checks

import (
	"fmt"
	"sort"

	"tracetest/internal/model"
)

// ResponseAssertion evaluates one named condition against a normalized HTTP
// response. Value is retained as evidence even when the assertion fails.
type ResponseAssertion struct {
	Name   string
	Source model.SourceRange
	Check  func(model.Response) (passed bool, value any, err error)
}

// EvaluateResponse evaluates every assertion exactly once. A predicate error
// is returned as technical error; false predicates are normal check evidence.
func EvaluateResponse(response model.Response, assertions []ResponseAssertion) ([]model.AssertionEvidence, error) {
	evidence := make([]model.AssertionEvidence, 0, len(assertions))
	for _, assertion := range assertions {
		if assertion.Name == "" || assertion.Check == nil {
			return nil, fmt.Errorf("response assertion requires name and predicate")
		}
		passed, value, err := assertion.Check(response)
		item := model.AssertionEvidence{Name: assertion.Name, Passed: passed, Value: value, Source: assertion.Source}
		if err != nil {
			item.Error = err.Error()
			evidence = append(evidence, item)
			return evidence, fmt.Errorf("evaluate response assertion %q: %w", assertion.Name, err)
		}
		evidence = append(evidence, item)
	}
	return evidence, nil
}

// AllPassed reports conjunction semantics for response assertion evidence.
func AllPassed(evidence []model.AssertionEvidence) bool {
	for _, item := range evidence {
		if !item.Passed {
			return false
		}
	}
	return true
}

// FromMap converts named predicates to a stable evaluation order. It exists for
// HCL map attributes, whose map iteration order must not affect results.
func FromMap(assertions map[string]func(model.Response) (bool, any, error)) []ResponseAssertion {
	names := make([]string, 0, len(assertions))
	for name := range assertions {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]ResponseAssertion, 0, len(names))
	for _, name := range names {
		result = append(result, ResponseAssertion{Name: name, Check: assertions[name]})
	}
	return result
}
