package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/parser"
)

var firebird25Corpus = []string{
	"SELECT FIRST 10 SKIP 5 * FROM RDB$RELATIONS",
	"SELECT * FROM RDB$RELATIONS ROWS 1 TO 10",
	"UPDATE OR INSERT INTO T (ID, NAME) VALUES (1, 'a') MATCHING (ID)",
	"INSERT INTO T (ID) VALUES (1) RETURNING ID",
	"MERGE INTO T USING S ON T.ID = S.ID WHEN MATCHED THEN UPDATE SET NAME = S.NAME WHEN NOT MATCHED THEN INSERT (ID, NAME) VALUES (S.ID, S.NAME)",
	"RECREATE TABLE T (ID INTEGER)",
	"CREATE OR ALTER PROCEDURE P AS BEGIN END",
	"CREATE SEQUENCE S",
	"SELECT NEXT VALUE FOR S FROM RDB$DATABASE",
	"CREATE GLOBAL TEMPORARY TABLE G (ID INTEGER) ON COMMIT PRESERVE ROWS",
	"EXECUTE BLOCK RETURNS (N INTEGER) AS BEGIN N = 1; SUSPEND; END",
	"SELECT IIF(1 = 1, 'y', 'n'), LIST(RDB$RELATION_NAME) FROM RDB$RELATIONS",
	"WITH RECURSIVE C AS (SELECT 1 AS N FROM RDB$DATABASE) SELECT N FROM C",
}

// Diagnostics reports static InterBase findings, not SQL grammar errors. Parser
// tree acceptance is covered separately below.
func TestFirebird25CorpusDiagnosticsHaveNoErrorSeverity(t *testing.T) {
	for i, statement := range firebird25Corpus {
		t.Run(statement, func(t *testing.T) {
			result, err := language.NewService().Diagnostics(context.Background(), language.Request{
				Document: language.Document{URI: "memory:firebird-corpus", Text: statement},
				Context: language.Context{
					Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.FirebirdSQLVariant(3)},
					DialectResolved: true,
				},
			})
			if err != nil {
				t.Fatalf("Diagnostics() error = %v", err)
			}
			for _, diagnostic := range result.Value {
				if diagnostic.Severity == 1 {
					t.Errorf("error-severity diagnostic for corpus statement %d: %+v", i+1, diagnostic)
				}
			}
		})
	}
}

func TestFirebird25CorpusBuildsStatementTreesAndRejectsIllegalToken(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.FirebirdSQLVariant(3)}
	for i, statement := range firebird25Corpus {
		t.Run(statement, func(t *testing.T) {
			parsed, err := parser.ParseWithDriverVariant(statement, dv)
			if err != nil {
				t.Fatalf("ParseWithDriverVariant() error = %v", err)
			}
			if !hasStatementTree(parsed) {
				t.Fatalf("corpus statement %d produced no statement tree: %#v", i+1, parsed)
			}
		})
	}

	if _, err := parser.ParseWithDriverVariant("SELECT ! FROM T", dv); err == nil {
		t.Fatal("ParseWithDriverVariant() accepted an illegal-token control without an error")
	}
}

func TestFirebirdParserAndDiagnosticsDoNotValidateGrammarTypo(t *testing.T) {
	const malformed = "SELEC * FORM T"
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.FirebirdSQLVariant(3)}
	parsed, err := parser.ParseWithDriverVariant(malformed, dv)
	if err != nil {
		t.Fatalf("ParseWithDriverVariant() error = %v; parser only rejects lexical errors", err)
	}
	if !hasStatementTree(parsed) {
		t.Fatalf("parser should return its best-effort tree for grammar typos: %#v", parsed)
	}

	result, err := language.NewService().Diagnostics(context.Background(), language.Request{
		Document: language.Document{URI: "memory:firebird-malformed-control", Text: malformed},
		Context:  language.Context{Variant: dv, DialectResolved: true},
	})
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	if len(result.Value) != 0 {
		t.Fatalf("grammar typo unexpectedly produced diagnostics: %+v", result.Value)
	}
}

func hasStatementTree(parsed ast.TokenList) bool {
	if parsed == nil || parsed.Type() != ast.TypeQuery {
		return false
	}
	for _, node := range parsed.GetTokens() {
		if node.Type() == ast.TypeStatement {
			return true
		}
	}
	return false
}
