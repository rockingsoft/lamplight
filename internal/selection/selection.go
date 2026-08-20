package selection

import (
	"fmt"
	"sort"

	"lamplight/internal/model"
)

type Selector struct {
	Name string
	Tag  string
}

func Select(def *model.ProjectDefinition, selector Selector) ([]model.TestDefinition, error) {
	if selector.Name != "" && selector.Tag != "" {
		return nil, fmt.Errorf("test name and --tag cannot be combined")
	}
	var tests []model.TestDefinition
	if selector.Name != "" {
		t, ok := def.Tests[selector.Name]
		if !ok {
			return nil, fmt.Errorf("test %q not found", selector.Name)
		}
		tests = append(tests, t)
	} else {
		for _, t := range def.Tests {
			if selector.Tag == "" || contains(t.Tags, selector.Tag) {
				tests = append(tests, t)
			}
		}
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
	if len(tests) == 0 {
		return nil, fmt.Errorf("selection matched no tests")
	}
	return tests, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
