package checks

import (
	"errors"
	"testing"

	"lamplight/internal/model"
)

func TestEvaluateResponsePreservesEveryFalseAssertion(t *testing.T) {
	calls := 0
	evidence, err := EvaluateResponse(model.Response{StatusCode: 201}, []ResponseAssertion{{Name: "created", Check: func(model.Response) (bool, any, error) { calls++; return true, 201, nil }}, {Name: "body", Check: func(model.Response) (bool, any, error) { calls++; return false, nil, nil }}})
	if err != nil || calls != 2 || AllPassed(evidence) || len(evidence) != 2 || evidence[1].Passed {
		t.Fatalf("evidence=%#v calls=%d err=%v", evidence, calls, err)
	}
}

func TestEvaluateResponseReturnsPredicateErrors(t *testing.T) {
	evidence, err := EvaluateResponse(model.Response{}, []ResponseAssertion{{Name: "bad", Check: func(model.Response) (bool, any, error) { return false, nil, errors.New("bad type") }}})
	if err == nil || len(evidence) != 1 || evidence[0].Error != "bad type" {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
}
