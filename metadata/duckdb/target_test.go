package duckmeta

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sqls-server/sqls/metadata"
)

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    Target
		wantErr string
	}{
		{raw: "/data/x.duckdb", want: Target{Path: "/data/x.duckdb"}},
		{raw: "/data/x.duckdb?access=read_write", want: Target{Path: "/data/x.duckdb", ReadWrite: true}},
		{raw: "/data/x.duckdb?access=read_only", want: Target{Path: "/data/x.duckdb"}},
		{raw: `C:\Data\x.duckdb`, want: Target{Path: `C:\Data\x.duckdb`}},
		{raw: `C:/Data/x.duckdb?access=read_write`, want: Target{Path: `C:/Data/x.duckdb`, ReadWrite: true}},
		{raw: `\\srv\share\x.duckdb`, want: Target{Path: `\\srv\share\x.duckdb`}},
		{raw: "/tmp/a b%20#c.duckdb", want: Target{Path: "/tmp/a b%20#c.duckdb"}},
		{raw: "", wantErr: "missing database file path"},
		{raw: "   ", wantErr: "missing database file path"},
		{raw: "?access=read_write", wantErr: "missing database file path"},
		{raw: "x.duckdb", wantErr: "database file path must be absolute"},
		{raw: `data\x.duckdb`, wantErr: "database file path must be absolute"},
		{raw: "/x.duckdb?mode=rw", wantErr: `unknown option "mode"`},
		{raw: "/x.duckdb?access=rw", wantErr: `invalid access "rw" (use read_write)`},
	} {
		got, err := ParseTarget(tc.raw)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("ParseTarget(%q) error = %v, want %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseTarget(%q) = %+v, %v; want %+v", tc.raw, got, err, tc.want)
		}
	}
}

func TestKeyForIgnoresAccessAndResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "database.duckdb")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias.duckdb")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}

	linkKey, err := KeyFor(Config{Target: link + "?access=read_write"})
	if err != nil {
		t.Fatalf("KeyFor(link): %v", err)
	}
	fileKey, err := KeyFor(Config{Target: file})
	if err != nil {
		t.Fatalf("KeyFor(file): %v", err)
	}
	canonical, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	want := metadata.Key{Engine: "duckdb", URL: "duckdb://" + canonical}
	if linkKey != fileKey || linkKey != want {
		t.Fatalf("KeyFor(link) = %+v, KeyFor(file) = %+v; want %+v", linkKey, fileKey, want)
	}
}

func TestKeyForMissingFileIsCleanedAbsolute(t *testing.T) {
	got, err := KeyFor(Config{Target: "/tmp/x/../y.duckdb"})
	if err != nil {
		t.Fatalf("KeyFor(): %v", err)
	}
	want := metadata.Key{Engine: "duckdb", URL: "duckdb:///tmp/y.duckdb"}
	if got != want {
		t.Fatalf("KeyFor() = %+v, want %+v", got, want)
	}
}

func TestCanonicalPathWindowsLowerCase(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{path: `C:\Data\X.DuckDB`, want: `c:\data\x.duckdb`},
		{path: `\\Srv\Share\A.duckdb`, want: `\\srv\share\a.duckdb`},
	} {
		got, err := CanonicalPath(tc.path)
		if err != nil || got != tc.want {
			t.Errorf("CanonicalPath(%q) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
}

func TestScopeFor(t *testing.T) {
	scope, err := ScopeFor(Config{Target: "/data/x.duckdb"})
	if err != nil {
		t.Fatalf("ScopeFor(): %v", err)
	}
	if scope.Principal != "local" || scope.TextPolicy != "utf8" {
		t.Fatalf("ScopeFor() = %+v, want principal local and text policy utf8", scope)
	}

	if _, err := ScopeFor(Config{Target: "relative.duckdb"}); err == nil || err.Error() != "database file path must be absolute" {
		t.Fatalf("ScopeFor(invalid target) error = %v, want absolute-path validation error", err)
	}
}
