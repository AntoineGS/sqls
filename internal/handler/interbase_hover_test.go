package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

// interBaseHoverCache is the shared handler-side catalog fixture. CITY is both
// a real table and a cached relation; MYVIEW is a view that is also in
// SchemaTables, as the contract guarantees; MYTRIGGER has an empty Event,
// which is the normal case until the driver-side accessor spec lands; MYUDF
// has one unrenderable argument type.
func interBaseHoverCache(t *testing.T) *database.DBCache {
	t.Helper()
	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Table: "CITY", Name: "ID"}, Type: "INTEGER"},
		{ColumnBase: database.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"CITY", "MYVIEW"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}
	cache, err := database.NewDBCacheUpdater(repo).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatal("GenerateDBCachePrimary:", err)
	}
	cache.Catalog = &database.CatalogCache{
		Procedures: map[string]*database.ProcedureDesc{
			"MYPROC": {
				Name:   "MYPROC",
				Source: sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
				InputParameters: []*database.ProcedureParameterDesc{
					{Name: "IN_AMOUNT", Position: 0, Direction: database.ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
				},
				OutputParameters: []*database.ProcedureParameterDesc{
					{Name: "OUT_TOTAL", Position: 0, Direction: database.ParameterOutput, Type: "INTEGER"},
				},
			},
		},
		Views: map[string]*database.ViewDesc{
			"MYVIEW": {
				Name:       "MYVIEW",
				ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
				Columns:    []*database.ColumnDesc{{ColumnBase: database.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"}},
			},
		},
		Generators: map[string]*database.GeneratorDesc{
			"GEN_ORDER_ID": {Name: "GEN_ORDER_ID", ID: sql.NullInt64{Int64: 3, Valid: true}},
		},
		Functions: map[string]*database.FunctionDesc{
			"MYUDF": {
				Name:       "MYUDF",
				ReturnType: "DOUBLE PRECISION",
				Arguments: []*database.FunctionArgumentDesc{
					{Position: sql.NullInt64{Int64: 1, Valid: true}, Type: ""},
				},
			},
		},
		Triggers: map[string]*database.TriggerDesc{
			"MYTRIGGER": {
				Name:         "MYTRIGGER",
				RelationName: sql.NullString{String: "CITY", Valid: true},
				Event:        "",
				Active:       sql.NullBool{Bool: true, Valid: true},
				Source:       sql.NullString{String: "BEGIN\n  NEW.ID = 1;\nEND", Valid: true},
			},
		},
	}
	cache.Metadata[database.MetadataViews] = database.MetadataReady
	return cache
}

// interBaseHoverServer builds a Server wired for the InterBase parser without
// a jsonrpc2 connection. The repository is injected at the call site because
// interbase_common.go's init already claims the InterBase driver name in
// database.driverFactories and RegisterFactory panics on a duplicate.
func interBaseHoverServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer()
	t.Cleanup(func() { _ = server.Stop() })
	server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	return server
}

func hoverAt(t *testing.T, server *Server, repo database.DBRepository, cache *database.DBCache, text string, character int) *lsp.Hover {
	t.Helper()
	params := lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: character},
		},
	}
	base, err := hoverWithDriver(text, params, cache, dialect.DatabaseDriverInterBase)
	if err != nil && !errors.Is(err, ErrNoHover) {
		t.Fatal("hoverWithDriver:", err)
	}
	return server.interBaseHover(context.Background(), repo, cache, params, text, base, server.connectionGeneration(), server.parserDriverVariant())
}

func TestResolveInterBaseHoverTarget(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		character int
		driver    dialect.DatabaseDriver
		catalog   bool
		wantOK    bool
		wantKind  database.ObjectKind
		wantName  string
	}{
		{name: "procedure", text: "execute procedure myproc", character: 20, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindProcedure, wantName: "MYPROC"},
		{name: "view", text: "select * from myview", character: 16, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindView, wantName: "MYVIEW"},
		{name: "generator", text: "select gen_id(gen_order_id, 1) from rdb$database", character: 18, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindGenerator, wantName: "GEN_ORDER_ID"},
		{name: "external function", text: "select myudf(1)", character: 9, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindFunction, wantName: "MYUDF"},
		{name: "trigger", text: "select mytrigger", character: 10, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindTrigger, wantName: "MYTRIGGER"},
		{name: "table", text: "select * from city", character: 16, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindTable, wantName: "CITY"},
		{name: "column produces no target", text: "select id from city", character: 8, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: false},
		{name: "alias produces no target", text: "select * from city c", character: 19, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: false},
		{name: "unknown identifier", text: "select * from nosuchthing", character: 16, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: false},
		{name: "no catalog yet", text: "execute procedure myproc", character: 20, driver: dialect.DatabaseDriverInterBase, catalog: false, wantOK: false},
		{name: "not interbase", text: "execute procedure myproc", character: 20, driver: dialect.DatabaseDriverPostgreSQL, catalog: true, wantOK: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cache := interBaseHoverCache(t)
			if !tt.catalog {
				cache.Catalog = nil
			}
			params := lsp.HoverParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: tt.character},
				},
			}
			got, _, ok := resolveInterBaseHoverTarget(tt.text, params, cache, tt.driver)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (target=%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.kind, tt.wantKind)
			}
			// The catalog spelling, not the user's: it is what ObjectDDL is
			// asked for and what the memo is keyed on.
			if got.name != tt.wantName {
				t.Errorf("name = %q, want %q", got.name, tt.wantName)
			}
		})
	}
}

func TestResolveInterBaseHoverTargetRequiresReadyViewsForTableFallback(t *testing.T) {
	cache := &database.DBCache{
		ColumnsWithParent: map[string][]*database.ColumnDesc{"\tV": {
			{ColumnBase: database.ColumnBase{Table: "V", Name: "ID"}},
		}},
		Catalog:  &database.CatalogCache{Views: map[string]*database.ViewDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataLoading},
	}
	text := "SELECT * FROM V"
	params := lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Character: strings.Index(text, "V")},
	}}
	if _, _, ok := resolveInterBaseHoverTarget(text, params, cache, dialect.DatabaseDriverInterBase); ok {
		t.Fatal("relation with unresolved view category was classified as a table DDL target")
	}
	cache.Metadata[database.MetadataViews] = database.MetadataReady
	got, _, ok := resolveInterBaseHoverTarget(text, params, cache, dialect.DatabaseDriverInterBase)
	if !ok || got.kind != database.ObjectKindTable || got.name != "V" {
		t.Fatalf("ready-empty views table fallback = %+v, %v", got, ok)
	}
}

func TestResolveInterBaseHoverTargetUsesKnownViewWhileViewsLoading(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"V"}},
		Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{
			"V": {Name: "V"},
		}},
		Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataLoading},
	}
	text := "SELECT * FROM V"
	params := lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Character: strings.Index(text, "V")},
	}}
	got, _, ok := resolveInterBaseHoverTarget(text, params, cache, dialect.DatabaseDriverInterBase)
	if !ok || got.kind != database.ObjectKindView || got.name != "V" {
		t.Fatalf("known view match while category loads = %+v, %v", got, ok)
	}
}

func TestHoverDoesNotUseColumnTableFallbackUntilViewsReady(t *testing.T) {
	cache := interBaseHoverCache(t)
	text := "SELECT * FROM MYVIEW"
	params := lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Character: strings.Index(text, "MYVIEW")},
	}}
	cache.Metadata[database.MetadataViews] = database.MetadataLoading
	got, err := hoverWithDriverVariant(text, params, cache, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil && !errors.Is(err, ErrNoHover) {
		t.Fatal("hover while views are loading:", err)
	}
	if got != nil {
		t.Fatalf("unresolved view was reported as a table: %+v", got)
	}

	cache.Metadata[database.MetadataViews] = database.MetadataReady
	got, err = hoverWithDriverVariant(text, params, cache, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal("hover after views are ready:", err)
	}
	if got == nil || !strings.Contains(got.Contents.Value, "MYVIEW") {
		t.Fatalf("ready view should retain positive column-backed hover, got %+v", got)
	}
}

func TestResolveInterBaseHoverTargetIsCaseInsensitiveWithoutUpperCasingAtTheCallSite(t *testing.T) {
	params := lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 20},
		},
	}
	got, _, ok := resolveInterBaseHoverTarget("execute procedure MyProc", params, interBaseHoverCache(t), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("a mixed-case identifier did not resolve; DBCache accessors must normalise the name they are given")
	}
	if got.name != "MYPROC" {
		t.Errorf("name = %q, want %q", got.name, "MYPROC")
	}
}

func TestInterBaseHoverProcedureAppendsDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE PROCEDURE MYPROC (IN_AMOUNT NUMERIC(18, 2)) RETURNS (OUT_TOTAL INTEGER) AS BEGIN SUSPEND; END", nil
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover for a cached procedure")
	}
	for _, want := range []string{
		"`MYPROC` procedure",
		"IN_AMOUNT",
		"OUT_TOTAL",
		"```sql",
		"CREATE PROCEDURE MYPROC",
	} {
		if !strings.Contains(got.Contents.Value, want) {
			t.Errorf("hover missing %q:\n%s", want, got.Contents.Value)
		}
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 1 || calls[0].Kind != database.ObjectKindProcedure || calls[0].Name != "MYPROC" {
		t.Errorf("ObjectDDLCalls() = %+v, want one {procedure MYPROC}", calls)
	}
}

func TestInterBaseHoverUnsupportedDDLFallsBackToSummary(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.NewUnsupportedDDLError("procedure", "MYPROC", `parameter "IN_AMOUNT" nullability is unknown`)
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover; unsupported DDL must never suppress the summary")
	}
	value := got.Contents.Value

	// The summary survives intact. This is the common case for procedures, so
	// it is the case that must not degrade to nothing.
	for _, want := range []string{"`MYPROC` procedure", "IN_AMOUNT", "BEGIN\n  SUSPEND;\nEND"} {
		if !strings.Contains(value, want) {
			t.Errorf("hover lost %q from the summary:\n%s", want, value)
		}
	}
	// Exactly one italic note, naming object, name and blocking feature.
	for _, want := range []string{"_DDL unavailable:", "procedure", `"MYPROC"`, `parameter "IN_AMOUNT" nullability is unknown`} {
		if !strings.Contains(value, want) {
			t.Errorf("note missing %q:\n%s", want, value)
		}
	}
	if strings.Count(value, "_DDL unavailable") != 1 {
		t.Errorf("want exactly one DDL note, got %d:\n%s", strings.Count(value, "_DDL unavailable"), value)
	}
	// No fenced block, and above all no fabricated declaration.
	if strings.Contains(value, "```sql\nCREATE") {
		t.Errorf("a CREATE statement was fabricated:\n%s", value)
	}
}

func TestInterBaseHoverUnsupportedDDLWithoutDetailDegradesToBareNote(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	driverMessage := "interbase: schema: GenerateDDL refused for reasons of its own"
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		// A bare sentinel with no structured detail: UnsupportedDDLDetail
		// reports ok == false.
		return "", fmt.Errorf("%s: %w", driverMessage, database.ErrUnsupportedDDL)
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover")
	}
	value := got.Contents.Value

	if !strings.Contains(value, "_DDL unavailable._") {
		t.Errorf("note is not the bare form:\n%s", value)
	}
	// The failure this pins: a note built by printing err.Error(). The driver
	// message is not user-facing documentation.
	if strings.Contains(value, driverMessage) || strings.Contains(value, "GenerateDDL") {
		t.Errorf("a driver message leaked into the hover note:\n%s", value)
	}
}

func TestInterBaseHoverObjectNotFoundAppendsNothing(t *testing.T) {
	cache := interBaseHoverCache(t)
	server := interBaseHoverServer(t)

	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.ErrObjectNotFound
	}
	withRepo := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)

	// The same hover with no DDLRepository at all. A stale cache is not
	// something the user did, so the two must be byte-identical: no note, no
	// fenced block, no marker of any kind.
	plain := hoverAt(t, interBaseHoverServer(t), database.NewMockDBRepository(nil), cache, "execute procedure myproc", 20)

	if withRepo == nil || plain == nil {
		t.Fatal("no hover in one of the two arrangements")
	}
	if withRepo.Contents.Value != plain.Contents.Value {
		t.Errorf("ErrObjectNotFound changed the hover:\ngot:  %q\nwant: %q", withRepo.Contents.Value, plain.Contents.Value)
	}
	if strings.Contains(withRepo.Contents.Value, "stale") || strings.Contains(withRepo.Contents.Value, "unavailable") {
		t.Errorf("a footnote the user cannot act on was added:\n%s", withRepo.Contents.Value)
	}
	if len(repo.ObjectDDLCalls()) != 1 {
		t.Errorf("ObjectDDL was called %d times, want 1", len(repo.ObjectDDLCalls()))
	}
}

func TestInterBaseHoverDDLErrorIsNotSurfacedAsRequestError(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", errors.New("interbase: connection reset")
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("a DDL failure suppressed the hover; the user asked for documentation, not for DDL")
	}
	if !strings.Contains(got.Contents.Value, "`MYPROC` procedure") {
		t.Errorf("the summary was lost:\n%s", got.Contents.Value)
	}
	if strings.Contains(got.Contents.Value, "connection reset") {
		t.Errorf("a driver error reached the popup:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverExternalFunctionNeverCallsObjectDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE FUNCTION MYUDF", nil
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "select myudf(1)", 9)
	if got == nil {
		t.Fatal("no hover for a cached external function")
	}
	// ObjectKindFunction always returns ErrUnsupportedDDL, so calling it would
	// guarantee a wasted round trip and a note line on every hover.
	if calls := repo.ObjectDDLCalls(); len(calls) != 0 {
		t.Errorf("ObjectDDL was called for an external function: %+v", calls)
	}
	value := got.Contents.Value
	if !strings.Contains(value, "`MYUDF` external function") {
		t.Errorf("hover lost the function summary:\n%s", value)
	}
	if strings.Contains(value, "DDL") {
		t.Errorf("hovering an external function mentioned DDL:\n%s", value)
	}
	// The argument type is permanently unrenderable; the argument is still
	// listed and nothing stands in for the type.
	if !strings.Contains(value, "argument 1") {
		t.Errorf("the unrenderable argument was dropped:\n%s", value)
	}
	if strings.Contains(value, "<unknown>") || strings.Contains(value, "argument 1: ") {
		t.Errorf("a placeholder was rendered for the unknown argument type:\n%s", value)
	}
}

func TestInterBaseHoverTriggerOmitsEmptyEvent(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.ErrObjectNotFound
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "select mytrigger", 10)
	if got == nil {
		t.Fatal("no hover for a cached trigger")
	}
	value := got.Contents.Value
	for _, want := range []string{"`MYTRIGGER` trigger", "CITY", "active", "NEW.ID = 1;"} {
		if !strings.Contains(value, want) {
			t.Errorf("hover missing %q:\n%s", want, value)
		}
	}
	// An empty Event omits the line entirely: the RelationName line is
	// directly followed by the Active line with nothing between them. A
	// forbidden-substring check on "``" would also match the legitimate
	// closing ``` of the Source fence below, so this asserts adjacency
	// instead (same fix as TestTriggerDocOmitsEmptyEvent in Task 3).
	if strings.Contains(value, "<unknown>") {
		t.Errorf("a placeholder event was rendered:\n%s", value)
	}
	if !strings.Contains(value, "On `CITY`.\n\nCurrently active.") {
		t.Errorf("Event was not cleanly omitted between RelationName and Active:\n%s", value)
	}
}

func TestInterBaseHoverGeneratorRendersNameOnly(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE GENERATOR GEN_ORDER_ID", nil
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "select gen_id(gen_order_id, 1) from rdb$database", 18)
	if got == nil {
		t.Fatal("no hover for a cached generator")
	}
	if !strings.Contains(got.Contents.Value, "`GEN_ORDER_ID` generator") {
		t.Errorf("hover lost the generator name:\n%s", got.Contents.Value)
	}
	if !strings.Contains(got.Contents.Value, "CREATE GENERATOR GEN_ORDER_ID") {
		t.Errorf("hover lost the generator DDL:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverTableStillShowsColumnTable(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE TABLE CITY (ID INTEGER)", nil
	}
	cache := interBaseHoverCache(t)

	plain := hoverAt(t, interBaseHoverServer(t), database.NewMockDBRepository(nil), cache, "select * from city", 16)
	got := hoverAt(t, server, repo, cache, "select * from city", 16)
	if plain == nil || got == nil {
		t.Fatal("no hover for a table")
	}

	// DDL is appended, never substituted: the existing markdown column table
	// must still be there, unchanged, as its own prefix.
	if !strings.HasPrefix(got.Contents.Value, plain.Contents.Value) {
		t.Errorf("the column table was replaced rather than appended to:\ngot:  %q\nbase: %q", got.Contents.Value, plain.Contents.Value)
	}
	if !strings.Contains(got.Contents.Value, "CREATE TABLE CITY") {
		t.Errorf("the table DDL was not appended:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverWithoutCapabilityIsUnchanged(t *testing.T) {
	// A plain MockDBRepository, an ordinary build, and every non-InterBase
	// driver all land here: the summary, and nothing else.
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)

	got := hoverAt(t, server, database.NewMockDBRepository(nil), cache, "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover without a DDL capability")
	}
	if !strings.Contains(got.Contents.Value, "`MYPROC` procedure") {
		t.Errorf("the catalog summary needs no capability:\n%s", got.Contents.Value)
	}
	if strings.Contains(got.Contents.Value, "DDL unavailable") {
		t.Errorf("a missing capability produced a note:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverEndToEndKeepsExistingColumnHover(t *testing.T) {
	// Through a real jsonrpc2 round trip with the pre-existing fixture, which
	// has no catalog: the whole InterBase path must be inert.
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	defer tx.server.Stop()
	configureInterBaseTestServer(t, tx, dialect.SQLVariantInterBase1)

	tx.textDocumentDidOpen(t, testFileURI, "select rdb$relation_id from rdb$database")
	var got lsp.Hover
	if err := tx.conn.Call(tx.ctx, "textDocument/hover", lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 11},
		},
	}, &got); err != nil {
		t.Fatal("conn.Call textDocument/hover:", err)
	}
	if !strings.Contains(got.Contents.Value, "`RDB$RELATION_ID` column") {
		t.Fatalf("hover = %q, want the existing column metadata", got.Contents.Value)
	}
}

func TestInterBaseHoverMemoisesObjectDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END", nil
	}

	first := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	second := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if first == nil || second == nil {
		t.Fatal("no hover")
	}
	if first.Contents.Value != second.Contents.Value {
		t.Errorf("the memoised hover differs from the first:\ngot:  %q\nwant: %q", second.Contents.Value, first.Contents.Value)
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 1 {
		t.Fatalf("ObjectDDL was called %d times for two hovers on the same token, want 1", len(calls))
	}

	// A reconnect invalidates everything: the new connection may be a
	// different database entirely.
	server.stateMu.Lock()
	server.connGeneration++
	server.stateMu.Unlock()

	third := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if third == nil {
		t.Fatal("no hover after a reconnect")
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 2 {
		t.Errorf("ObjectDDL was called %d times after a reconnect, want 2", len(calls))
	}
}

func TestInterBaseHoverMemoisesTheUnsupportedNote(t *testing.T) {
	// Unsupported DDL is the common case for procedures, so it is the case
	// that must not re-query on every cursor rest.
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.NewUnsupportedDDLError("procedure", "MYPROC", `parameter "IN_AMOUNT" nullability is unknown`)
	}

	hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	got := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover")
	}
	if !strings.Contains(got.Contents.Value, "_DDL unavailable:") {
		t.Errorf("the memoised note was lost:\n%s", got.Contents.Value)
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 1 {
		t.Errorf("ObjectDDL was called %d times, want 1", len(calls))
	}
}

func TestInterBaseHoverDoesNotMemoiseTransientFailures(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	calls := 0
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("interbase: connection reset")
		}
		return "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END", nil
	}

	if got := hoverAt(t, server, repo, cache, "execute procedure myproc", 20); got == nil {
		t.Fatal("no hover after a transport failure")
	}
	got := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover on the retry")
	}
	// Memoising "" for a transport failure would suppress this object's DDL
	// until the next reconnect.
	if !strings.Contains(got.Contents.Value, "CREATE PROCEDURE MYPROC") {
		t.Errorf("a transient failure was memoised; the retry produced no DDL:\n%s", got.Contents.Value)
	}
	if len(repo.ObjectDDLCalls()) != 2 {
		t.Errorf("ObjectDDL was called %d times, want 2", len(repo.ObjectDDLCalls()))
	}
}

func TestInterBaseHoverMemoIsKeyedByObject(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(_ context.Context, kind database.ObjectKind, name string) (string, error) {
		return "CREATE " + strings.ToUpper(string(kind)) + " " + name, nil
	}

	procedure := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	view := hoverAt(t, server, repo, cache, "select * from myview", 16)
	if procedure == nil || view == nil {
		t.Fatal("no hover")
	}
	if !strings.Contains(procedure.Contents.Value, "CREATE PROCEDURE MYPROC") {
		t.Errorf("procedure hover:\n%s", procedure.Contents.Value)
	}
	if !strings.Contains(view.Contents.Value, "CREATE VIEW MYVIEW") {
		t.Errorf("view hover:\n%s", view.Contents.Value)
	}

	// MYPROC and MYVIEW have different names, so a name-only key can never
	// collide between them: that pair alone cannot pin the failure this test
	// is named for. resolveInterBaseHoverTarget resolves views before
	// procedures, so one cache can never offer both kinds under the same
	// name; a procedure and a view both named SHARED are hovered from two
	// separate cache instances instead, sharing the same server so the memo
	// is what must still tell them apart.
	procShared := interBaseHoverCache(t)
	procShared.Catalog.Procedures["SHARED"] = &database.ProcedureDesc{Name: "SHARED"}
	viewShared := interBaseHoverCache(t)
	viewShared.Catalog.Views["SHARED"] = &database.ViewDesc{Name: "SHARED"}

	sharedProcedure := hoverAt(t, server, repo, procShared, "execute procedure shared", 20)
	sharedView := hoverAt(t, server, repo, viewShared, "select * from shared", 16)
	if sharedProcedure == nil || sharedView == nil {
		t.Fatal("no hover for the shared-name pair")
	}
	if !strings.Contains(sharedProcedure.Contents.Value, "CREATE PROCEDURE SHARED") {
		t.Errorf("shared-name procedure hover:\n%s", sharedProcedure.Contents.Value)
	}
	// The failure this pins: a memo keyed on the name alone serves the
	// procedure's DDL for the view.
	if !strings.Contains(sharedView.Contents.Value, "CREATE VIEW SHARED") {
		t.Errorf("shared-name view hover was served the wrong object's DDL:\n%s", sharedView.Contents.Value)
	}

	var sharedCalls []database.ObjectDDLCall
	for _, call := range repo.ObjectDDLCalls() {
		if call.Name == "SHARED" {
			sharedCalls = append(sharedCalls, call)
		}
	}
	if len(sharedCalls) != 2 {
		t.Fatalf("ObjectDDL was called %d times for the shared name, want 2: %+v", len(sharedCalls), sharedCalls)
	}
	if sharedCalls[0].Kind == sharedCalls[1].Kind {
		t.Errorf("both ObjectDDL calls for the shared name used kind %q, want one procedure and one view: %+v", sharedCalls[0].Kind, sharedCalls)
	}
}

func TestHoverMemoLockIsNotHeldAcrossObjectDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		// Takes the very mutex the memo lives under. If stateMu were held
		// across the round trip this blocks forever, which is precisely the
		// invariant "stateMu is never held across any I/O" — and a 3-second
		// ObjectDDL call is I/O.
		server.stateMu.Lock()
		server.stateMu.Unlock()
		return "CREATE PROCEDURE MYPROC", nil
	}

	params := lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 20},
		},
	}
	done := make(chan *lsp.Hover, 1)
	go func() {
		done <- server.interBaseHover(context.Background(), repo, cache, params, "execute procedure myproc", nil, server.connectionGeneration(), server.parserDriverVariant())
	}()

	select {
	case got := <-done:
		if got == nil {
			t.Fatal("no hover")
		}
		if !strings.Contains(got.Contents.Value, "CREATE PROCEDURE MYPROC") {
			t.Errorf("hover lost the DDL:\n%s", got.Contents.Value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hover deadlocked: stateMu was held across the ObjectDDL round trip")
	}
}
