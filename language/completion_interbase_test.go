// This file ports internal/completer's InterBase behavioral completion
// coverage (deleted in the shared-language-core migration; see
// task-B2-report.md Important 2) against language.Service.Complete
// directly, replacing the deleted package's own test suite rather than
// resurrecting it.
package language_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

// interBaseCompletionCatalogCache is the shared fixture for most cases
// below: a selectable procedure (MYPROC), a non-selectable procedure
// (DOWORK), a view that is ALSO an ordinary table candidate — which is the
// contract's normal state, since SortedViews() does not subtract from
// SortedTables() — a generator, and a UDF with one unrenderable argument
// type.
func interBaseCompletionCatalogCache() *catalog.Cache {
	return &catalog.Cache{
		SchemaTables: map[string][]string{"": {"CITY", "MYVIEW"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CITY"):   {{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "ID"}, Type: "INTEGER"}},
			catalog.ColumnDatabaseKey("", "MYVIEW"): {{ColumnBase: catalog.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"}},
		},
		Catalog: &catalog.CatalogCache{
			Procedures: map[string]*catalog.ProcedureDesc{
				"MYPROC": {
					Name: "MYPROC",
					InputParameters: []*catalog.ProcedureParameterDesc{
						{Name: "IN_CODE", Position: 0, Direction: catalog.ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
						{Name: "IN_AMOUNT", Position: 1, Direction: catalog.ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
					},
					OutputParameters: []*catalog.ProcedureParameterDesc{
						{Name: "OUT_TOTAL", Position: 0, Direction: catalog.ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
					},
				},
				"DOWORK": {
					Name: "DOWORK",
					InputParameters: []*catalog.ProcedureParameterDesc{
						{Name: "IN_ID", Position: 0, Direction: catalog.ParameterInput, Type: "INTEGER"},
					},
				},
			},
			Views: map[string]*catalog.ViewDesc{
				"MYVIEW": {
					Name:       "MYVIEW",
					ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
					Columns: []*catalog.ColumnDesc{
						{ColumnBase: catalog.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
					},
				},
			},
			Generators: map[string]*catalog.GeneratorDesc{
				"GEN_ORDER_ID": {Name: "GEN_ORDER_ID", ID: sql.NullInt64{Int64: 3, Valid: true}},
			},
			Functions: map[string]*catalog.FunctionDesc{
				"MYUDF": {
					Name:       "MYUDF",
					ReturnType: "DOUBLE PRECISION",
					ModuleName: sql.NullString{String: "udflib", Valid: true},
					EntryPoint: sql.NullString{String: "myudf", Valid: true},
					Arguments: []*catalog.FunctionArgumentDesc{
						{Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "DOUBLE PRECISION"},
						{Position: sql.NullInt64{Int64: 2, Valid: true}, Type: ""},
					},
				},
			},
		},
	}
}

// interBaseBaseCompletionCache is the same relations with no Catalog at
// all: the window before the worker's secondary pass lands, and every
// non-InterBase driver.
func interBaseBaseCompletionCache() *catalog.Cache {
	return &catalog.Cache{
		SchemaTables: map[string][]string{"": {"CITY", "MYVIEW"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CITY"):   {{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "ID"}, Type: "INTEGER"}},
			catalog.ColumnDatabaseKey("", "MYVIEW"): {{ColumnBase: catalog.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"}},
		},
	}
}

func interBaseJoinCompletionCache() *catalog.Cache {
	foreignKeys, err := catalog.GroupForeignKeys([]*catalog.ForeignKey{
		{
			[2]*catalog.ColumnBase{{Table: "CHILD", Name: "CHILD_A"}, {Table: "PARENT", Name: "PARENT_A"}},
			[2]*catalog.ColumnBase{{Table: "CHILD", Name: "CHILD_B"}, {Table: "PARENT", Name: "PARENT_B"}},
		},
	})
	if err != nil {
		panic(err)
	}
	return &catalog.Cache{
		SchemaTables: map[string][]string{"": {"CHILD", "PARENT"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CHILD"): {
				{ColumnBase: catalog.ColumnBase{Table: "CHILD", Name: "CHILD_A"}},
				{ColumnBase: catalog.ColumnBase{Table: "CHILD", Name: "CHILD_B"}},
			},
			catalog.ColumnDatabaseKey("", "PARENT"): {
				{ColumnBase: catalog.ColumnBase{Table: "PARENT", Name: "PARENT_A"}},
				{ColumnBase: catalog.ColumnBase{Table: "PARENT", Name: "PARENT_B"}},
			},
		},
		ForeignKeys: foreignKeys,
	}
}

func interBaseDollarJoinCompletionCache() *catalog.Cache {
	foreignKeys, err := catalog.GroupForeignKeys([]*catalog.ForeignKey{
		{
			[2]*catalog.ColumnBase{{Table: "CHILD$2", Name: "CHILD_ID$2"}, {Table: "PARENT$2", Name: "PARENT_ID$2"}},
		},
	})
	if err != nil {
		panic(err)
	}
	return &catalog.Cache{
		SchemaTables: map[string][]string{"": {"CHILD$2", "PARENT$2"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CHILD$2"):  {{ColumnBase: catalog.ColumnBase{Table: "CHILD$2", Name: "CHILD_ID$2"}}},
			catalog.ColumnDatabaseKey("", "PARENT$2"): {{ColumnBase: catalog.ColumnBase{Table: "PARENT$2", Name: "PARENT_ID$2"}}},
		},
		ForeignKeys: foreignKeys,
	}
}

// interBaseDialectCompletionCache carries the InterBase system table and a
// mixed-case user table, for the '$' word-pattern and lowercase-identifier
// coverage.
func interBaseDialectCompletionCache() *catalog.Cache {
	return &catalog.Cache{
		SchemaTables: map[string][]string{"": {"RDB$DATABASE", "users"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "RDB$DATABASE"): {
				{ColumnBase: catalog.ColumnBase{Table: "RDB$DATABASE", Name: "RDB$RELATION_ID"}},
				{ColumnBase: catalog.ColumnBase{Table: "RDB$DATABASE", Name: "RDB_OTHER"}},
			},
			catalog.ColumnDatabaseKey("", "users"): {
				{ColumnBase: catalog.ColumnBase{Table: "users", Name: "id"}},
			},
		},
	}
}

func interBaseCompletionRequest(cache *catalog.Cache, text string, variant dialect.SQLVariant) language.Request {
	return language.Request{
		Document: language.Document{URI: "memory:completion-interbase", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        &catalog.MetadataSnapshot{Cache: cache},
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: variant},
			DialectResolved: true,
		},
	}
}

func otherDriverCompletionRequest(cache *catalog.Cache, driver dialect.DatabaseDriver, text string) language.Request {
	return language.Request{
		Document: language.Document{URI: "memory:completion-other-driver", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        &catalog.MetadataSnapshot{Cache: cache},
			Variant:         dialect.DriverVariant{Driver: driver},
			DialectResolved: true,
		},
	}
}

func completeAt(t *testing.T, req language.Request, col int) []language.CompletionItem {
	t.Helper()
	result, err := language.NewService().Complete(context.Background(), req, language.Position{UTF16Column: col})
	if err != nil {
		t.Fatal("Complete:", err)
	}
	return result.Value.Items
}

func completionLabels(items []language.CompletionItem) map[string]bool {
	labels := make(map[string]bool, len(items))
	for _, item := range items {
		labels[item.Label] = true
	}
	return labels
}

func candidatesFor(items []language.CompletionItem, label string) []language.CompletionItem {
	matched := []language.CompletionItem{}
	for _, item := range items {
		if item.Label == label {
			matched = append(matched, item)
		}
	}
	return matched
}

func soleCandidate(t *testing.T, items []language.CompletionItem, label string) language.CompletionItem {
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
	const text = "execute procedure "
	got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))

	item := soleCandidate(t, got, "MYPROC")
	if item.Kind != language.KindMethod {
		t.Errorf("kind = %v, want KindMethod", item.Kind)
	}
	if item.Detail != "procedure" {
		t.Errorf("detail = %q, want %q", item.Detail, "procedure")
	}
	// A procedure with no output is still executable, so both are offered
	// here — unlike the FROM position.
	if len(candidatesFor(got, "DOWORK")) != 1 {
		t.Errorf("DOWORK missing after EXECUTE PROCEDURE: %v", completionLabels(got))
	}
	// Tables are not relations here.
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
	const text = "execute procedure myp"
	got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))
	if len(candidatesFor(got, "MYPROC")) != 1 {
		t.Errorf("lowercase prefix %q did not match MYPROC: %v", "myp", completionLabels(got))
	}
}

func TestInterBaseSelectableProcedureCompletionInFromClause(t *testing.T) {
	const text = "select * from "
	got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))

	item := soleCandidate(t, got, "MYPROC")
	if item.Kind != language.KindMethod {
		t.Errorf("kind = %v, want KindMethod", item.Kind)
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
	cache := interBaseCompletionCatalogCache()
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

	const text = "select * from "
	got := completeAt(t, interBaseCompletionRequest(cache, text, dialect.SQLVariantInterBase3), len(text))

	// soleCandidate fails loudly at 2, which is what a naive "append view
	// candidates alongside table candidates" implementation produces.
	item := soleCandidate(t, got, "MYVIEW")
	if item.Detail != "view" {
		t.Errorf("detail = %q, want %q", item.Detail, "view")
	}
	if item.Kind != language.KindClass {
		t.Errorf("kind = %v, want KindClass so the view sorts with tables", item.Kind)
	}
	if len(candidatesFor(got, "CITY")) != 1 {
		t.Errorf("ordinary table candidate changed: %v", completionLabels(got))
	}
	if detail := soleCandidate(t, got, "CITY").Detail; detail != "table" {
		t.Errorf("CITY detail = %q, want %q", detail, "table")
	}
}

func TestInterBaseViewCandidatesInInsertColumnPosition(t *testing.T) {
	// The one position where completionTypeView is set and
	// completionTypeTable is not.
	const text = "insert into city ("
	got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))
	if len(candidatesFor(got, "MYVIEW")) != 1 {
		t.Errorf("view candidate missing where completionTypeView is the only relation type: %v", completionLabels(got))
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
		// select list.
		{name: "select list over a selectable procedure", text: "select  from myproc", col: 7},
		// ParentTypeTable: "myproc." with myproc also in FROM. This is the
		// half of the feature a user actually types.
		{name: "member identifier", text: "select myproc. from myproc", col: 14},
		// The same branch reached through an alias.
		{name: "aliased member identifier", text: "select p. from myproc p", col: 9},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := interBaseCompletionRequest(interBaseCompletionCatalogCache(), tt.text, dialect.SQLVariantInterBase3)
			got := completeAt(t, req, tt.col)

			item := soleCandidate(t, got, "OUT_TOTAL")
			if item.Kind != language.KindField {
				t.Errorf("kind = %v, want KindField", item.Kind)
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
	const text = "select "
	got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))

	item := soleCandidate(t, got, "MYUDF")
	if item.Kind != language.KindFunction {
		t.Errorf("kind = %v, want KindFunction", item.Kind)
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
	const text = "select "
	got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))

	item := soleCandidate(t, got, "MYUDF")
	if item.Documentation == "" {
		t.Fatal("the UDF candidate carries no documentation")
	}
	doc := item.Documentation
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
	const text = "select gen_id("
	got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))

	item := soleCandidate(t, got, "GEN_ORDER_ID")
	if item.Kind != language.KindValue {
		t.Errorf("kind = %v, want KindValue", item.Kind)
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
		got := completeAt(t, interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, dialect.SQLVariantInterBase3), len(text))
		if len(candidatesFor(got, "GEN_ORDER_ID")) != 0 {
			t.Errorf("generator offered at %q: %v", text, completionLabels(got))
		}
	}
}

func TestInterBaseCompletionDegradesWithoutCatalog(t *testing.T) {
	// The window between initialize and the worker's first successful
	// secondary pass, and every ordinary build.
	cache := interBaseBaseCompletionCache()
	if cache.HasCatalog() {
		t.Fatal("fixture error: the base cache must have no catalog")
	}

	const fromText = "select * from "
	got := completeAt(t, interBaseCompletionRequest(cache, fromText, dialect.SQLVariantInterBase3), len(fromText))
	if len(candidatesFor(got, "CITY")) != 1 {
		t.Errorf("today's table candidates were lost: %v", completionLabels(got))
	}
	for _, absent := range []string{"MYPROC", "DOWORK", "GEN_ORDER_ID", "MYUDF"} {
		if len(candidatesFor(got, absent)) != 0 {
			t.Errorf("%q was offered with no catalog: %v", absent, completionLabels(got))
		}
	}

	const execText = "execute procedure "
	if got := completeAt(t, interBaseCompletionRequest(cache, execText, dialect.SQLVariantInterBase3), len(execText)); len(got) == 0 {
		t.Error("completion returned nothing at all; keywords must still be offered")
	}
	const genText = "select gen_id("
	if got := completeAt(t, interBaseCompletionRequest(cache, genText, dialect.SQLVariantInterBase3), len(genText)); len(candidatesFor(got, "GEN_ORDER_ID")) != 0 {
		t.Error("a generator was offered with no catalog")
	}
}

func TestInterBaseCandidatesAreNotOfferedToOtherDrivers(t *testing.T) {
	const text = "execute procedure "
	req := otherDriverCompletionRequest(interBaseCompletionCatalogCache(), dialect.DatabaseDriverPostgreSQL, text)
	got := completeAt(t, req, len(text))
	if len(candidatesFor(got, "MYPROC")) != 0 {
		t.Errorf("a procedure candidate reached a PostgreSQL user: %v", completionLabels(got))
	}
	// This is the mitigation for the shared multiKeywordMap: the PostgreSQL
	// user keeps exactly the candidates they get today.
	if len(got) == 0 {
		t.Error("the PostgreSQL user received nothing; keyword candidates must be retained")
	}
}

// TestInterBaseViewDetailRequiresACatalogRegardlessOfDriver targets the two
// call sites that only relabel a table candidate as a view when
// cache.View() succeeds, i.e. only when HasCatalog() is true: since only an
// InterBase repository's catalog pass ever populates .Catalog, no
// non-InterBase cache can reach the "view" relabeling in production. This
// pins that the only cache state a non-InterBase driver actually has (no
// Catalog) reproduces today's exact Detail/Kind for a same-named table at
// both call sites, not just the absence of new names.
func TestInterBaseViewDetailRequiresACatalogRegardlessOfDriver(t *testing.T) {
	cache := interBaseBaseCompletionCache()
	for _, driver := range []dialect.DatabaseDriver{dialect.DatabaseDriverPostgreSQL, dialect.DatabaseDriverMySQL} {
		t.Run(string(driver), func(t *testing.T) {
			complete := func(text string) []language.CompletionItem {
				return completeAt(t, otherDriverCompletionRequest(cache, driver, text), len(text))
			}

			// tableCandidates: the FROM position.
			from := complete("select * from ")
			if item := soleCandidate(t, from, "MYVIEW"); item.Detail != "table" || item.Kind != language.KindClass {
				t.Errorf("FROM: MYVIEW = %+v, want Detail=table Kind=KindClass", item)
			}

			// referencedTableCandidates: the referenced-table position.
			where := complete("select * from MYVIEW where ")
			if item := soleCandidate(t, where, "MYVIEW"); item.Detail != "referenced table" || item.Kind != language.KindClass {
				t.Errorf(`WHERE: MYVIEW = %+v, want Detail="referenced table" Kind=KindClass`, item)
			}
		})
	}
}

func TestInterBaseJoinMatchesUppercaseCatalogNames(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "lowercase tables with bare aliases",
			text: "select * from child c join parent p on ",
			want: "p.PARENT_A = c.CHILD_A AND p.PARENT_B = c.CHILD_B",
		},
		{
			name: "mixed case tables with as aliases",
			text: "SELECT * FROM Child AS c JOIN Parent AS p ON ",
			want: "p.PARENT_A = c.CHILD_A AND p.PARENT_B = c.CHILD_B",
		},
		{
			name: "mixed case tables without aliases",
			text: "SELECT * FROM Child JOIN Parent ON ",
			want: "Parent.PARENT_A = Child.CHILD_A AND Parent.PARENT_B = Child.CHILD_B",
		},
		{
			name: "mixed case tables with mixed case aliases",
			text: "select * from cHiLd childAlias join pArEnT parentAlias on ",
			want: "parentAlias.PARENT_A = childAlias.CHILD_A AND parentAlias.PARENT_B = childAlias.CHILD_B",
		},
		{
			name: "lowercase tables with bare aliases at join clause",
			text: "select * from child c join ",
			want: "PARENT P1 ON P1.PARENT_A = c.CHILD_A AND P1.PARENT_B = c.CHILD_B",
		},
		{
			name: "mixed case table with as alias at join clause",
			text: "SELECT * FROM Child AS c JOIN ",
			want: "PARENT P1 ON P1.PARENT_A = c.CHILD_A AND P1.PARENT_B = c.CHILD_B",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := interBaseCompletionRequest(interBaseJoinCompletionCache(), tt.text, dialect.SQLVariantInterBase1)
			got := completeAt(t, req, len(tt.text))
			if !completionLabels(got)[tt.want] {
				t.Fatalf("missing composite foreign-key completion %q in %v", tt.want, completionLabels(got))
			}
		})
	}
}

func TestInterBaseJoinEscapesDollarInSnippet(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantLabel  string
		wantInsert string
	}{
		{
			name:       "join clause",
			text:       "select * from child$2 c$2 join ",
			wantLabel:  "PARENT$2 P1 ON P1.PARENT_ID$2 = c$2.CHILD_ID$2",
			wantInsert: "PARENT\\$2 ${1:P1} ON ${1:P1}.PARENT_ID\\$2 = c\\$2.CHILD_ID\\$2$0",
		},
		{
			name:       "join condition",
			text:       "select * from child$2 c$2 join parent$2 p$2 on ",
			wantLabel:  "p$2.PARENT_ID$2 = c$2.CHILD_ID$2",
			wantInsert: "p\\$2.PARENT_ID\\$2 = c\\$2.CHILD_ID\\$2$0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := interBaseCompletionRequest(interBaseDollarJoinCompletionCache(), tt.text, dialect.SQLVariantInterBase1)
			got := completeAt(t, req, len(tt.text))

			var candidate *language.CompletionItem
			for i := range got {
				if got[i].Label == tt.wantLabel {
					candidate = &got[i]
					break
				}
			}
			if candidate == nil {
				t.Fatalf("missing completion label %q in %v", tt.wantLabel, completionLabels(got))
			}
			if candidate.InsertText != tt.wantInsert {
				t.Fatalf("completion %q InsertText = %q, want %q", tt.wantLabel, candidate.InsertText, tt.wantInsert)
			}
		})
	}
}

func TestInterBaseKeywordsByVariant(t *testing.T) {
	tests := []struct {
		name    string
		variant dialect.SQLVariant
		prefix  string
		want    []string
		absent  []string
	}{
		{
			name:    "dialect 1 has no TIME or TIMESTAMP type",
			variant: dialect.SQLVariantInterBase1,
			absent:  []string{"TIME", "TIMESTAMP"},
		},
		{
			// Absence alone would also hold if completion returned nothing at
			// all, so this case proves the pipeline runs under dialect 1.
			// TRIGGER is in the shared InterBase keyword list, not the
			// dialect 3 delta, so it must be offered under both variants.
			name:    "dialect 1 still offers its own keywords",
			variant: dialect.SQLVariantInterBase1,
			prefix:  "TRI",
			want:    []string{"TRIGGER"},
		},
		{
			name:    "dialect 3 offers TIME and TIMESTAMP",
			variant: dialect.SQLVariantInterBase3,
			want:    []string{"TIME", "TIMESTAMP"},
		},
		{
			name:    "dialect 3 keeps the shared keywords too",
			variant: dialect.SQLVariantInterBase3,
			prefix:  "TRI",
			want:    []string{"TRIGGER"},
		},
		{
			name:    "the default variant is dialect 3",
			variant: dialect.SQLVariantDefault,
			want:    []string{"TIME", "TIMESTAMP"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := tt.prefix
			if text == "" {
				text = "TIM"
			}
			req := interBaseCompletionRequest(interBaseCompletionCatalogCache(), text, tt.variant)
			got := completeAt(t, req, len(text))

			labels := completionLabels(got)
			for _, want := range tt.want {
				if !labels[want] {
					t.Errorf("missing keyword %q in %v", want, labels)
				}
			}
			for _, absent := range tt.absent {
				if labels[absent] {
					t.Errorf("unexpected keyword %q in %v", absent, labels)
				}
			}
		})
	}
}

func TestInterBaseDialect1DollarIdentifier(t *testing.T) {
	const text = "select rdb$ from rdb$database"
	req := interBaseCompletionRequest(interBaseDialectCompletionCache(), text, dialect.SQLVariantInterBase1)
	got := completeAt(t, req, 11)

	labels := completionLabels(got)
	if !labels["RDB$RELATION_ID"] {
		t.Fatalf("missing InterBase catalog column in completions: %v", labels)
	}
	if labels["RDB_OTHER"] {
		t.Fatalf("completion prefix should include '$': %v", labels)
	}
}

func TestInterBaseDialect1KeepsLowercaseUserNames(t *testing.T) {
	const text = "select u. from users u"
	req := interBaseCompletionRequest(interBaseDialectCompletionCache(), text, dialect.SQLVariantInterBase1)
	got := completeAt(t, req, 9)

	if !completionLabels(got)["id"] {
		t.Fatalf("missing lowercase user column in completions: %v", completionLabels(got))
	}
}

func TestInterBaseDialect3DelimitedIdentifierPrefix(t *testing.T) {
	tests := []struct {
		name    string
		variant dialect.SQLVariant
	}{
		{name: "dialect 3 treats the double quote as an identifier start", variant: dialect.SQLVariantInterBase3},
		{name: "dialect 1 treats it as a string", variant: dialect.SQLVariantInterBase1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const text = `select rdb$ from rdb$database`
			req := interBaseCompletionRequest(interBaseDialectCompletionCache(), text, tt.variant)
			got := completeAt(t, req, 11)
			// Catalog column completion must keep working under both dialects:
			// the '$' word pattern is a driver property, not a dialect one.
			if !completionLabels(got)["RDB$RELATION_ID"] {
				t.Fatalf("missing InterBase catalog column in completions: %v", completionLabels(got))
			}
		})
	}
}
