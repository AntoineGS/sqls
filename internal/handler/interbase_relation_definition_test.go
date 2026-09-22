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

func TestInterBaseRelationDefinitionWritesColumnSnapshotRange(t *testing.T) {
	server := newDefinitionServer(t)
	ddl := `CREATE TABLE "CUSTOMERINVOICE" ("AMOUNTPAID" NUMERIC(15,2), "BALANCE" INTEGER, "INVOICE" INTEGER)`
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
