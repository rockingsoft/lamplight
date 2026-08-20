package checks

import (
	"errors"
	"reflect"
	"testing"

	"tracetest/internal/model"
)

func TestEvaluateResponseRejectsMalformedAssertions(t *testing.T) {
	for _, assertion := range []ResponseAssertion{{}, {Name: "missing predicate"}} {
		if evidence, err := EvaluateResponse(model.Response{}, []ResponseAssertion{assertion}); err == nil || evidence != nil {
			t.Fatalf("assertion=%#v evidence=%#v err=%v", assertion, evidence, err)
		}
	}
	if evidence, err := EvaluateResponse(model.Response{}, nil); err != nil || evidence == nil || len(evidence) != 0 {
		t.Fatalf("empty assertions evidence=%#v err=%v", evidence, err)
	}
	if !AllPassed(nil) || !AllPassed([]model.AssertionEvidence{}) {
		t.Fatal("empty evidence should pass conjunction")
	}
}

func TestFromMapSortsNamesAndPreservesSourceValues(t *testing.T) {
	called := []string{}
	input := map[string]func(model.Response) (bool, any, error){
		"zeta":  func(model.Response) (bool, any, error) { called = append(called, "zeta"); return true, 1, nil },
		"alpha": func(model.Response) (bool, any, error) { called = append(called, "alpha"); return true, 2, nil },
	}
	assertions := FromMap(input)
	if got := []string{assertions[0].Name, assertions[1].Name}; !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("names = %#v", got)
	}
	evidence, err := EvaluateResponse(model.Response{}, assertions)
	if err != nil || !reflect.DeepEqual(called, []string{"alpha", "zeta"}) || !AllPassed(evidence) {
		t.Fatalf("evidence=%#v called=%#v err=%v", evidence, called, err)
	}
	if evidence[0].Value != 2 || evidence[1].Value != 1 {
		t.Fatalf("values = %#v", evidence)
	}
}

func TestEvaluateResponseStopsAfterPredicateError(t *testing.T) {
	calls := 0
	evidence, err := EvaluateResponse(model.Response{}, []ResponseAssertion{
		{Name: "bad", Check: func(model.Response) (bool, any, error) { calls++; return false, "value", errors.New("broken") }},
		{Name: "never", Check: func(model.Response) (bool, any, error) { calls++; return true, nil, nil }},
	})
	if err == nil || calls != 1 || len(evidence) != 1 || evidence[0].Value != "value" || evidence[0].Error != "broken" {
		t.Fatalf("evidence=%#v calls=%d err=%v", evidence, calls, err)
	}
}
