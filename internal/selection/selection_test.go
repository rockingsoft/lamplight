package selection

import (
	"testing"

	"lamplight/internal/model"
)

func TestSelectOrdersAndFilters(t *testing.T) {
	def := &model.ProjectDefinition{Tests: map[string]model.TestDefinition{
		"z": {Name: "z", Tags: []string{"slow"}},
		"a": {Name: "a", Tags: []string{"smoke"}},
	}}
	tests, err := Select(def, Selector{})
	if err != nil || len(tests) != 2 || tests[0].Name != "a" {
		t.Fatalf("unexpected selection: %#v %v", tests, err)
	}
	tests, err = Select(def, Selector{Tags: []string{"smoke"}})
	if err != nil || len(tests) != 1 || tests[0].Name != "a" {
		t.Fatalf("unexpected tag selection: %#v %v", tests, err)
	}
}

func TestSelectSupportsMultipleTagsAndExclusion(t *testing.T) {
	def := &model.ProjectDefinition{Tests: map[string]model.TestDefinition{
		"fast":  {Name: "fast", Tags: []string{"smoke"}},
		"slow":  {Name: "slow", Tags: []string{"slow"}},
		"other": {Name: "other", Tags: []string{"regression"}},
	}}
	tests, err := Select(def, Selector{Tags: []string{"smoke", "slow"}, Exclude: true})
	if err != nil || len(tests) != 1 || tests[0].Name != "other" {
		t.Fatalf("unexpected excluded tag selection: %#v %v", tests, err)
	}
}
