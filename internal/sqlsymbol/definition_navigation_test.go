package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestProvenDefinitionColumnForSingleBaseRelation(t *testing.T) {
	text := "ALTER PROCEDURE P AS\nDECLARE VARIABLE DBID INTEGER;\nBEGIN\nSELECT DBID FROM DATABASEID;\nEND"
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	catalog := newDiagnosticFixtureCatalog()
	catalog.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID", Type: "INTEGER"}}, ColumnsKnown: true}
	offset := strings.Index(text, "SELECT DBID") + len("SELECT ")
	got, ok := a.ProvenDefinitionColumn(offset, catalog)
	if !ok || got.Relation.Key() != "DATABASEID" || got.Column.Key() != "DBID" {
		t.Fatalf("ProvenDefinitionColumn = (%+v, %v), want DATABASEID.DBID", got, ok)
	}
	resolution := a.Resolve(offset)
	if resolution.Role != Ambiguous || !resolution.DefinitionColumnCandidate {
		t.Fatalf("resolution = %+v, want retained Ambiguous role and definition-only candidate", resolution)
	}
}

func TestDefinitionColumnCandidateIsOnlySingleLocalValueCollision(t *testing.T) {
	tests := []struct {
		name, text string
		want       bool
	}{
		{"collision", "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID; END", true},
		{"ordinary local", "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN DBID = 1; END", false},
		{"duplicate local", "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID; END", false},
		{"output target", "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID INTO DBID; END", false},
		{"callable", "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID() FROM DATABASEID; END", false},
		{"unsupported", "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN MERGE INTO DATABASEID D USING DATABASEID S ON D.ID = S.ID WHEN MATCHED THEN UPDATE SET DBID = DBID; END", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
			if err != nil {
				t.Fatal(err)
			}
			offset := strings.Index(tt.text, "SELECT DBID") + len("SELECT ")
			if tt.name == "ordinary local" {
				offset = strings.Index(tt.text, "DBID =")
			}
			if tt.name == "output target" {
				offset = strings.Index(tt.text, "INTO DBID") + len("INTO ")
			}
			got := a.Resolve(offset).DefinitionColumnCandidate
			if got != tt.want {
				t.Fatalf("candidate = %v, want %v; resolution=%+v", got, tt.want, a.Resolve(offset))
			}
		})
	}
}

func TestProvenDefinitionColumnRejectsUnprovenShapes(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		setup func(*diagnosticFixtureCatalog)
	}{
		{name: "missing column", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID; END", setup: func(c *diagnosticFixtureCatalog) {
			c.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "OTHER"}}, ColumnsKnown: true}
		}},
		{name: "ambiguous owners", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID D JOIN OTHER O ON D.ID = O.ID; END", setup: func(c *diagnosticFixtureCatalog) {
			c.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID"}, {Name: "ID"}}, ColumnsKnown: true}
			c.relations["OTHER"] = RelationFact{Columns: []ColumnFact{{Name: "DBID"}}, ColumnsKnown: true}
		}},
		{name: "incomplete columns", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID; END", setup: func(c *diagnosticFixtureCatalog) {
			c.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID"}}, ColumnsKnown: false}
		}},
		{name: "CTE", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN WITH DATABASEID AS (SELECT DBID FROM T) SELECT DBID FROM DATABASEID; END", setup: func(c *diagnosticFixtureCatalog) {
			c.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID"}}, ColumnsKnown: true}
		}},
		{name: "derived source", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM (SELECT DBID FROM DATABASEID) D; END", setup: func(c *diagnosticFixtureCatalog) {
			c.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID"}}, ColumnsKnown: true}
		}},
		{name: "preceding DDL invalidation", text: "ALTER TABLE DATABASEID ADD X INTEGER; ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID; END", setup: func(c *diagnosticFixtureCatalog) {
			c.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID"}}, ColumnsKnown: true}
		}},
		{name: "duplicate local", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID; END", setup: func(c *diagnosticFixtureCatalog) {
			c.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID"}}, ColumnsKnown: true}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
			if err != nil {
				t.Fatal(err)
			}
			catalog := newDiagnosticFixtureCatalog()
			if tt.setup != nil {
				tt.setup(catalog)
			}
			offset := strings.Index(tt.text, "SELECT DBID") + len("SELECT ")
			if offset < len("SELECT ") {
				t.Fatal("fixture lacks target DBID")
			}
			if got, ok := a.ProvenDefinitionColumn(offset, catalog); ok {
				t.Fatalf("unexpected proof %+v", got)
			}
		})
	}
}
