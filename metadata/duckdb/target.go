// Package duckmeta defines metadata identity for DuckDB database files.
package duckmeta

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sqls-server/sqls/metadata"
)

// Target is the parsed form of a DuckDB catalog target.
type Target struct {
	Path      string
	ReadWrite bool
}

// ParseTarget parses the text after "duckdb://" in a catalog target.
func ParseTarget(raw string) (Target, error) {
	path, query, hasQuery := strings.Cut(raw, "?")
	if strings.TrimSpace(path) == "" {
		return Target{}, fmt.Errorf("missing database file path")
	}
	if !isAbsolutePath(path) {
		return Target{}, fmt.Errorf("database file path must be absolute")
	}

	target := Target{Path: path}
	if !hasQuery {
		return target, nil
	}
	for _, option := range strings.Split(query, "&") {
		key, value, _ := strings.Cut(option, "=")
		if key != "access" {
			return Target{}, fmt.Errorf("unknown option %q", key)
		}
		switch value {
		case "read_write":
			target.ReadWrite = true
		case "read_only":
			target.ReadWrite = false
		default:
			return Target{}, fmt.Errorf("invalid access %q (use read_write)", value)
		}
	}
	return target, nil
}

// CanonicalPath returns a canonical identity for an absolute database path.
// Existing paths resolve symlinks; missing paths are cleaned without creating
// or opening the database.
func CanonicalPath(path string) (string, error) {
	if isWindowsDrivePath(path) || isWindowsUNCPath(path) {
		canonical := path
		if runtime.GOOS == "windows" {
			canonical = filepath.Clean(canonical)
		}
		return strings.ToLower(canonical), nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err == nil {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		abs = resolved
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return filepath.Clean(abs), nil
}

// Config contains the raw DuckDB catalog target.
type Config struct {
	Target string
}

// KeyFor returns the shared metadata identity for cfg's target. Access mode is
// intentionally excluded so read-only and read-write aliases share metadata.
func KeyFor(cfg Config) (metadata.Key, error) {
	target, err := ParseTarget(cfg.Target)
	if err != nil {
		return metadata.Key{}, err
	}
	path, err := CanonicalPath(target.Path)
	if err != nil {
		return metadata.Key{}, err
	}
	return metadata.Key{Engine: "duckdb", URL: "duckdb://" + path}, nil
}

// ScopeFor returns the local visibility scope for a DuckDB file.
func ScopeFor(cfg Config) (metadata.Scope, error) {
	if _, err := ParseTarget(cfg.Target); err != nil {
		return metadata.Scope{}, err
	}
	return metadata.Scope{Principal: "local", TextPolicy: "utf8"}, nil
}

func isAbsolutePath(path string) bool {
	return filepath.IsAbs(path) || isWindowsDrivePath(path) || isWindowsUNCPath(path)
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 3 || path[1] != ':' || (path[2] != '\\' && path[2] != '/') {
		return false
	}
	return (path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')
}

func isWindowsUNCPath(path string) bool {
	return strings.HasPrefix(path, `\\`)
}
