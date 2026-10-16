package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/language"
)

func TestLanguageRequestCarriesCacheEpoch(t *testing.T) {
	req := languageRequest(editorSnapshot{CacheEpoch: 42})
	if req.Document.CacheEpoch != 42 {
		t.Fatalf("document cache epoch = %d, want 42", req.Document.CacheEpoch)
	}
}

func TestNavigationResultCurrentRejectsDidSaveAtSameVersion(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	uri := "file:///same-version-save.sql"
	text := "SELECT old_name FROM t"
	if err := s.openFileAtVersion(uri, "sql", text, 7); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	result, err := language.NewService().Rename(context.Background(), languageRequest(snapshot), language.Position{Line: 0, UTF16Column: 9}, "new_name")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.updateFileAtRevision(uri, "SELECT new_name FROM t", snapshot.Revision)
	if err != nil || !updated {
		t.Fatalf("updateFileAtRevision = (%v, %v), want update", updated, err)
	}
	live, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if live.Version != snapshot.Version || live.Revision == snapshot.Revision {
		t.Fatalf("didSave snapshot version/revision = %d/%d, captured %d/%d", live.Version, live.Revision, snapshot.Version, snapshot.Revision)
	}
	if err := s.navigationResultCurrent(context.Background(), snapshot, result.Stamp); !errors.Is(err, language.ErrStale) {
		t.Fatalf("same-version didSave fence error = %v, want %v", err, language.ErrStale)
	}
}

func TestNavigationResultCurrentRejectsConfigOnlyDialectChange(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverInterBase, Dialect: 1}}}
	uri := "file:///dialect-config.sql"
	text := "ALTER PROCEDURE p AS BEGIN END"
	if err := s.openFileAtVersion(uri, "sql", text, 3); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	result, err := language.NewService().References(context.Background(), languageRequest(snapshot), language.Position{Line: 0, UTF16Column: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.WSCfg.Connections[0].Dialect = 3
	s.stateMu.Unlock()
	live, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if live.Generation != snapshot.Generation || live.Variant == snapshot.Variant || live.MetadataIncarnation != snapshot.MetadataIncarnation {
		t.Fatalf("fixture did not isolate dialect change: before=%+v after=%+v", snapshot, live)
	}
	if err := s.navigationResultCurrent(context.Background(), snapshot, result.Stamp); !errors.Is(err, language.ErrStale) {
		t.Fatalf("config-only dialect fence error = %v, want %v", err, language.ErrStale)
	}
}
