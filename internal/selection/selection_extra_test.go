package selection

import (
	"strings"
	"testing"

	"tracetest/internal/model"
)

func TestSelectRejectsConflictingAndMissingSelectors(t *testing.T) {
	definition := &model.ProjectDefinition{Tests: map[string]model.TestDefinition{"health": {Name: "health", Tags: []string{"smoke"}}}}
	for _, test := range []struct {
		name     string
		selector Selector
		want     string
	}{
		{name: "name and tag", selector: Selector{Name: "health", Tag: "smoke"}, want: "cannot be combined"},
		{name: "unknown name", selector: Selector{Name: "missing"}, want: `test "missing" not found`},
		{name: "unknown tag", selector: Selector{Tag: "slow"}, want: "matched no tests"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Select(definition, test.selector)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSelectByNameReturnsTheNamedTest(t *testing.T) {
	definition := &model.ProjectDefinition{Tests: map[string]model.TestDefinition{
		"health": {Name: "health"},
		"other":  {Name: "other"},
	}}
	tests, err := Select(definition, Selector{Name: "health"})
	if err != nil || len(tests) != 1 || tests[0].Name != "health" {
		t.Fatalf("tests=%#v err=%v", tests, err)
	}
}
