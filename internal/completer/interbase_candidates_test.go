package completer

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

// interBaseCatalogCache is the shared fixture for every feature-2 test: two
// procedures (one selectable, one not), a view that is ALSO an ordinary table
// candidate — which is the contract's normal state, since SortedViews() does
// not subtract from SortedTables() — a generator, and a UDF with one
// unrenderable argument type.
func interBaseCatalogCache(t *testing.T) *database.DBCache {
	t.Helper()
	cache := interBaseBaseCache(t)
	cache.Catalog = &database.CatalogCache{
		Procedures: map[string]*database.ProcedureDesc{
			"MYPROC": {
				Name: "MYPROC",
				InputParameters: []*database.ProcedureParameterDesc{
					{Name: "IN_CODE", Position: 0, Direction: database.ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
					{Name: "IN_AMOUNT", Position: 1, Direction: database.ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
				},
				OutputParameters: []*database.ProcedureParameterDesc{
					{Name: "OUT_TOTAL", Position: 0, Direction: database.ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
				},
			},
			"DOWORK": {
				Name: "DOWORK",
				InputParameters: []*database.ProcedureParameterDesc{
					{Name: "IN_ID", Position: 0, Direction: database.ParameterInput, Type: "INTEGER"},
				},
			},
		},
		Views: map[string]*database.ViewDesc{
			"MYVIEW": {
				Name:       "MYVIEW",
				ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
				Columns: []*database.ColumnDesc{
					{ColumnBase: database.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
				},
			},
		},
		Generators: map[string]*database.GeneratorDesc{
			"GEN_ORDER_ID": {Name: "GEN_ORDER_ID", ID: sql.NullInt64{Int64: 3, Valid: true}},
		},
		Functions: map[string]*database.FunctionDesc{
			"MYUDF": {
				Name:       "MYUDF",
				ReturnType: "DOUBLE PRECISION",
				ModuleName: sql.NullString{String: "udflib", Valid: true},
				EntryPoint: sql.NullString{String: "myudf", Valid: true},
				Arguments: []*database.FunctionArgumentDesc{
					{Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "DOUBLE PRECISION"},
					{Position: sql.NullInt64{Int64: 2, Valid: true}, Type: ""},
				},
			},
		},
	}
	return cache
}

// interBaseBaseCache is the same cache with no Catalog at all: the window
// before the worker's secondary pass lands, and every non-InterBase driver.
func interBaseBaseCache(t *testing.T) *database.DBCache {
	t.Helper()
	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Schema: "MAIN", Table: "CITY", Name: "ID"}, Type: "INTEGER"},
		{ColumnBase: database.ColumnBase{Schema: "MAIN", Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "MAIN", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{"MAIN"}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			// MYVIEW is here because interBaseRelationsQuery selects every
			// non-system row of RDB$RELATIONS, which includes views.
			return map[string][]string{"MAIN": {"CITY", "MYVIEW"}}, nil
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
	return cache
}

func completeInterBase(t *testing.T, cache *database.DBCache, text string) []lsp.CompletionItem {
	t.Helper()
	c := NewCompleter(cache)
	c.Driver = dialect.DatabaseDriverInterBase
	got, err := c.Complete(text, lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			Position: lsp.Position{Line: 0, Character: len(text)},
		},
	}, false)
	if err != nil {
		t.Fatal("Complete:", err)
	}
	return got
}

func candidatesFor(items []lsp.CompletionItem, label string) []lsp.CompletionItem {
	matched := []lsp.CompletionItem{}
	for _, item := range items {
		if item.Label == label {
			matched = append(matched, item)
		}
	}
	return matched
}

func soleCandidate(t *testing.T, items []lsp.CompletionItem, label string) lsp.CompletionItem {
	t.Helper()
	matched := candidatesFor(items, label)
	if len(matched) == 0 {
		t.Fatalf("no candidate labelled %q in %v", label, completionLabels(items))
	}
	if len(matched) > 1 {
		t.Fatalf("%d candidates labelled %q, want exactly 1: %+v", len(matched), label, matched)
	}
	return matched[0]
}

func TestInterBaseProcedureCompletionAfterExecuteProcedure(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "execute procedure ")

	item := soleCandidate(t, got, "MYPROC")
	if item.Kind != lsp.MethodCompletion {
		t.Errorf("kind = %v, want MethodCompletion", item.Kind)
	}
	if item.Detail != "procedure" {
		t.Errorf("detail = %q, want %q", item.Detail, "procedure")
	}
	// A procedure with no output is still executable, so both are offered
	// here — unlike the FROM position.
	if len(candidatesFor(got, "DOWORK")) != 1 {
		t.Errorf("DOWORK missing after EXECUTE PROCEDURE: %v", completionLabels(got))
	}
	// Tables are not relations here. If the ExecuteProcedure branch is written
	// to include CompletionTypeTable, this fails.
	if len(candidatesFor(got, "CITY")) != 0 {
		t.Errorf("table candidate offered after EXECUTE PROCEDURE: %v", completionLabels(got))
	}
	// Keywords are retained deliberately: the multi-keyword group fires for
	// every dialect, and a PostgreSQL user writing a legacy trigger must not
	// lose the candidates they get today.
	if !completionLabels(got)["SELECT"] {
		t.Errorf("keyword candidates were dropped: %v", completionLabels(got))
	}
}

func TestInterBaseCompletionAfterExecuteProcedureIsCaseInsensitive(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "execute procedure myp")
	if len(candidatesFor(got, "MYPROC")) != 1 {
		t.Errorf("lowercase prefix %q did not match MYPROC: %v", "myp", completionLabels(got))
	}
}

func TestInterBaseSelectableProcedureCompletionInFromClause(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select * from ")

	item := soleCandidate(t, got, "MYPROC")
	if item.Kind != lsp.MethodCompletion {
		t.Errorf("kind = %v, want MethodCompletion", item.Kind)
	}
	if item.Detail != "selectable procedure" {
		t.Errorf("detail = %q, want %q", item.Detail, "selectable procedure")
	}
	// DOWORK has no output parameters, so it cannot appear in a FROM clause.
	if len(candidatesFor(got, "DOWORK")) != 0 {
		t.Errorf("a procedure with no output was offered as a relation: %v", completionLabels(got))
	}
}

func TestInterBaseViewInFromPositionIsOfferedExactlyOnce(t *testing.T) {
	cache := interBaseCatalogCache(t)
	// Precondition, so this test can never pass vacuously: the view must be
	// reachable through BOTH paths, which is the contract's normal state.
	if _, ok := cache.View("MYVIEW"); !ok {
		t.Fatal("fixture error: MYVIEW is not in the view catalog")
	}
	found := false
	for _, table := range cache.SortedTables() {
		if table == "MYVIEW" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture error: MYVIEW is not in SortedTables() %v", cache.SortedTables())
	}

	got := completeInterBase(t, cache, "select * from ")

	// soleCandidate fails loudly at 2, which is what a naive "append view
	// candidates alongside table candidates" implementation produces.
	item := soleCandidate(t, got, "MYVIEW")
	if item.Detail != "view" {
		t.Errorf("detail = %q, want %q", item.Detail, "view")
	}
	if item.Kind != lsp.ClassCompletion {
		t.Errorf("kind = %v, want ClassCompletion so the view sorts with tables", item.Kind)
	}
	if len(candidatesFor(got, "CITY")) != 1 {
		t.Errorf("ordinary table candidate changed: %v", completionLabels(got))
	}
	if detail := soleCandidate(t, got, "CITY").Detail; detail != "table" {
		t.Errorf("CITY detail = %q, want %q", detail, "table")
	}
}

func TestInterBaseViewCandidatesInInsertColumnPosition(t *testing.T) {
	// The one position where CompletionTypeView is set and
	// CompletionTypeTable is not.
	got := completeInterBase(t, interBaseCatalogCache(t), "insert into city (")
	if len(candidatesFor(got, "MYVIEW")) != 1 {
		t.Errorf("view candidate missing where CompletionTypeView is the only relation type: %v", completionLabels(got))
	}
}

func TestInterBaseProcedureOutputParameterColumnCompletion(t *testing.T) {
	// Each case carries its own column. Do NOT derive it from len(text):
	// the member-identifier case needs the procedure in scope as a relation
	// as well as before the dot, so its text is longer than its cursor.
	cases := []struct {
		name string
		text string
		col  int
	}{
		// ParentTypeNone: the procedure is in FROM, the cursor is in the
		// select list. Reaches procedureColumnCandidates through the
		// no-parent fallback.
		{name: "select list over a selectable procedure", text: "select  from myproc", col: 7},
		// ParentTypeTable: "myproc." with myproc also in FROM. This is the
		// half of the feature a user actually types, and it reaches
		// procedureColumnCandidates through the ColumnDescs miss.
		//
		// The FROM clause is not decoration. parseutil.ExtractTable returns
		// an empty slice for a bare "select myproc." — measured — and
		// columnCandidates ranges over that slice, so with nothing in scope
		// neither branch is ever entered and this subtest could not pass
		// against any implementation. That is pre-existing completer
		// behaviour ("select city." behaves identically), not something this
		// plan introduces.
		{name: "member identifier", text: "select myproc. from myproc", col: 14},
		// The same branch reached through an alias. ExtractTable resolves
		// aliases at this position and the ParentTypeTable match tests
		// table.Alias as well as table.Name, so this path exists — it is
		// here because nothing else in the plan exercises it.
		{name: "aliased member identifier", text: "select p. from myproc p", col: 9},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCompleter(interBaseCatalogCache(t))
			c.Driver = dialect.DatabaseDriverInterBase
			got, err := c.Complete(tt.text, lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					Position: lsp.Position{Line: 0, Character: tt.col},
				},
			}, false)
			if err != nil {
				t.Fatal("Complete:", err)
			}

			item := soleCandidate(t, got, "OUT_TOTAL")
			if item.Kind != lsp.FieldCompletion {
				t.Errorf("kind = %v, want FieldCompletion", item.Kind)
			}
			if item.Detail != `column from "MYPROC"` {
				t.Errorf("detail = %q, want %q", item.Detail, `column from "MYPROC"`)
			}
			// Input parameters are never valid text in a statement: DSQL has
			// no named parameters.
			if len(candidatesFor(got, "IN_CODE")) != 0 {
				t.Errorf("an input parameter was offered as a column: %v", completionLabels(got))
			}
		})
	}
}

func TestInterBaseExternalFunctionCompletionInSelectExpr(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select ")

	item := soleCandidate(t, got, "MYUDF")
	if item.Kind != lsp.FunctionCompletion {
		t.Errorf("kind = %v, want FunctionCompletion", item.Kind)
	}
	if item.Detail != "external function" {
		t.Errorf("detail = %q, want %q", item.Detail, "external function")
	}
	// Built-in functions keep coming: a UDF is callable exactly where one is.
	if !completionLabels(got)["GEN_ID"] {
		t.Errorf("built-in function candidates were lost: %v", completionLabels(got))
	}
}

func TestInterBaseExternalFunctionCompletionWithUnrenderableArgumentType(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select ")

	item := soleCandidate(t, got, "MYUDF")
	if item.Documentation == nil {
		t.Fatal("the UDF candidate carries no documentation")
	}
	doc := item.Documentation.Value
	if !strings.Contains(doc, "argument 1: `DOUBLE PRECISION`") {
		t.Errorf("documentation lost the renderable argument:\n%s", doc)
	}
	if !strings.Contains(doc, "argument 2") {
		t.Errorf("documentation dropped the unrenderable argument entirely:\n%s", doc)
	}
	// The object must be completable by name even though one argument type is
	// permanently unrenderable, and no placeholder may appear anywhere in it.
	for _, forbidden := range []string{"<unknown>", "argument 2: ", "``", "UNKNOWN"} {
		if strings.Contains(doc, forbidden) {
			t.Errorf("documentation contains the placeholder %q:\n%s", forbidden, doc)
		}
	}
}

func TestInterBaseGeneratorCompletionInsideGenId(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select gen_id(")

	item := soleCandidate(t, got, "GEN_ORDER_ID")
	if item.Kind != lsp.ValueCompletion {
		t.Errorf("kind = %v, want ValueCompletion", item.Kind)
	}
	if item.Detail != "generator" {
		t.Errorf("detail = %q, want %q", item.Detail, "generator")
	}
}

func TestInterBaseGeneratorCompletionIsScopedToGenId(t *testing.T) {
	// Offering every generator in every expression position would bury column
	// candidates. These three are the positions a naive implementation leaks
	// into.
	for _, text := range []string{"select upper(", "select ", "select * from "} {
		got := completeInterBase(t, interBaseCatalogCache(t), text)
		if len(candidatesFor(got, "GEN_ORDER_ID")) != 0 {
			t.Errorf("generator offered at %q: %v", text, completionLabels(got))
		}
	}
}

func TestInterBaseCompletionDegradesWithoutCatalog(t *testing.T) {
	// The window between initialize and the worker's first successful
	// secondary pass, and every ordinary build.
	cache := interBaseBaseCache(t)
	if cache.HasCatalog() {
		t.Fatal("fixture error: the base cache must have no catalog")
	}

	got := completeInterBase(t, cache, "select * from ")
	if len(candidatesFor(got, "CITY")) != 1 {
		t.Errorf("today's table candidates were lost: %v", completionLabels(got))
	}
	for _, absent := range []string{"MYPROC", "DOWORK", "GEN_ORDER_ID", "MYUDF"} {
		if len(candidatesFor(got, absent)) != 0 {
			t.Errorf("%q was offered with no catalog: %v", absent, completionLabels(got))
		}
	}

	if got := completeInterBase(t, cache, "execute procedure "); len(got) == 0 {
		t.Error("completion returned nothing at all; keywords must still be offered")
	}
	if got := completeInterBase(t, cache, "select gen_id("); len(candidatesFor(got, "GEN_ORDER_ID")) != 0 {
		t.Error("a generator was offered with no catalog")
	}
}

func TestInterBaseCandidatesAreNotOfferedToOtherDrivers(t *testing.T) {
	c := NewCompleter(interBaseCatalogCache(t))
	c.Driver = dialect.DatabaseDriverPostgreSQL

	got, err := c.Complete("execute procedure ", lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			Position: lsp.Position{Line: 0, Character: 18},
		},
	}, false)
	if err != nil {
		t.Fatal("Complete:", err)
	}
	if len(candidatesFor(got, "MYPROC")) != 0 {
		t.Errorf("a procedure candidate reached a PostgreSQL user: %v", completionLabels(got))
	}
	// This is the mitigation for the shared multiKeywordMap change: the
	// PostgreSQL user keeps exactly the candidates they get today.
	if len(got) == 0 {
		t.Error("the PostgreSQL user received nothing; keyword candidates must be retained")
	}
}

// TestInterBaseViewDetailRequiresACatalogRegardlessOfDriver targets the two
// candidates.go call sites this task extends (generateTableCandidates,
// generateTableCandidatesByInfos), which are package-level functions with no
// access to c.Driver: they gate solely on dbCache.View(), i.e. on
// HasCatalog(). That is correct per the brief ("DBCache.View is nil-safe and
// returns false for every driver without a catalog") because only an
// InterBase repository's CatalogRepository implementation ever populates
// .Catalog, so no non-InterBase DBCache can reach the "view" relabeling in
// production. This pins that the only DBCache state a non-InterBase driver
// actually has (no Catalog) reproduces today's exact Detail/Kind for a
// same-named table at both call sites, not just the absence of new names.
func TestInterBaseViewDetailRequiresACatalogRegardlessOfDriver(t *testing.T) {
	cache := interBaseBaseCache(t)
	for _, driver := range []dialect.DatabaseDriver{dialect.DatabaseDriverPostgreSQL, dialect.DatabaseDriverMySQL} {
		t.Run(string(driver), func(t *testing.T) {
			c := NewCompleter(cache)
			c.Driver = driver

			complete := func(text string) []lsp.CompletionItem {
				t.Helper()
				got, err := c.Complete(text, lsp.CompletionParams{
					TextDocumentPositionParams: lsp.TextDocumentPositionParams{
						Position: lsp.Position{Line: 0, Character: len(text)},
					},
				}, false)
				if err != nil {
					t.Fatal("Complete:", err)
				}
				return got
			}

			// generateTableCandidates: the FROM position.
			from := complete("select * from ")
			if item := soleCandidate(t, from, "MYVIEW"); item.Detail != "table" || item.Kind != lsp.ClassCompletion {
				t.Errorf("FROM: MYVIEW = %+v, want Detail=table Kind=ClassCompletion", item)
			}

			// generateTableCandidatesByInfos: the referenced-table position.
			where := complete("select * from MYVIEW where ")
			if item := soleCandidate(t, where, "MYVIEW"); item.Detail != "referenced table" || item.Kind != lsp.ClassCompletion {
				t.Errorf(`WHERE: MYVIEW = %+v, want Detail="referenced table" Kind=ClassCompletion`, item)
			}
		})
	}
}

func TestCompletionKindSortPrefixesAreAssigned(t *testing.T) {
	cases := []struct {
		kind lsp.CompletionItemKind
		want string
	}{
		{lsp.FieldCompletion, "0"},
		{lsp.ClassCompletion, "1"},
		{lsp.FunctionCompletion, "10"},
		{lsp.MethodCompletion, "10"},
		{lsp.ValueCompletion, "11"},
		{lsp.KeywordCompletion, "9999"},
	}
	for _, tt := range cases {
		if got := getSortTextPrefix(tt.kind); got != tt.want {
			t.Errorf("getSortTextPrefix(%v) = %q, want %q", tt.kind, got, tt.want)
		}
	}
	// The failure this pins: a new kind left in the catch-all list sorts below
	// every keyword, which is worse than not offering it.
	for _, kind := range []lsp.CompletionItemKind{lsp.MethodCompletion, lsp.ValueCompletion} {
		if getSortTextPrefix(kind) == getSortTextPrefix(lsp.KeywordCompletion) {
			t.Errorf("kind %v is still in the keyword sort bucket", kind)
		}
	}
}
