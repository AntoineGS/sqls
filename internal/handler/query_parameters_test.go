package handler

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestParameterDiscoveryDoesNotNeedCatalogOrDatabaseIO(t *testing.T) {
	s := NewServer()
	defer s.worker.Stop()
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Alias: "nrf01", Host: "test", Path: "db.ib"}
	s.connGeneration = 7
	s.files["file:///query.sql"] = &File{Text: "SELECT :ID, :id FROM T"}
	result, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command: CommandGetQueryParameters, Arguments: []interface{}{"file:///query.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := result.(lsp.QueryParameterDiscovery)
	if !got.Supported || len(got.Parameters) != 1 || got.ConnectionGeneration != 7 {
		t.Fatalf("%+v", got)
	}
}

// discoverParams runs getQueryParameters against a freshly built Server with
// the given state, for tests that only care about the resulting identity.
func discoverParams(t *testing.T, cfg *database.DBConfig, dbConn *database.DBConnection, generation int, text string, rng *lsp.Range) lsp.QueryParameterDiscovery {
	t.Helper()
	s := NewServer()
	defer s.worker.Stop()
	s.dbConn = dbConn
	s.curDBCfg = cfg
	s.connGeneration = generation
	const uri = "file:///query.sql"
	s.files[uri] = &File{Text: text}
	result, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{uri},
		Range:     rng,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.(lsp.QueryParameterDiscovery)
}

func TestParameterIdentityChangedFieldsProduceDifferentConnectionKey(t *testing.T) {
	baseCfg := func() *database.DBConfig {
		return &database.DBConfig{
			Driver:    dialect.DatabaseDriverInterBase,
			Alias:     "nrf01",
			Host:      "test",
			Path:      "db.ib",
			User:      "alice",
			DBName:    "maindb",
			InterBase: &database.InterBaseConfig{Role: "READERS"},
		}
	}
	dbConn := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, DatabaseName: "maindb"}
	const text = "SELECT 1"

	base := discoverParams(t, baseCfg(), dbConn, 1, text, nil)

	tests := []struct {
		name   string
		mutate func(*database.DBConfig)
	}{
		{"alias", func(c *database.DBConfig) { c.Alias = "centrale" }},
		{"host", func(c *database.DBConfig) { c.Host = "other" }},
		{"path", func(c *database.DBConfig) { c.Path = "other.ib" }},
		{"user", func(c *database.DBConfig) { c.User = "bob" }},
		{"role", func(c *database.DBConfig) { c.InterBase.Role = "WRITERS" }},
		{"database", func(c *database.DBConfig) { c.DBName = "otherdb" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg()
			tt.mutate(cfg)
			got := discoverParams(t, cfg, dbConn, 1, text, nil)
			if got.ConnectionKey == base.ConnectionKey {
				t.Errorf("ConnectionKey unchanged after changing %s", tt.name)
			}
		})
	}
}

func TestParameterIdentityPasswordDoesNotAffectConnectionKey(t *testing.T) {
	dbConn := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	cfg1 := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "test", Path: "db.ib", User: "alice", Passwd: "secret1"}
	cfg2 := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "test", Path: "db.ib", User: "alice", Passwd: "secret2"}

	got1 := discoverParams(t, cfg1, dbConn, 1, "SELECT 1", nil)
	got2 := discoverParams(t, cfg2, dbConn, 1, "SELECT 1", nil)
	if got1.ConnectionKey != got2.ConnectionKey {
		t.Errorf("ConnectionKey changed with password: %q vs %q", got1.ConnectionKey, got2.ConnectionKey)
	}
}

func TestParameterIdentityDSNCredentialsDoNotAffectConnectionKey(t *testing.T) {
	dbConn := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	cfg1 := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, DataSourceName: "alice:secret1@test/3050:db.ib"}
	cfg2 := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, DataSourceName: "alice:secret2@test/3050:db.ib"}

	got1 := discoverParams(t, cfg1, dbConn, 1, "SELECT 1", nil)
	got2 := discoverParams(t, cfg2, dbConn, 1, "SELECT 1", nil)
	if got1.ConnectionKey != got2.ConnectionKey {
		t.Errorf("ConnectionKey changed with the DSN's embedded credentials: %q vs %q", got1.ConnectionKey, got2.ConnectionKey)
	}
}

func TestParameterIdentityGenerationDoesNotAffectConnectionKey(t *testing.T) {
	dbConn := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "test", Path: "db.ib", User: "alice"}

	got1 := discoverParams(t, cfg, dbConn, 3, "SELECT 1", nil)
	got2 := discoverParams(t, cfg, dbConn, 9, "SELECT 1", nil)
	if got1.ConnectionKey != got2.ConnectionKey {
		t.Errorf("ConnectionKey changed with generation: %q vs %q", got1.ConnectionKey, got2.ConnectionKey)
	}
	if got1.ConnectionGeneration == got2.ConnectionGeneration {
		t.Errorf("ConnectionGeneration = %d for both, want the requested generations reflected", got1.ConnectionGeneration)
	}
}

func TestParameterIdentityDialectChangesQueryKey(t *testing.T) {
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "test", Path: "db.ib", User: "alice"}
	dbConn1 := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1}
	dbConn3 := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}

	got1 := discoverParams(t, cfg, dbConn1, 1, "SELECT 1", nil)
	got3 := discoverParams(t, cfg, dbConn3, 1, "SELECT 1", nil)
	if got1.QueryKey == got3.QueryKey {
		t.Errorf("QueryKey unchanged across dialect 1 and dialect 3")
	}
}

func TestParameterIdentitySameSelectionSharesQueryKeyAcrossDocuments(t *testing.T) {
	const selection = "SELECT :ID FROM T"
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "test", Path: "db.ib", User: "alice"}
	dbConn := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}

	s := NewServer()
	defer s.worker.Stop()
	s.dbConn = dbConn
	s.curDBCfg = cfg
	s.files["file:///a.sql"] = &File{Text: selection}
	s.files["file:///b.sql"] = &File{Text: "-- unrelated preface\n" + selection}

	resultA, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{"file:///a.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resultB, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{"file:///b.sql"},
		Range: &lsp.Range{
			Start: lsp.Position{Line: 1, Character: 0},
			End:   lsp.Position{Line: 1, Character: len(selection)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	gotA := resultA.(lsp.QueryParameterDiscovery)
	gotB := resultB.(lsp.QueryParameterDiscovery)
	if gotA.QueryKey != gotB.QueryKey {
		t.Errorf("QueryKey differs for identical selected SQL: %q vs %q", gotA.QueryKey, gotB.QueryKey)
	}
	if gotA.DocumentKey == gotB.DocumentKey {
		t.Errorf("DocumentKey unexpectedly equal across two different documents")
	}
}

func TestParameterDiscoveryNonInterBaseReturnsUnsupported(t *testing.T) {
	s := NewServer()
	defer s.worker.Stop()
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverPostgreSQL}
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, Host: "test"}
	s.files["file:///query.sql"] = &File{Text: "SELECT :ID FROM T"}

	result, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{"file:///query.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := result.(lsp.QueryParameterDiscovery)
	if got.Supported {
		t.Errorf("Supported = true, want false for a non-InterBase connection")
	}
	if len(got.Parameters) != 0 {
		t.Errorf("Parameters = %+v, want none", got.Parameters)
	}
}

func TestParameterDiscoveryWithNoConnectionReturnsUnsupported(t *testing.T) {
	s := NewServer()
	defer s.worker.Stop()
	s.files["file:///query.sql"] = &File{Text: "SELECT :ID FROM T"}

	result, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{"file:///query.sql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := result.(lsp.QueryParameterDiscovery)
	if got.Supported {
		t.Errorf("Supported = true, want false with no open connection")
	}
}

func TestParameterDiscoveryUnknownURIFails(t *testing.T) {
	s := NewServer()
	defer s.worker.Stop()
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "test", Path: "db.ib", User: "alice"}

	_, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{"file:///missing.sql"},
	})
	if err == nil {
		t.Fatal("want error for an unknown document URI")
	}
}

func TestParameterDiscoveryInvalidRangeFails(t *testing.T) {
	const text = "SELECT 1\nSELECT 2"
	tests := []struct {
		name string
		rng  lsp.Range
	}{
		{"negative start line", lsp.Range{Start: lsp.Position{Line: -1, Character: 0}, End: lsp.Position{Line: 0, Character: 1}}},
		{"negative character", lsp.Range{Start: lsp.Position{Line: 0, Character: -1}, End: lsp.Position{Line: 0, Character: 1}}},
		{"reversed", lsp.Range{Start: lsp.Position{Line: 1, Character: 0}, End: lsp.Position{Line: 0, Character: 0}}},
		{"line past document", lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 5, Character: 0}}},
		{"character past line", lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 100}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer()
			defer s.worker.Stop()
			s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
			s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "test", Path: "db.ib", User: "alice"}
			s.files["file:///query.sql"] = &File{Text: text}

			rng := tt.rng
			_, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
				Command:   CommandGetQueryParameters,
				Arguments: []interface{}{"file:///query.sql"},
				Range:     &rng,
			})
			if err == nil {
				t.Fatal("want error for an invalid range")
			}
		})
	}
}

func TestParameterIdentityNilConfigReturnsError(t *testing.T) {
	s := NewServer()
	defer s.worker.Stop()
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	s.curDBCfg = nil
	s.files["file:///query.sql"] = &File{Text: "SELECT 1"}

	_, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{"file:///query.sql"},
	})
	if err == nil {
		t.Fatal("want error for a nil connection config on an InterBase connection")
	}
}

func TestParameterDiscoveryOverJSONRPCTouchesNoRepository(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	recorder := installQueryParametersRecorder(t)
	tx.addWorkspaceConfig(t, stubQueryParametersConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT :ID FROM T")

	var got lsp.QueryParameterDiscovery
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	if !got.Supported {
		t.Fatalf("Supported = false, want true: %+v", got)
	}
	if len(got.Parameters) != 1 || got.Parameters[0].Key != "ID" {
		t.Fatalf("Parameters = %+v, want one parameter keyed ID", got.Parameters)
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if got.ConnectionKey == "" {
		t.Errorf("ConnectionKey is empty, want a populated hash")
	}
	if calls := recorder.recordedCalls(); len(calls) != 0 {
		t.Errorf("repository served %d parameterized calls, want 0 for discovery", len(calls))
	}
}
