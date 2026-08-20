package tracetestmigrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type FileResult struct {
	Source, Destination string
	Warnings            []string
}

// Run migrates a YAML file or every .yaml/.yml file below a directory into a
// Lamplight project rooted at outputDir.
func Run(input, outputDir string, force bool) ([]FileResult, error) {
	files, err := inputFiles(input)
	if err != nil {
		return nil, err
	}
	type pending struct {
		source, destination string
		result              Result
	}
	pendingFiles := make([]pending, 0, len(files))
	seen := map[string]string{}
	for _, source := range files {
		contents, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", source, err)
		}
		result, err := Convert(contents)
		if err != nil {
			return nil, fmt.Errorf("migrate %s: %w", source, err)
		}
		name := fileName(result.Name) + ".wick"
		destination := filepath.Join(outputDir, "lamplight", name)
		if prior, exists := seen[destination]; exists {
			return nil, fmt.Errorf("%s and %s both map to %s", prior, source, destination)
		}
		seen[destination] = source
		if !force {
			if _, err := os.Stat(destination); err == nil {
				return nil, fmt.Errorf("refusing to overwrite %s (use --force)", destination)
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
		pendingFiles = append(pendingFiles, pending{source: source, destination: destination, result: result})
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "lamplight"), 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	configPath := filepath.Join(outputDir, ".lamplight")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte("project {\n  base_dir = \"./lamplight\"\n  output   = \"pretty\"\n}\n"), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", configPath, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat %s: %w", configPath, err)
	}
	results := make([]FileResult, 0, len(pendingFiles))
	for _, item := range pendingFiles {
		if err := os.WriteFile(item.destination, item.result.HCL, 0o644); err != nil {
			return results, fmt.Errorf("write %s: %w", item.destination, err)
		}
		results = append(results, FileResult{Source: item.source, Destination: item.destination, Warnings: item.result.Warnings})
	}
	return results, nil
}

func inputFiles(input string) ([]string, error) {
	info, err := os.Stat(input)
	if err != nil {
		return nil, fmt.Errorf("stat input: %w", err)
	}
	if !info.IsDir() {
		if !isYAML(input) {
			return nil, fmt.Errorf("input file must end in .yaml or .yml")
		}
		return []string{input}, nil
	}
	var files []string
	err = filepath.WalkDir(input, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() && isYAML(path) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan input: %w", err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("input directory contains no .yaml or .yml files")
	}
	return files, nil
}

func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

var unsafeFileCharacters = regexp.MustCompile(`[^a-z0-9_-]+`)

func fileName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = unsafeFileCharacters.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "migrated-test"
	}
	return name
}
