package sqlsymbol

import (
	"strings"
	"testing"
)

type singletonTestCatalog struct {
	widthTestCatalog
	keys map[string][][]string
}

func (c singletonTestCatalog) UniqueKeys(table Name) ([][]string, bool) {
	keys, ok := c.keys[table.Key()]
	return keys, ok
}

func TestSingletonSelectDiagnostics(t *testing.T) {
	catalog := singletonTestCatalog{
		widthTestCatalog: widthTestCatalog{
			"ORDERS":    {{Name: "ID", Type: "INTEGER"}, {Name: "CUSTOMER_ID", Type: "INTEGER"}, {Name: "CODE", Type: "VARCHAR(20)"}},
			"CUSTOMERS": {{Name: "ID", Type: "INTEGER"}, {Name: "NAME", Type: "VARCHAR(20)"}},
			"LINES":     {{Name: "ORDER_ID", Type: "INTEGER"}, {Name: "LINE_NO", Type: "INTEGER"}},
			"LOGS":      {{Name: "ID", Type: "INTEGER"}},
			"OddTable":  {{Name: "Key", Type: "INTEGER"}},
		},
		keys: map[string][][]string{
			"ORDERS": {{"ID"}, {"CODE"}}, "CUSTOMERS": {{"ID"}},
			"LINES": {{"ORDER_ID", "LINE_NO"}}, "LOGS": {}, "OddTable": {{"Key"}},
		},
	}
	// These cases catch lost key segments, incorrect equality propagation,
	// and treating an optional join's ON clause as a preserved-side filter.
	tests := []struct {
		name string
		sql  string
		warn bool
	}{
		{"unfiltered", "SELECT ID FROM ORDERS INTO :RESULT;", true},
		{"into before from", "SELECT ID INTO :RESULT FROM ORDERS;", true},
		{"primary key", "SELECT ID FROM ORDERS WHERE ID = :INPUT_ID INTO :RESULT;", false},
		{"unique alternative", "SELECT ID FROM ORDERS WHERE CODE = 'one' INTO :RESULT;", false},
		{"nonunique", "SELECT ID FROM ORDERS WHERE CUSTOMER_ID = :INPUT_ID INTO :RESULT;", true},
		{"no keys", "SELECT ID FROM LOGS WHERE ID = 1 INTO :RESULT;", true},
		{"composite incomplete", "SELECT LINE_NO FROM LINES WHERE ORDER_ID = 1 INTO :RESULT;", true},
		{"composite complete", "SELECT LINE_NO FROM LINES WHERE (ORDER_ID = :INPUT_ID AND 2 = LINE_NO) INTO :RESULT;", false},
		{"inequality", "SELECT ID FROM ORDERS WHERE ID > 1 INTO :RESULT;", true},
		{"nullable key equality", "SELECT ID FROM ORDERS WHERE CODE = :INPUT_CODE INTO :RESULT;", false},
		{"nullable key null", "SELECT ID FROM ORDERS WHERE CODE IS NULL INTO :RESULT;", true},
		{"self equality", "SELECT ID FROM ORDERS WHERE ID = ID INTO :RESULT;", true},
		{"same row nonkey", "SELECT ID FROM ORDERS WHERE ID = CUSTOMER_ID INTO :RESULT;", true},
		{"additional filter", "SELECT ID FROM ORDERS WHERE ID = 1 AND CUSTOMER_ID > 2 INTO :RESULT;", false},
		{"transitive key binding", "SELECT ID FROM ORDERS WHERE ID = CUSTOMER_ID AND CUSTOMER_ID = 1 INTO :RESULT;", false},
		{"quoted names", `SELECT "Key" FROM "OddTable" WHERE "Key" = 1 INTO :RESULT;`, false},
		{"quoted unrestricted", `SELECT "Key" FROM "OddTable" INTO :RESULT;`, true},
		{"quoted mismatch", `SELECT "Key" FROM "oddtable" INTO :RESULT;`, false},
		{"multiple targets", "SELECT ID, CUSTOMER_ID FROM ORDERS INTO :RESULT, :OTHER_RESULT;", true},
		{"distinct not singleton", "SELECT DISTINCT CUSTOMER_ID FROM ORDERS INTO :RESULT;", true},
		{"distinct constant", "SELECT DISTINCT 1 FROM ORDERS INTO :RESULT;", false},
		{"aggregate", "SELECT MAX(ID) FROM ORDERS INTO :RESULT;", false},
		{"count", "SELECT COUNT(*) FROM ORDERS INTO :RESULT;", false},
		{"grouped aggregate", "SELECT MAX(ID) FROM ORDERS GROUP BY CUSTOMER_ID INTO :RESULT;", true},
		{"row limit", "SELECT ID FROM ORDERS ROWS 1 INTO :RESULT;", false},
		{"larger limit", "SELECT ID FROM ORDERS ROWS 2 INTO :RESULT;", true},
		{"ordered limit", "SELECT ID FROM ORDERS ORDER BY ID DESC ROWS 1 INTO :RESULT;", false},
		{"inner many to one", "SELECT c.ID FROM ORDERS o JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID WHERE o.ID = 1 INTO :RESULT;", false},
		{"inner reverse anchor", "SELECT c.ID FROM CUSTOMERS c JOIN ORDERS o ON c.ID = o.CUSTOMER_ID WHERE o.ID = 1 INTO :RESULT;", false},
		{"inner fanout", "SELECT l.LINE_NO FROM ORDERS o JOIN LINES l ON l.ORDER_ID = o.ID WHERE o.ID = 1 INTO :RESULT;", true},
		{"inner composite", "SELECT l.LINE_NO FROM ORDERS o JOIN LINES l ON l.ORDER_ID = o.ID AND l.LINE_NO = 1 WHERE o.ID = 1 INTO :RESULT;", false},
		{"inner composite transitive", "SELECT l.LINE_NO FROM ORDERS o JOIN LINES l ON l.ORDER_ID = o.CUSTOMER_ID AND l.LINE_NO = 1 WHERE o.CUSTOMER_ID = 1 AND o.ID = 2 INTO :RESULT;", false},
		{"inner no anchor", "SELECT c.ID FROM ORDERS o JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID INTO :RESULT;", true},
		{"self join", "SELECT b.ID FROM ORDERS a JOIN ORDERS b ON b.ID = a.CUSTOMER_ID WHERE a.ID = 1 INTO :RESULT;", false},
		{"left many to one", "SELECT c.ID FROM ORDERS o LEFT JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID WHERE o.ID = 1 INTO :RESULT;", false},
		{"left fanout", "SELECT l.LINE_NO FROM ORDERS o LEFT OUTER JOIN LINES l ON l.ORDER_ID = o.ID WHERE o.ID = 1 INTO :RESULT;", true},
		{"left on does not filter root", "SELECT c.ID FROM ORDERS o LEFT JOIN CUSTOMERS c ON o.ID = 1 AND c.ID = o.CUSTOMER_ID INTO :RESULT;", true},
		{"left on constant optional", "SELECT c.ID FROM ORDERS o LEFT JOIN CUSTOMERS c ON c.ID = 1 INTO :RESULT;", true},
		{"left where optional key does not bind root", "SELECT c.ID FROM ORDERS o LEFT JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID WHERE c.ID = 1 INTO :RESULT;", true},
		{"left chain", "SELECT l.LINE_NO FROM ORDERS o LEFT JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID LEFT JOIN LINES l ON l.ORDER_ID = c.ID AND l.LINE_NO = 1 WHERE o.ID = 1 INTO :RESULT;", false},
		{"left chain fanout", "SELECT l.LINE_NO FROM ORDERS o LEFT JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID LEFT JOIN LINES l ON l.ORDER_ID = c.ID WHERE o.ID = 1 INTO :RESULT;", true},
		{"left on transitive does not filter root", "SELECT c.ID FROM ORDERS o LEFT JOIN CUSTOMERS c ON o.ID = o.CUSTOMER_ID AND o.CUSTOMER_ID = 1 AND c.ID = 1 INTO :RESULT;", true},
		{"for loop", "FOR SELECT ID FROM ORDERS INTO :RESULT DO BEGIN OTHER_RESULT = RESULT; END", false},
		{"ordinary select", "SELECT ID FROM ORDERS;", false},
		{"unknown table", "SELECT ID FROM MISSING INTO :RESULT;", false},
		{"unknown column", "SELECT ID FROM ORDERS WHERE MISSING = 1 INTO :RESULT;", false},
		{"ambiguous column", "SELECT o.ID FROM ORDERS o JOIN CUSTOMERS c ON ID = 1 INTO :RESULT;", false},
		{"or unsupported", "SELECT ID FROM ORDERS WHERE ID = 1 OR ID = 2 INTO :RESULT;", false},
		{"function unsupported", "SELECT ID FROM ORDERS WHERE ABS(ID) = 1 INTO :RESULT;", false},
		{"union unsupported", "SELECT ID FROM ORDERS UNION SELECT ID FROM LOGS INTO :RESULT;", false},
		{"derived unsupported", "SELECT x.ID FROM (SELECT ID FROM ORDERS) x INTO :RESULT;", false},
		{"right join unsupported", "SELECT o.ID FROM ORDERS o RIGHT JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID INTO :RESULT;", false},
		{"full join unsupported", "SELECT o.ID FROM ORDERS o FULL JOIN CUSTOMERS c ON c.ID = o.CUSTOMER_ID INTO :RESULT;", false},
		{"missing predicate", "SELECT ID FROM ORDERS WHERE INTO :RESULT;", false},
		{"unfinished equality", "SELECT ID FROM ORDERS WHERE ID = INTO :RESULT;", false},
		{"missing join predicate", "SELECT o.ID FROM ORDERS o JOIN CUSTOMERS c ON INTO :RESULT;", false},
		{"missing terminator", "SELECT ID FROM ORDERS INTO :RESULT", false},
		{"missing target", "SELECT ID FROM ORDERS INTO;", false},
		{"missing alias", "SELECT ID FROM ORDERS AS INTO :RESULT;", false},
		{"alias reuse", "SELECT o.ID FROM ORDERS o JOIN CUSTOMERS o ON o.ID = 1 INTO :RESULT;", false},
		{"unresolved parameter", "SELECT ID FROM ORDERS WHERE ID = :MISSING INTO :RESULT;", false},
		{"missing group by", "SELECT MAX(ID) FROM ORDERS GROUP INTO :RESULT;", false},
		{"bare targets", "SELECT ID FROM ORDERS INTO RESULT;", true},
		{"unbound target", "SELECT ID FROM ORDERS INTO :MISSING;", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := "CREATE PROCEDURE P (INPUT_ID INTEGER, INPUT_CODE VARCHAR(20)) RETURNS (RESULT INTEGER, OTHER_RESULT INTEGER) AS BEGIN " + tt.sql + " END"
			a, err := AnalyzeDiagnostics(text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			var found []Finding
			for _, f := range a.Diagnostics(catalog) {
				if f.Code == "interbase-singleton-select" {
					found = append(found, f)
				}
			}
			want := 0
			if tt.warn {
				want = 1
			}
			if len(found) != want {
				t.Fatalf("singleton findings = %+v, want %d", found, want)
			}
			if len(found) > 0 {
				if found[0].Severity != 2 || text[found[0].Span.Start:found[0].Span.End] != "SELECT" || !strings.Contains(found[0].Message, "multiple rows") {
					t.Fatalf("invalid warning: %+v", found[0])
				}
			}
		})
	}
}

func TestSingletonSelectWithoutKeyMetadata(t *testing.T) {
	text := "CREATE PROCEDURE P RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM ORDERS INTO :RESULT; END"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	for _, catalog := range []Catalog{nil, widthTestCatalog{"ORDERS": {{Name: "ID", Type: "INTEGER"}}}} {
		for _, f := range a.Diagnostics(catalog) {
			if f.Code == "interbase-singleton-select" {
				t.Fatalf("unexpected warning without key metadata: %+v", f)
			}
		}
	}
}

// A scalar UDF changes a value, not the number of rows produced by FROM.
// CONFIG_NAME alone does not cover BRANCH_CONFIG's composite primary key.
func TestSingletonSelectScalarFunctionProjectionDoesNotHideMissingKey(t *testing.T) {
	catalog := singletonTestCatalog{
		widthTestCatalog: widthTestCatalog{
			"BRANCH_CONFIG": {
				{Name: "BRANCHID", Type: "VARCHAR(2)"},
				{Name: "CONFIG_NAME", Type: "VARCHAR(80)"},
				{Name: "CONFIG_VALUE", Type: "VARCHAR(80)"},
			},
		},
		keys: map[string][][]string{"BRANCH_CONFIG": {{"BRANCHID", "CONFIG_NAME"}}},
	}
	tests := []struct {
		name, statement string
		warn            bool
	}{
		{
			name:      "missing branch id",
			statement: "SELECT F_LEFT(config_value, 1) FROM branch_config WHERE config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS' into :AllowNoPayments;",
			warn:      true,
		},
		{
			name:      "full composite key",
			statement: "SELECT F_LEFT(config_value, 1) FROM branch_config WHERE config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS' AND branchid = '00' into :AllowNoPayments;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := "ALTER PROCEDURE IMPORTEXTERNALORDER AS DECLARE VARIABLE AllowNoPayments CHAR(1); BEGIN " + tt.statement + " END"
			analysis, err := AnalyzeDiagnostics(text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			var found []Finding
			for _, finding := range analysis.Diagnostics(catalog) {
				if finding.Code == "interbase-singleton-select" {
					found = append(found, finding)
				}
			}
			want := 0
			if tt.warn {
				want = 1
			}
			if len(found) != want {
				t.Fatalf("singleton warnings = %+v, want %d", found, want)
			}
			if tt.warn && text[found[0].Span.Start:found[0].Span.End] != "SELECT" {
				t.Fatalf("warning span = %q, want SELECT", text[found[0].Span.Start:found[0].Span.End])
			}
		})
	}
}
