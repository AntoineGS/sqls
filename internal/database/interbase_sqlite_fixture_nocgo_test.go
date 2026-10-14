//go:build !cgo

package database

import "testing"

func requireInterBaseSQLiteFixture(t *testing.T) {
	t.Helper()
	t.Skip("catalog SQL fixture requires SQLite cgo; pure metadata/store and driver-stub tests remain enabled")
}
