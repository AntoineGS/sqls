package handler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
)

// newDefinitionServer installs a temporary host snapshot store. Never let
// navigation acceptance tests write into the developer's user cache.
func newDefinitionServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer()
	t.Cleanup(func() { _ = server.Stop() })
	server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	server.curDBCfg = &database.DBConfig{
		Alias:          "local_ib",
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: "localhost/3050:/db/app.ib",
	}
	server.snapshots = newTestSnapshotStore(t)
	return server
}

func snapshotRootIsEmpty(t *testing.T, store *sourceSnapshotStore) bool {
	t.Helper()
	files := 0
	err := filepath.WalkDir(store.root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal("WalkDir:", err)
	}
	return files == 0
}
