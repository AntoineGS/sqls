//go:build windows

package duckmeta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalPathResolvesWindowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.duckdb")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias.duckdb")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("Windows symlink creation is unavailable: %v", err)
	}

	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks(target): %v", err)
	}
	want = strings.ToLower(filepath.Clean(want))

	got, err := CanonicalPath(link)
	if err != nil {
		t.Fatalf("CanonicalPath(link): %v", err)
	}
	if got != want {
		t.Fatalf("CanonicalPath(link) = %q, want resolved target %q", got, want)
	}
}
