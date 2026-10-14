//go:build integration && interbase && cgo && linux && amd64

package database

import (
	"os"
	"testing"
)

// This entry point is deliberately separate from read-only live qualification.
// Only the disposable runner supplies these four explicit write-fixture inputs.
// Reuse the already qualified actual native delta/independent-full matrix,
// including source isolation, quoted identities, view visibility fallback and D1.
func TestInterBaseWatchFixtureParity(t *testing.T) {
	target := os.Getenv("SQLS_WATCH_FIXTURE_TARGET")
	if target == "" {
		t.Skip("explicit disposable watcher fixture required")
	}
	d1 := os.Getenv("SQLS_WATCH_FIXTURE_D1_TARGET")
	user := os.Getenv("SQLS_WATCH_FIXTURE_USER")
	password := os.Getenv("SQLS_WATCH_FIXTURE_PASSWORD")
	if d1 == "" || user == "" || password == "" {
		t.Fatal("incomplete disposable watcher fixture environment")
	}
	t.Setenv("SQLS_WATCH_DISPOSABLE", "1")
	t.Setenv("SQLS_WATCH_DISPOSABLE_DATABASE", target)
	t.Setenv("SQLS_WATCH_DISPOSABLE_DATABASE_D1", d1)
	t.Setenv("SQLS_WATCH_DISPOSABLE_USER", user)
	t.Setenv("SQLS_WATCH_DISPOSABLE_PASSWORD", password)
	TestWatchedChangesDisposableNativeParity(t)
}
