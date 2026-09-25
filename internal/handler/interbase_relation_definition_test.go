package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

func relationCatalog() *database.DBCache {
	return &database.DBCache{
		SchemaTables: map[string][]string{"": {"CUSTOMERINVOICE"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{"\tCUSTOMERINVOICE": {
			{ColumnBase: database.ColumnBase{Table: "CUSTOMERINVOICE", Name: "AMOUNTPAID"}},
			{ColumnBase: database.ColumnBase{Table: "CUSTOMERINVOICE", Name: "BALANCE"}},
			{ColumnBase: database.ColumnBase{Table: "CUSTOMERINVOICE", Name: "INVOICE"}},
		}},
		Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataReady},
	}
}

func TestResolveRelationTargetOwnershipAndShadowing(t *testing.T) {
	cases := []struct {
		name, sql string
		offset    int
		role      sqlsymbol.Role
		wantOK    bool
		want      string
	}{
		{"table", "UPDATE CUSTOMERINVOICE SET BALANCE = 1", 8, sqlsymbol.Relation, true, "CUSTOMERINVOICE"},
		{"column", "UPDATE CUSTOMERINVOICE SET BALANCE = 1", 32, sqlsymbol.Column, true, "CUSTOMERINVOICE"},
		{"inner unknown qualifier shadows outer", "SELECT c.BALANCE FROM CUSTOMERINVOICE c WHERE EXISTS (SELECT 1 FROM MISSING c WHERE c.BALANCE = 1)", 87, sqlsymbol.Column, false, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a, err := sqlsymbol.Analyze(tt.sql, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
			if err != nil {
				t.Fatal(err)
			}
			r := a.Resolve(tt.offset)
			if r.Role != tt.role || r.SQL == nil {
				t.Fatalf("resolution = %+v", r)
			}
			got, ok := resolveRelationTarget(*r.SQL, r.Role, relationCatalog())
			if ok != tt.wantOK {
				t.Fatalf("resolveRelationTarget = %+v, %v want ok=%v", got, ok, tt.wantOK)
			}
			if ok && got.name != tt.want {
				t.Errorf("target name %q, want %q", got.name, tt.want)
			}
		})
	}
}

func TestResolveRelationTargetWaitsForViewsBeforeTableFallback(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"V"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{"\tV": {
			{ColumnBase: database.ColumnBase{Table: "V", Name: "ID"}},
		}},
		Catalog:  &database.CatalogCache{Views: map[string]*database.ViewDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataLoading},
	}
	text := "SELECT * FROM V"
	a, err := sqlsymbol.Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(strings.Index(text, "V"))
	if _, ok := resolveRelationTarget(*r.SQL, r.Role, cache); ok {
		t.Fatal("relation with unresolved view category was classified as a table")
	}
	cache.Metadata[database.MetadataViews] = database.MetadataReady
	got, ok := resolveRelationTarget(*r.SQL, r.Role, cache)
	if !ok || got.kind != database.ObjectKindTable || got.name != "V" {
		t.Fatalf("ready-empty views table fallback = %+v, %v", got, ok)
	}
}

func TestProvenDefinitionTargetRequiresCompleteMetadata(t *testing.T) {
	proof := sqlsymbol.DefinitionColumn{Relation: sqlsymbol.Name{Text: "DATABASEID"}, Column: sqlsymbol.Name{Text: "DBID"}}
	column := &database.ColumnDesc{ColumnBase: database.ColumnBase{Table: "DATABASEID", Name: "DBID"}}
	base := func(viewState, columnState database.MetadataState, descriptors []*database.ColumnDesc) *database.DBCache {
		return &database.DBCache{
			SchemaTables:      map[string][]string{"": {"DATABASEID"}},
			ColumnsWithParent: map[string][]*database.ColumnDesc{"\tDATABASEID": descriptors},
			Metadata: map[database.MetadataKind]database.MetadataState{
				database.MetadataViews: viewState, database.MetadataColumnsCurrent: columnState,
			},
		}
	}
	tests := []struct {
		name  string
		cache *database.DBCache
		want  bool
	}{
		{name: "complete table", cache: base(database.MetadataReady, database.MetadataReady, []*database.ColumnDesc{column}), want: true},
		{name: "loading with partial positive descriptor", cache: base(database.MetadataReady, database.MetadataLoading, []*database.ColumnDesc{column})},
		{name: "failed with partial positive descriptor", cache: base(database.MetadataReady, database.MetadataFailed, []*database.ColumnDesc{column})},
		{name: "missing column", cache: base(database.MetadataReady, database.MetadataReady, []*database.ColumnDesc{{ColumnBase: database.ColumnBase{Table: "DATABASEID", Name: "OTHER"}}})},
		{name: "view category incomplete", cache: &database.DBCache{Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{"V": {Name: "V", Columns: []*database.ColumnDesc{column}}}}, Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataLoading}}},
		{name: "view columns incomplete", cache: &database.DBCache{Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{"V": {Name: "V"}}}, Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataReady}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := proof
			if strings.HasPrefix(tt.name, "view") {
				candidate.Relation = sqlsymbol.Name{Text: "V"}
			}
			_, got := provenDefinitionTarget(candidate, tt.cache)
			if got != tt.want {
				t.Fatalf("provenDefinitionTarget = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInterBaseRelationDefinitionWritesColumnSnapshotRange(t *testing.T) {
	server := newDefinitionServer(t)
	ddl := `CREATE TABLE "CUSTOMERINVOICE" ("AMOUNTPAID" NUMERIC(15,2) /* normalized legacy scaled DOUBLE */, "BALANCE" INTEGER, "INVOICE" INTEGER)`
	repo := newStubDDLRepository(func(_ context.Context, kind database.ObjectKind, name string) (string, error) {
		if kind != database.ObjectKindTable || name != "CUSTOMERINVOICE" {
			t.Errorf("ObjectDDL(%q,%q)", kind, name)
		}
		return ddl, nil
	})
	sqlText := "UPDATE CUSTOMERINVOICE SET BALANCE = INVOICE WHERE AMOUNTPAID > 0"
	pos := lsp.Position{Line: 0, Character: strings.Index(sqlText, "BALANCE") + 1}
	got, err := server.interBaseRelationDefinition(context.Background(), repo, relationCatalog(), sqlText, pos,
		dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0].URI, "file://") {
		t.Fatalf("definition = %+v", got)
	}
	path, err := url.PathUnescape(strings.TrimPrefix(got[0].URI, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantOffset := strings.Index(ddl, `"BALANCE"`)
	want := lsp.Range{Start: lsp.Position{Line: 3, Character: utf16Len(ddl[:wantOffset])}, End: lsp.Position{Line: 3, Character: utf16Len(ddl[:wantOffset]) + len(`"BALANCE"`)}}
	// All text is on the DDL's first line after the banner; derive the UTF-16
	// character position from the declaration's line-local byte offset.
	if got[0].Range.Start.Line != 3 || string(content) == "" {
		t.Fatalf("range=%+v, snapshot=%s", got[0].Range, content)
	}
	if got[0].Range.Start.Character != utf16Len(ddl[:wantOffset]) || got[0].Range.End.Character != utf16Len(ddl[:wantOffset])+len(`"BALANCE"`) {
		t.Errorf("range=%+v want %+v", got[0].Range, want)
	}
	if !strings.Contains(string(content), `CREATE TABLE "CUSTOMERINVOICE"`) {
		t.Errorf("snapshot missing DDL: %s", content)
	}
	if !strings.Contains(string(content), "normalized legacy scaled DOUBLE") {
		t.Errorf("strict-first navigation lost inline Dialect 1 type provenance: %s", content)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRelationTargetRequiresCandidateMetadata(t *testing.T) {
	text := "SELECT BALANCE FROM CUSTOMERINVOICE ci JOIN UNKNOWN u ON 1=1"
	a, err := sqlsymbol.Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(strings.Index(text, "BALANCE"))
	if r.SQL == nil {
		t.Fatalf("resolution = %+v", r)
	}
	if _, ok := resolveRelationTarget(*r.SQL, sqlsymbol.Column, relationCatalog()); ok {
		t.Fatal("resolved with unavailable candidate metadata")
	}
}

func TestResolveRelationTargetRejectsUnknownColumn(t *testing.T) {
	text := "SELECT ci.NOTACOLUMN FROM CUSTOMERINVOICE ci"
	a, err := sqlsymbol.Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(strings.Index(text, "NOTACOLUMN"))
	if r.SQL == nil {
		t.Fatalf("resolution = %+v", r)
	}
	if _, ok := resolveRelationTarget(*r.SQL, r.Role, relationCatalog()); ok {
		t.Fatal("resolved a column absent from available catalog metadata")
	}
}

func TestInterBaseRelationDefinitionRequiresCatalogAndDDLCapability(t *testing.T) {
	text := "SELECT ci.BALANCE FROM CUSTOMERINVOICE ci"
	pos := lsp.Position{Character: strings.Index(text, "BALANCE") + 1}
	for _, tt := range []struct {
		name  string
		repo  database.DBRepository
		cache *database.DBCache
	}{
		{name: "no repository", cache: relationCatalog()},
		{name: "no cache", repo: newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) { return "", nil })},
		{name: "no DDL capability", repo: &database.MockDBRepository{}, cache: relationCatalog()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newDefinitionServer(t)
			got, err := server.interBaseRelationDefinition(context.Background(), tt.repo, tt.cache, text, pos,
				dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
			if err != nil || len(got) != 0 {
				t.Fatalf("definition = %+v, %v; want no locations", got, err)
			}
			if !snapshotRootIsEmpty(t, server.snapshots) {
				t.Fatal("snapshot written without required metadata/capability")
			}
		})
	}
}

func TestResolveRelationTargetRejectsAmbiguousColumnOwners(t *testing.T) {
	text := "SELECT BALANCE FROM CUSTOMERINVOICE ci JOIN CUSTOMERINVOICE other ON 1=1"
	a, err := sqlsymbol.Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(strings.Index(text, "BALANCE"))
	if r.SQL == nil {
		t.Fatalf("resolution = %+v", r)
	}
	if _, ok := resolveRelationTarget(*r.SQL, sqlsymbol.Column, relationCatalog()); ok {
		t.Fatal("resolved a column with multiple catalog owners")
	}
}

func TestResolveRelationTargetDoesNotFallThroughUnknownDerivedBinding(t *testing.T) {
	text := "SELECT c.BALANCE FROM CUSTOMERINVOICE c WHERE EXISTS (SELECT 1 FROM (SELECT 1) c WHERE c.BALANCE = 1)"
	columnOffset := strings.LastIndex(text, "BALANCE")
	a, err := sqlsymbol.Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(columnOffset)
	if r.Role != sqlsymbol.Column || r.SQL == nil {
		t.Fatalf("resolution = %+v", r)
	}
	if _, ok := resolveRelationTarget(*r.SQL, r.Role, relationCatalog()); ok {
		t.Fatal("inner derived binding fell through to outer CUSTOMERINVOICE alias")
	}
}

func TestResolveRelationTargetRejectsDuplicateQualifierBindings(t *testing.T) {
	text := "SELECT c.BALANCE FROM CUSTOMERINVOICE c JOIN CUSTOMERINVOICE c ON 1=1"
	a, err := sqlsymbol.Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(strings.Index(text, "BALANCE"))
	if r.SQL == nil {
		t.Fatalf("resolution = %+v", r)
	}
	if _, ok := resolveRelationTarget(*r.SQL, r.Role, relationCatalog()); ok {
		t.Fatal("resolved through one of multiple same-scope bindings for qualifier c")
	}
}

func TestResolveRelationTargetMatchesQuotedMixedCaseCatalogMetadata(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"MixedCase"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{"\tMIXEDCASE": {
			{ColumnBase: database.ColumnBase{Table: "MixedCase", Name: "CamelCol"}},
		}},
		Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataReady},
	}
	for _, tt := range []struct {
		name   string
		column string
		want   bool
	}{
		{"exact quoted spelling", `"CamelCol"`, true},
		{"wrong quoted spelling", `"camelcol"`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text := `SELECT t.` + tt.column + ` FROM "MixedCase" t`
			a, err := sqlsymbol.Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
			if err != nil {
				t.Fatal(err)
			}
			r := a.Resolve(strings.Index(text, tt.column) + 1)
			if r.SQL == nil {
				t.Fatalf("resolution = %+v", r)
			}
			_, ok := resolveRelationTarget(*r.SQL, r.Role, cache)
			if ok != tt.want {
				t.Fatalf("resolveRelationTarget ok=%v, want %v", ok, tt.want)
			}
		})
	}
}

func TestInterBaseContextualDispatchKeepsViewSnapshotSourceFallback(t *testing.T) {
	server := newDefinitionServer(t)
	cache := &database.DBCache{Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{
		"MYVIEW": {Name: "MYVIEW", ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true}},
	}}}
	repo := newStubDDLRepository(func(_ context.Context, kind database.ObjectKind, name string) (string, error) {
		if kind != database.ObjectKindView || name != "MYVIEW" {
			t.Errorf("ObjectDDL(%q, %q), want (view, MYVIEW)", kind, name)
		}
		return "", database.ErrUnsupportedDDL
	})
	text := "SELECT * FROM MYVIEW"
	got, err := server.interBaseContextualDefinition(context.Background(), repo, cache, text,
		lsp.Position{Character: strings.Index(text, "MYVIEW") + 1}, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil || len(got) != 1 || !strings.HasPrefix(got[0].URI, "file://") {
		t.Fatalf("contextual definition = %+v, %v; want a view source snapshot", got, err)
	}
	path, err := url.PathUnescape(strings.TrimPrefix(got[0].URI, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "SELECT ID FROM CITY") || !strings.Contains(string(content), "verbatim catalog source follows") {
		t.Fatalf("view source fallback missing from snapshot:\n%s", content)
	}
}

func TestInterBaseRelationDefinitionDoesNotWriteWhenDDLIsUnavailable(t *testing.T) {
	server := newDefinitionServer(t)
	repo := newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
		return "", errors.New("driver offline")
	})
	text := "SELECT ci.BALANCE FROM CUSTOMERINVOICE ci"
	got, err := server.interBaseRelationDefinition(context.Background(), repo, relationCatalog(), text,
		lsp.Position{Character: strings.Index(text, "BALANCE") + 1}, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil || len(got) != 0 {
		t.Fatalf("definition = %+v, %v; want no location", got, err)
	}
	if !snapshotRootIsEmpty(t, server.snapshots) {
		t.Fatal("snapshot was written without available table DDL")
	}
}

func TestInterBaseRelationDefinitionFallsBackToCatalogDescriptionForUnsupportedTableDDL(t *testing.T) {
	tests := []struct {
		name      string
		table     string
		column    string
		symbol    string
		query     string
		selected  string
		wantRange lsp.Range
	}{
		{
			name:     "full table name",
			table:    "IMPORT_ORDER_LINE_ITEMS",
			column:   "ITEMS_ONLY",
			symbol:   "IMPORT_ORDER_LINE_ITEMS",
			query:    "SELECT * FROM IMPORT_ORDER_LINE_ITEMS",
			selected: `"IMPORT_ORDER_LINE_ITEMS"`,
			wantRange: lsp.Range{
				Start: lsp.Position{Line: 5, Character: 13},
				End:   lsp.Position{Line: 5, Character: 38},
			},
		},
		{
			name:     "legacy scaled double column",
			table:    "IMPORT_ORDER_PAYMENT",
			column:   "AMOUNT",
			symbol:   "AMOUNT",
			query:    "SELECT p.AMOUNT FROM IMPORT_ORDER_PAYMENT p",
			selected: `"AMOUNT"`,
			wantRange: lsp.Range{
				Start: lsp.Position{Line: 7, Character: 2},
				End:   lsp.Position{Line: 7, Character: 10},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newDefinitionServer(t)
			var calls []string
			description := normalizedTableDescription(tt.table, tt.column)
			repo := newStubTableDescriptionRepository(
				func(_ context.Context, kind database.ObjectKind, name string) (string, error) {
					calls = append(calls, "ObjectDDL")
					if kind != database.ObjectKindTable || name != tt.table {
						t.Errorf("ObjectDDL(%q, %q), want (table, %q)", kind, name, tt.table)
					}
					return "", database.ErrUnsupportedDDL
				},
				func(_ context.Context, name string) (database.TableDescription, error) {
					calls = append(calls, "TableDescription")
					if name != tt.table {
						t.Errorf("TableDescription(%q), want %q", name, tt.table)
					}
					return description, nil
				},
			)

			got, err := server.interBaseRelationDefinition(context.Background(), repo, tableDescriptionCatalog(tt.table, tt.column), tt.query,
				lsp.Position{Character: strings.Index(tt.query, tt.symbol) + 1},
				dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("definition = %+v; want one catalog description location", got)
			}
			if repo.calls != 1 || len(repo.descriptionCalls) != 1 || repo.descriptionCalls[0] != tt.table {
				t.Fatalf("ObjectDDL calls=%d, TableDescription calls=%v", repo.calls, repo.descriptionCalls)
			}
			if len(calls) != 2 || calls[0] != "ObjectDDL" || calls[1] != "TableDescription" {
				t.Fatalf("catalog access order = %v, want ObjectDDL before TableDescription", calls)
			}

			content := readDefinitionSnapshot(t, got)
			for _, want := range []string{
				`"` + tt.table + `"`,
				"normalized legacy display; not recovered SQL",
				"Catalog reconstruction; unsupported table facets may be incomplete.",
				"CREATE TABLE",
			} {
				if !strings.Contains(content, want) {
					t.Errorf("snapshot missing %q:\n%s", want, content)
				}
			}
			if strings.Contains(content, "Strict executable DDL could not be reproduced") {
				t.Errorf("fallback has redundant strict-refusal banner:\n%s", content)
			}
			if got[0].Range != tt.wantRange {
				t.Fatalf("range = %+v, want %+v", got[0].Range, tt.wantRange)
			}
			start, ok := symbolOffset(content, got[0].Range.Start)
			if !ok {
				t.Fatalf("invalid range start %+v", got[0].Range.Start)
			}
			end, ok := symbolOffset(content, got[0].Range.End)
			if !ok || content[start:end] != tt.selected {
				t.Fatalf("range selects %q, want exactly %q", content[start:end], tt.selected)
			}
		})
	}
}

func TestInterBaseRelationDefinitionAcceptsExactUnquotedSQLDescriptionSpans(t *testing.T) {
	const table, column = "IMPORT_ORDER_PAYMENT", "AMOUNT"
	body := "-- Informational catalog description; not executable DDL.\nCREATE TABLE " + table + " (\n  " + column + " NUMERIC(15, 2)\n)"
	tableStart := strings.Index(body, table)
	columnStart := strings.Index(body, column)
	description := database.TableDescription{
		Body:  body,
		Table: database.DescriptionSpan{Start: tableStart, End: tableStart + len(table)},
		Columns: []database.DescriptionColumn{{
			Name: column,
			Span: database.DescriptionSpan{Start: columnStart, End: columnStart + len(column)},
		}},
	}
	server := newDefinitionServer(t)
	repo := newStubTableDescriptionRepository(
		func(context.Context, database.ObjectKind, string) (string, error) {
			return "", database.ErrUnsupportedDDL
		},
		func(context.Context, string) (database.TableDescription, error) { return description, nil },
	)
	query := "SELECT p.AMOUNT FROM IMPORT_ORDER_PAYMENT p"
	got, err := server.interBaseRelationDefinition(context.Background(), repo, tableDescriptionCatalog(table, column), query,
		lsp.Position{Character: strings.Index(query, column) + 1},
		dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1})
	if err != nil || len(got) != 1 {
		t.Fatalf("interBaseRelationDefinition() = %v, %v; want exact SQL-shaped fallback location", got, err)
	}
	content := readDefinitionSnapshot(t, got)
	if !strings.Contains(content, "CREATE TABLE "+table) || strings.Contains(content, "Strict executable DDL could not be reproduced") {
		t.Fatalf("fallback snapshot = %q; want SQL-shaped reconstruction and no redundant refusal banner", content)
	}
	start, ok := symbolOffset(content, got[0].Range.Start)
	if !ok || content[start:start+len(column)] != column {
		t.Fatalf("range does not select exact unquoted column %q", column)
	}
}

func TestInterBaseRelationDefinitionRejectsSpansIntoComments(t *testing.T) {
	const table, column = "IMPORT_ORDER_PAYMENT", "AMOUNT"
	query := "SELECT p.AMOUNT FROM IMPORT_ORDER_PAYMENT p"
	cache := tableDescriptionCatalog(table, column)
	for _, targetColumn := range []bool{false, true} {
		name := "table"
		if targetColumn {
			name = "column"
		}
		t.Run(name, func(t *testing.T) {
			description := normalizedTableDescription(table, column)
			quotedTable := `"` + table + `"`
			quotedColumn := `"` + column + `"`
			comment := "-- exact name in comment: " + quotedTable + " " + quotedColumn + "\n"
			description.Body = comment + description.Body
			description.Table.Start += len(comment)
			description.Table.End += len(comment)
			description.Columns[0].Span.Start += len(comment)
			description.Columns[0].Span.End += len(comment)
			if targetColumn {
				start := strings.Index(comment, quotedColumn)
				description.Columns[0].Span = database.DescriptionSpan{Start: start, End: start + len(quotedColumn)}
			} else {
				start := strings.Index(comment, quotedTable)
				description.Table = database.DescriptionSpan{Start: start, End: start + len(quotedTable)}
			}
			repo := newStubTableDescriptionRepository(
				func(context.Context, database.ObjectKind, string) (string, error) {
					return "", database.ErrUnsupportedDDL
				},
				func(context.Context, string) (database.TableDescription, error) { return description, nil },
			)
			pos := strings.Index(query, table) + 1
			if targetColumn {
				pos = strings.Index(query, column) + 1
			}
			got, err := newDefinitionServer(t).interBaseRelationDefinition(context.Background(), repo, cache, query,
				lsp.Position{Character: pos},
				dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1})
			if err != nil || len(got) != 0 {
				t.Fatalf("definition = %v, %v; comment span must be rejected", got, err)
			}
		})
	}
}

func TestInterBaseRelationDefinitionAcceptsUnicodeQuotedDialect3Names(t *testing.T) {
	const table, column = "DÉTAILS", "MÜNCHEN"
	quotedTable, quotedColumn := `"`+table+`"`, `"`+column+`"`
	body := "-- Informational catalog description; not executable DDL.\nCREATE TABLE " + quotedTable + " (\n  " + quotedColumn + " VARCHAR(20)\n)"
	tableStart, columnStart := strings.Index(body, quotedTable), strings.Index(body, quotedColumn)
	description := database.TableDescription{
		Body:  body,
		Table: database.DescriptionSpan{Start: tableStart, End: tableStart + len(quotedTable)},
		Columns: []database.DescriptionColumn{{Name: column,
			Span: database.DescriptionSpan{Start: columnStart, End: columnStart + len(quotedColumn)}}},
	}
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {table}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{"\t" + table: {
			{ColumnBase: database.ColumnBase{Table: table, Name: column}},
		}},
		Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataReady},
	}
	repo := newStubTableDescriptionRepository(
		func(context.Context, database.ObjectKind, string) (string, error) {
			return "", database.ErrUnsupportedDDL
		},
		func(context.Context, string) (database.TableDescription, error) { return description, nil },
	)
	query := "SELECT " + quotedColumn + " FROM " + quotedTable
	columnAt := strings.Index(query, quotedColumn)
	got, err := newDefinitionServer(t).interBaseRelationDefinition(context.Background(), repo, cache, query,
		lsp.Position{Character: utf16Len(query[:columnAt]) + 1},
		dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	if err != nil || len(got) != 1 {
		t.Fatalf("interBaseRelationDefinition() = %v, %v; want one Unicode quoted identifier location", got, err)
	}
	content := readDefinitionSnapshot(t, got)
	start, ok := symbolOffset(content, got[0].Range.Start)
	if !ok || content[start:start+len(quotedColumn)] != quotedColumn {
		t.Fatalf("Unicode range did not select exact quoted name %q", quotedColumn)
	}
}

func TestInterBaseRelationDefinitionAcceptsSupportedRelationShapes(t *testing.T) {
	const table, column = "TEMP_DATA", "PAYLOAD"
	quotedTable, quotedColumn := `"`+table+`"`, `"`+column+`"`
	shapes := []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "global temporary delete rows", prefix: "CREATE GLOBAL TEMPORARY TABLE "},
		{name: "global temporary preserve rows", prefix: "CREATE GLOBAL TEMPORARY TABLE ", suffix: " ON COMMIT PRESERVE ROWS"},
		{name: "external table", prefix: "CREATE TABLE ", suffix: " EXTERNAL FILE 'temp.dat'"},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			body := "-- Informational catalog description; not executable DDL.\n" + shape.prefix + quotedTable + shape.suffix + " (\n  " + quotedColumn + " /* type unknown: catalog type unavailable */\n)"
			tableStart, columnStart := strings.Index(body, quotedTable), strings.Index(body, quotedColumn)
			description := database.TableDescription{
				Body:  body,
				Table: database.DescriptionSpan{Start: tableStart, End: tableStart + len(quotedTable)},
				Columns: []database.DescriptionColumn{{Name: column,
					Span: database.DescriptionSpan{Start: columnStart, End: columnStart + len(quotedColumn)}}},
			}
			query := "SELECT PAYLOAD FROM TEMP_DATA"
			for _, selectColumn := range []bool{false, true} {
				targetName := "table"
				pos := strings.Index(query, table) + 1
				if selectColumn {
					targetName = "column"
					pos = strings.Index(query, column) + 1
				}
				t.Run(targetName, func(t *testing.T) {
					repo := newStubTableDescriptionRepository(
						func(context.Context, database.ObjectKind, string) (string, error) {
							return "", database.ErrUnsupportedDDL
						},
						func(context.Context, string) (database.TableDescription, error) { return description, nil },
					)
					got, err := newDefinitionServer(t).interBaseRelationDefinition(context.Background(), repo,
						tableDescriptionCatalog(table, column), query, lsp.Position{Character: pos},
						dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
					if err != nil || len(got) != 1 {
						t.Fatalf("definition = %v, %v; expected %s shape to navigate", got, err, shape.name)
					}
					if !strings.Contains(readDefinitionSnapshot(t, got), "type unknown: catalog type unavailable") {
						t.Fatalf("%s reconstruction lost the local unknown-type note", shape.name)
					}
				})
			}
		})
	}
}

func TestInterBaseRelationDefinitionRejectsBlockCommentedDeclarations(t *testing.T) {
	const table, column = "IMPORT_ORDER_PAYMENT", "AMOUNT"
	quotedTable, quotedColumn := `"`+table+`"`, `"`+column+`"`
	body := "-- Informational catalog description; not executable DDL.\n/*\nCREATE TABLE " + quotedTable + " (\n  " + quotedColumn + " INTEGER\n)\n*/"
	tableStart, columnStart := strings.Index(body, quotedTable), strings.Index(body, quotedColumn)
	description := database.TableDescription{
		Body:  body,
		Table: database.DescriptionSpan{Start: tableStart, End: tableStart + len(quotedTable)},
		Columns: []database.DescriptionColumn{{Name: column,
			Span: database.DescriptionSpan{Start: columnStart, End: columnStart + len(quotedColumn)}}},
	}
	query := "SELECT p.AMOUNT FROM IMPORT_ORDER_PAYMENT p"
	for _, selectColumn := range []bool{false, true} {
		t.Run(map[bool]string{false: "table", true: "column"}[selectColumn], func(t *testing.T) {
			pos := strings.Index(query, table) + 1
			if selectColumn {
				pos = strings.Index(query, column) + 1
			}
			repo := newStubTableDescriptionRepository(
				func(context.Context, database.ObjectKind, string) (string, error) {
					return "", database.ErrUnsupportedDDL
				},
				func(context.Context, string) (database.TableDescription, error) { return description, nil },
			)
			got, err := newDefinitionServer(t).interBaseRelationDefinition(context.Background(), repo,
				tableDescriptionCatalog(table, column), query, lsp.Position{Character: pos},
				dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
			if err != nil || len(got) != 0 {
				t.Fatalf("definition = %v, %v; block-commented declaration must be rejected", got, err)
			}
		})
	}
}

func TestInterBaseRelationDefinitionDoesNotUseCatalogDescriptionForUnsafeFallbacks(t *testing.T) {
	table := "IMPORT_ORDER_PAYMENT"
	column := "AMOUNT"
	query := "SELECT p.AMOUNT FROM IMPORT_ORDER_PAYMENT p"
	cache := tableDescriptionCatalog(table, column)
	description := normalizedTableDescription(table, column)

	tests := []struct {
		name           string
		ctx            context.Context
		withCapability bool
		ddlErr         error
		description    database.TableDescription
		descriptionErr error
		query          string
		cache          *database.DBCache
		wantDDLCalls   int
		wantDescCalls  int
	}{
		{
			name:           "object not found",
			ctx:            context.Background(),
			withCapability: true,
			ddlErr:         database.ErrObjectNotFound,
			description:    description,
			query:          query,
			cache:          cache,
			wantDDLCalls:   1,
		},
		{
			name:           "driver offline",
			ctx:            context.Background(),
			withCapability: true,
			ddlErr:         errors.New("driver offline"),
			description:    description,
			query:          query,
			cache:          cache,
			wantDDLCalls:   1,
		},
		{
			name:         "unsupported DDL without description capability",
			ctx:          context.Background(),
			ddlErr:       database.ErrUnsupportedDDL,
			query:        query,
			cache:        cache,
			wantDDLCalls: 1,
		},
		{
			name:           "canceled description read",
			ctx:            canceledDefinitionContext(t),
			withCapability: true,
			ddlErr:         database.ErrUnsupportedDDL,
			descriptionErr: context.Canceled,
			query:          query,
			cache:          cache,
			wantDDLCalls:   1,
			wantDescCalls:  1,
		},
		{
			name:           "failed description read",
			ctx:            context.Background(),
			withCapability: true,
			ddlErr:         database.ErrUnsupportedDDL,
			descriptionErr: errors.New("catalog offline"),
			query:          query,
			cache:          cache,
			wantDDLCalls:   1,
			wantDescCalls:  1,
		},
		{
			name:           "column absent from exact description",
			ctx:            context.Background(),
			withCapability: true,
			ddlErr:         database.ErrUnsupportedDDL,
			description:    normalizedTableDescription(table, "OTHER_AMOUNT"),
			query:          query,
			cache:          cache,
			wantDDLCalls:   1,
			wantDescCalls:  1,
		},
		{
			name:           "stale table identity",
			ctx:            context.Background(),
			withCapability: true,
			ddlErr:         database.ErrUnsupportedDDL,
			description:    normalizedTableDescription("OTHER_TABLE", column),
			query:          query,
			cache:          cache,
			wantDDLCalls:   1,
			wantDescCalls:  1,
		},
		{
			name:           "malformed description body",
			ctx:            context.Background(),
			withCapability: true,
			ddlErr:         database.ErrUnsupportedDDL,
			description:    descriptionWithNonCommentLine(table, column),
			query:          query,
			cache:          cache,
			wantDDLCalls:   1,
			wantDescCalls:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newDefinitionServer(t)
			ddl := func(context.Context, database.ObjectKind, string) (string, error) {
				return "", tt.ddlErr
			}
			var repo database.DBRepository
			var ddlRepo *stubDDLRepository
			var descriptionRepo *stubTableDescriptionRepository
			if tt.withCapability {
				descriptionRepo = newStubTableDescriptionRepository(ddl, func(ctx context.Context, _ string) (database.TableDescription, error) {
					if tt.name == "canceled description read" && !errors.Is(ctx.Err(), context.Canceled) {
						t.Errorf("TableDescription context error = %v, want context.Canceled", ctx.Err())
					}
					return tt.description, tt.descriptionErr
				})
				ddlRepo = descriptionRepo.stubDDLRepository
				repo = descriptionRepo
			} else {
				ddlRepo = newStubDDLRepository(ddl)
				repo = ddlRepo
			}

			got, err := server.interBaseRelationDefinition(tt.ctx, repo, tt.cache, tt.query,
				lsp.Position{Character: strings.Index(tt.query, column) + 1},
				dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1})
			if err != nil || len(got) != 0 {
				t.Fatalf("definition = %+v, %v; want no locations", got, err)
			}
			var descriptionCalls []string
			if descriptionRepo != nil {
				descriptionCalls = descriptionRepo.descriptionCalls
			}
			if ddlRepo.calls != tt.wantDDLCalls || len(descriptionCalls) != tt.wantDescCalls {
				t.Fatalf("ObjectDDL calls=%d, TableDescription calls=%v; want %d and %d", ddlRepo.calls, descriptionCalls, tt.wantDDLCalls, tt.wantDescCalls)
			}
			if !snapshotRootIsEmpty(t, server.snapshots) {
				t.Fatal("snapshot was written for an unavailable, failed, or stale catalog result")
			}
		})
	}
}

func TestInterBaseRelationDefinitionDoesNotUseCatalogDescriptionForAmbiguousColumnOwner(t *testing.T) {
	table := "IMPORT_ORDER_PAYMENT"
	column := "AMOUNT"
	query := "SELECT AMOUNT FROM IMPORT_ORDER_PAYMENT a JOIN IMPORT_ORDER_PAYMENT b ON 1 = 1"
	server := newDefinitionServer(t)
	repo := newStubTableDescriptionRepository(
		func(context.Context, database.ObjectKind, string) (string, error) {
			return "", database.ErrUnsupportedDDL
		},
		func(context.Context, string) (database.TableDescription, error) {
			return normalizedTableDescription(table, column), nil
		},
	)

	got, err := server.interBaseRelationDefinition(context.Background(), repo, tableDescriptionCatalog(table, column), query,
		lsp.Position{Character: strings.Index(query, column) + 1},
		dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1})
	if err != nil || len(got) != 0 {
		t.Fatalf("definition = %+v, %v; want no locations for ambiguous column ownership", got, err)
	}
	if repo.calls != 0 || len(repo.descriptionCalls) != 0 {
		t.Fatalf("ObjectDDL calls=%d, TableDescription calls=%v; ambiguous ownership must stop before catalog reads", repo.calls, repo.descriptionCalls)
	}
	if !snapshotRootIsEmpty(t, server.snapshots) {
		t.Fatal("snapshot was written for an ambiguous column owner")
	}
}

func tableDescriptionCatalog(table, column string) *database.DBCache {
	return &database.DBCache{
		SchemaTables: map[string][]string{"": {table}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{"\t" + table: {
			{ColumnBase: database.ColumnBase{Table: table, Name: column}},
		}},
		Metadata: map[database.MetadataKind]database.MetadataState{database.MetadataViews: database.MetadataReady},
	}
}

func normalizedTableDescription(table, column string) database.TableDescription {
	quotedTable := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	quotedColumn := `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
	body := strings.Join([]string{
		"-- Informational catalog description; not executable DDL.",
		"CREATE TABLE " + quotedTable + " (",
		"  -- Catalog note: 🐟 declaration spans identify only exact names.",
		"  " + quotedColumn + " NUMERIC(15, 2) /* normalized legacy display; not recovered SQL */",
		"  -- Type provenance: conventional numeric display normalized from legacy scaled DOUBLE catalog metadata.",
		")",
	}, "\n")
	tableStart := strings.Index(body, quotedTable)
	columnStart := strings.Index(body, quotedColumn)
	return database.TableDescription{
		Body:  body,
		Table: database.DescriptionSpan{Start: tableStart, End: tableStart + len(quotedTable)},
		Columns: []database.DescriptionColumn{{
			Name: column,
			Span: database.DescriptionSpan{Start: columnStart, End: columnStart + len(quotedColumn)},
		}},
	}
}

func descriptionWithNonCommentLine(table, column string) database.TableDescription {
	description := normalizedTableDescription(table, column)
	description.Body = strings.Replace(description.Body, "CREATE TABLE ", "BROKEN TABLE ", 1)
	return description
}

func readDefinitionSnapshot(t *testing.T, locations lsp.Definition) string {
	t.Helper()
	if len(locations) != 1 {
		t.Fatalf("definition has %d locations, want one", len(locations))
	}
	path, err := url.PathUnescape(strings.TrimPrefix(locations[0].URI, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func canceledDefinitionContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type stubTableDescriptionRepository struct {
	*stubDDLRepository
	describe         func(context.Context, string) (database.TableDescription, error)
	descriptionCalls []string
}

func (r *stubTableDescriptionRepository) TableDescription(ctx context.Context, name string) (database.TableDescription, error) {
	r.descriptionCalls = append(r.descriptionCalls, name)
	return r.describe(ctx, name)
}

func newStubTableDescriptionRepository(
	ddl func(context.Context, database.ObjectKind, string) (string, error),
	describe func(context.Context, string) (database.TableDescription, error),
) *stubTableDescriptionRepository {
	return &stubTableDescriptionRepository{
		stubDDLRepository: newStubDDLRepository(ddl),
		describe:          describe,
	}
}
