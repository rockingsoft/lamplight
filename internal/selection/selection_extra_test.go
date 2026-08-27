package selection

import (
	"path/filepath"
	"strings"
	"testing"

	"lamplight/internal/model"
)

func TestSelectRejectsConflictingAndMissingSelectors(t *testing.T) {
	definition := &model.ProjectDefinition{Tests: map[string]model.TestDefinition{"health": {Name: "health", Tags: []string{"smoke"}}}}
	for _, test := range []struct {
		name     string
		selector Selector
		want     string
	}{
		{name: "name and tag", selector: Selector{Name: "health", Tags: []string{"smoke"}}, want: "cannot be combined"},
		{name: "unknown name", selector: Selector{Name: "missing"}, want: `test "missing" not found`},
		{name: "unknown tag", selector: Selector{Tags: []string{"slow"}}, want: "matched no tests"},
		{name: "exclude without selector", selector: Selector{Exclude: true}, want: "requires"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Select(definition, test.selector)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSelectByFileAndExcludeName(t *testing.T) {
	base := t.TempDir()
	definition := &model.ProjectDefinition{BaseDir: base, Tests: map[string]model.TestDefinition{
		"health": {Name: "health", File: filepath.Join(base, "api.wick")},
		"orders": {Name: "orders", File: filepath.Join(base, "flows", "orders.wick")},
		"refund": {Name: "refund", File: filepath.Join(base, "flows", "orders.wick")},
	}}

	tests, err := Select(definition, Selector{Files: []string{"flows/orders.wick"}})
	if err != nil || len(tests) != 2 || tests[0].Name != "orders" || tests[1].Name != "refund" {
		t.Fatalf("file selection=%#v err=%v", tests, err)
	}
	tests, err = Select(definition, Selector{Name: "orders", Exclude: true})
	if err != nil || len(tests) != 2 || tests[0].Name != "health" || tests[1].Name != "refund" {
		t.Fatalf("excluded name selection=%#v err=%v", tests, err)
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
