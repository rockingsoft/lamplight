package selection

import (
	"fmt"
	"path/filepath"
	"sort"

	"lamplight/internal/model"
)

type Selector struct {
	Name    string
	Tags    []string
	Files   []string
	Exclude bool
}

func Select(def *model.ProjectDefinition, selector Selector) ([]model.TestDefinition, error) {
	selectorKinds := 0
	if selector.Name != "" {
		selectorKinds++
	}
	if len(selector.Tags) > 0 {
		selectorKinds++
	}
	if len(selector.Files) > 0 {
		selectorKinds++
	}
	if selectorKinds > 1 {
		return nil, fmt.Errorf("test name, --file, and --tag selectors cannot be combined")
	}
	if selector.Exclude && selectorKinds == 0 {
		return nil, fmt.Errorf("--exclude requires a test name, --file, or --tag selector")
	}

	matched := make(map[string]bool)
	for name, test := range def.Tests {
		switch {
		case selector.Name != "":
			matched[name] = name == selector.Name
		case len(selector.Tags) > 0:
			matched[name] = containsAny(test.Tags, selector.Tags)
		case len(selector.Files) > 0:
			matched[name] = matchesAnyFile(def, test.File, selector.Files)
		default:
			matched[name] = true
		}
	}
	if selector.Name != "" && !matched[selector.Name] {
		return nil, fmt.Errorf("test %q not found", selector.Name)
	}
	if selectorKinds > 0 && countMatches(matched) == 0 {
		return nil, fmt.Errorf("selection matched no tests")
	}

	var tests []model.TestDefinition
	for name, test := range def.Tests {
		selected := matched[name]
		if selector.Exclude {
			selected = !selected
		}
		if selected {
			tests = append(tests, test)
		}
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
	if len(tests) == 0 {
		return nil, fmt.Errorf("selection matched no tests")
	}
	return tests, nil
}

func containsAny(values, wanted []string) bool {
	for _, value := range values {
		for _, candidate := range wanted {
			if value == candidate {
				return true
			}
		}
	}
	return false
}

func matchesAnyFile(def *model.ProjectDefinition, testFile string, wanted []string) bool {
	testFile = filepath.Clean(testFile)
	for _, candidate := range wanted {
		candidate = filepath.Clean(candidate)
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(def.BaseDir, candidate)
		}
		if testFile == filepath.Clean(candidate) {
			return true
		}
	}
	return false
}

func countMatches(matches map[string]bool) int {
	count := 0
	for _, matched := range matches {
		if matched {
			count++
		}
	}
	return count
}
