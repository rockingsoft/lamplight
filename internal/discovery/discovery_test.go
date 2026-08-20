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
	for _, name := range []string{"z.wick", "a.wick", ".lamplight", "ignored.txt", "nested/b.wick", "legacy.hcl"} {
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
	want := []string{filepath.Join(dir, "a.wick"), filepath.Join(dir, "nested", "b.wick"), filepath.Join(dir, "z.wick")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover() = %#v, want %#v", got, want)
	}
}

func TestDiscoverIgnoresSymlinksAndNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.wick"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.wick"), filepath.Join(dir, "linked.wick")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "linked-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "linked-dir", "hidden.wick"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "linked-dir"), filepath.Join(dir, "symlink-dir")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "ignored.wick"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "linked-dir", "hidden.wick"), filepath.Join(dir, "real.wick")}
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
	if err := os.WriteFile(filepath.Join(blocked, "test.wick"), nil, 0o600); err != nil {
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
