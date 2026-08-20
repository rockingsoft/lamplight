// Package discovery finds Lamplight definition files deterministically.
package discovery

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const DefinitionSuffix = ".wick"

// Discover returns regular Lamplight definition files below baseDir in relative
// lexical order.
// Directory symlinks are never followed by filepath.WalkDir; symlinked files
// are ignored as well, making the source tree explicit and reproducible.
func Discover(baseDir string) ([]string, error) {
	baseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(baseDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), DefinitionSuffix) {
			return nil
		}
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for i := range files {
		files[i] = filepath.Join(baseDir, files[i])
	}
	return files, nil
}
