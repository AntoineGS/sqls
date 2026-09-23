package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestExplainStatementsRendersOnePlanPerStatement(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(_ context.Context, query string) (string, error) {
		if strings.Contains(query, "COUNTRY") {
			return "PLAN (COUNTRY INDEX (RDB$PRIMARY7))", nil
		}
		return "PLAN (CITY NATURAL)", nil
	}

	got, err := explainStatements(context.Background(), repo, []string{
		"SELECT * FROM CITY",
		"SELECT * FROM COUNTRY",
	})
	if err != nil {
		t.Fatal("explainStatements:", err)
	}

	for _, want := range []string{
		"-- statement 1",
		"PLAN (CITY NATURAL)",
		"-- statement 2",
		"PLAN (COUNTRY INDEX (RDB$PRIMARY7))",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("explain output missing %q:\n%s", want, got)
		}
	}
	if calls := repo.ExplainPlanCalls(); len(calls) != 2 || calls[0] != "SELECT * FROM CITY" {
		t.Errorf("ExplainPlanCalls() = %v, want both statements verbatim", calls)
	}
	// A SELECT is not prepared-only news: the banner belongs to DML alone.
	if strings.Contains(got, "prepared only") {
		t.Errorf("a SELECT carried the DML banner:\n%s", got)
	}
}

func TestExplainStatementsEmptyPlanReportsNoPlan(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "", nil }

	got, err := explainStatements(context.Background(), repo, []string{"SELECT * FROM CITY"})
	if err != nil {
		t.Fatal("an empty plan is not an error:", err)
	}
	// Two independent facts, both required: the plan was empty, and nothing
	// ran. A naive implementation prints the empty string and the pane is
	// blank, which reads as a broken command.
	if !strings.Contains(got, "No plan text.") {
		t.Errorf("output does not report the empty plan:\n%s", got)
	}
	if !strings.Contains(got, "Nothing was executed.") {
		t.Errorf("output does not say nothing ran:\n%s", got)
	}
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(got), "-- statement 1")) == "" {
		t.Errorf("output is a bare header with no body:\n%q", got)
	}
}

func TestExplainStatementsDMLShowsPreparedOnlyBanner(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) {
		return "PLAN (CITY INDEX (PK_CITY))", nil
	}

	got, err := explainStatements(context.Background(), repo, []string{
		"UPDATE CITY SET NAME = 'x' WHERE ID = 1",
	})
	if err != nil {
		t.Fatal("explainStatements:", err)
	}
	if !strings.Contains(got, "-- statement 1 (prepared only; nothing was inserted, updated or deleted)") {
		t.Errorf("UPDATE is missing the prepared-only banner:\n%s", got)
	}
	if !strings.Contains(got, "PLAN (CITY INDEX (PK_CITY))") {
		t.Errorf("the plan itself is missing:\n%s", got)
	}
}

func TestExplainStatementsBannersEveryMutatingKind(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "PLAN (X NATURAL)", nil }

	for _, query := range []string{
		"INSERT INTO CITY (ID) VALUES (1)",
		"UPDATE CITY SET NAME = 'x'",
		"DELETE FROM CITY WHERE ID = 1",
		"EXECUTE PROCEDURE MYPROC(1)",
	} {
		got, err := explainStatements(context.Background(), repo, []string{query})
		if err != nil {
			t.Fatalf("explainStatements(%q): %v", query, err)
		}
		if !strings.Contains(got, "prepared only") {
			t.Errorf("%q was explained with no prepared-only banner:\n%s", query, got)
		}
	}
}

func TestExplainStatementsRefusesUnsupportedStatement(t *testing.T) {
	repo := database.NewMockCapabilityRepository()

	got, err := explainStatements(context.Background(), repo, []string{"CREATE TABLE city (id integer)"})
	if err != nil {
		t.Fatal("a refusal is rendered, not returned as an error:", err)
	}
	if !strings.Contains(got, "Explain supports SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE") {
		t.Errorf("output does not explain the refusal:\n%s", got)
	}
	// The statement kind must be named, or the user cannot tell which of
	// several statements was refused.
	if !strings.Contains(got, "CREATE TABLE") {
		t.Errorf("output does not name the refused statement kind:\n%s", got)
	}
	if calls := repo.ExplainPlanCalls(); len(calls) != 0 {
		t.Errorf("ExplainPlan was called %d times for a refused statement, want 0", len(calls))
	}
}

func TestExplainStatementsRefusalDoesNotStopLaterStatements(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "PLAN (CITY NATURAL)", nil }

	got, err := explainStatements(context.Background(), repo, []string{
		"COMMIT",
		"SELECT * FROM CITY",
	})
	if err != nil {
		t.Fatal("explainStatements:", err)
	}
	if !strings.Contains(got, "PLAN (CITY NATURAL)") {
		t.Errorf("a refused first statement suppressed the second:\n%s", got)
	}
}

func TestExplainStatementsExplainsArrayColumnFailure(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) {
		// The driver's real wording: the pooled prepare path passes
		// allow_arrays = 0, so output-type validation rejects the statement
		// instead of returning a plan (native.c:4718).
		return "", errors.New("interbase: prepare statement: array results are unsupported by database/sql")
	}

	got, err := explainStatements(context.Background(), repo, []string{"SELECT ARR FROM T"})
	if err != nil {
		t.Fatal("an array column is a known failure, not a command error:", err)
	}
	if !strings.Contains(got, "array column") {
		t.Errorf("output does not name the cause:\n%s", got)
	}
	if !strings.Contains(got, "array results are unsupported by database/sql") {
		t.Errorf("output hides the driver's own message:\n%s", got)
	}
}

func TestExplainStatementsPropagatesOtherErrors(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	sentinel := errors.New("interbase: dynamic SQL error: token unknown - line 1, column 8")
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "", sentinel }

	if _, err := explainStatements(context.Background(), repo, []string{"SELECT FROM"}); !errors.Is(err, sentinel) {
		t.Errorf("explainStatements error = %v, want the driver error", err)
	}
}

func TestExplainRepositoryForReportsTheDriverWhenUnsupported(t *testing.T) {
	_, err := explainRepositoryFor(database.NewMockDBRepository(nil))
	if err == nil {
		t.Fatal("a repository without ExplainPlan must be refused")
	}
	if !strings.Contains(err.Error(), "mock") {
		t.Errorf("error = %q, want it to name the driver", err)
	}
	if _, err := explainRepositoryFor(database.NewMockCapabilityRepository()); err != nil {
		t.Errorf("a capability repository was refused: %v", err)
	}
}

func TestExplainCodeActionIsAdvertised(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.Stop()

	tx.textDocumentDidOpen(t, testFileURI, "SELECT * FROM CITY")

	var got []lsp.Command
	if err := tx.conn.Call(tx.ctx, "textDocument/codeAction", lsp.CodeActionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call textDocument/codeAction:", err)
	}

	var found *lsp.Command
	for i, command := range got {
		if command.Command == CommandExplainQuery {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("no %q command in %+v", CommandExplainQuery, got)
	}
	if found.Title != "Explain SQL" {
		t.Errorf("title = %q, want %q", found.Title, "Explain SQL")
	}
	if len(found.Arguments) != 1 || found.Arguments[0] != testFileURI {
		t.Errorf("arguments = %v, want the document URI", found.Arguments)
	}
}
