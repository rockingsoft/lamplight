package discovery

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestDiscoverSortedAndConfigExcluded(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z.hcl", "a.hcl", ".tracetest.hcl", "ignored.txt", "nested/b.hcl"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "a.hcl"), filepath.Join(dir, "nested", "b.hcl"), filepath.Join(dir, "z.hcl")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover() = %#v, want %#v", got, want)
	}
}

func TestDiscoverIgnoresSymlinksAndNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.hcl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.hcl"), filepath.Join(dir, "linked.hcl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "linked-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "linked-dir", "hidden.hcl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "linked-dir"), filepath.Join(dir, "symlink-dir")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "ignored.hcl"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "linked-dir", "hidden.hcl"), filepath.Join(dir, "real.hcl")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover() = %#v, want %#v", got, want)
	}
}

func TestDiscoverReturnsWalkError(t *testing.T) {
	if _, err := Discover(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Discover() returned nil error for a missing base directory")
	}
}

func TestDiscoverPropagatesPermissionWalkError(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "test.hcl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0o755)
	if _, err := Discover(dir); err == nil {
		t.Skip("the test process can traverse mode-000 directories")
	}
}
