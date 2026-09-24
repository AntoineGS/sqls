package handler

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

type signatureHelpTestCase struct {
	name  string
	input string
	line  int
	col   int
	want  lsp.SignatureHelp
}

var signatureHelpTestCases = []signatureHelpTestCase{
	// single record
	// input is "insert into city (ID, Name, CountryCode) VALUES (123,  NULL, '2020')"
	genSingleRecordInsertTest(50, 0),
	genSingleRecordInsertTest(52, 0),
	genSingleRecordInsertTest(53, 1),
	genSingleRecordInsertTest(59, 1),
	genSingleRecordInsertTest(60, 2),
	genSingleRecordInsertTest(67, 2),

	// multi record
	// input is "insert into city (ID, Name, CountryCode) VALUES (123, 'aaa', '2020'), (456, 'bbb', '2021')"
	genMultiRecordInsertTest(50, 0),
	genMultiRecordInsertTest(52, 0),
	genMultiRecordInsertTest(53, 1),
	genMultiRecordInsertTest(59, 1),
	genMultiRecordInsertTest(60, 2),
	genMultiRecordInsertTest(67, 2),

	genMultiRecordInsertTest(72, 0),
	genMultiRecordInsertTest(74, 0),
	genMultiRecordInsertTest(76, 1),
	genMultiRecordInsertTest(81, 1),
	genMultiRecordInsertTest(83, 2),
	genMultiRecordInsertTest(89, 2),
}

func genSingleRecordInsertTest(col int, wantActiveParameter int) signatureHelpTestCase {
	return signatureHelpTestCase{
		name:  fmt.Sprintf("single record %d-%d", col, wantActiveParameter),
		input: "insert into city (ID, Name, CountryCode) VALUES (123,  NULL, '2020')",
		line:  0,
		col:   col,
		want: lsp.SignatureHelp{
			Signatures: []lsp.SignatureInformation{
				{
					Label:         "city (ID, Name, CountryCode)",
					Documentation: "city table columns",
					Parameters: []lsp.ParameterInformation{
						{
							Label:         "ID",
							Documentation: "`int(11)` PRI auto_increment",
						},
						{
							Label:         "Name",
							Documentation: "`char(35)`",
						},
						{
							Label:         "CountryCode",
							Documentation: "`char(3)` MUL",
						},
					},
				},
			},
			ActiveSignature: 0.0,
			ActiveParameter: float64(wantActiveParameter),
		},
	}
}

func genMultiRecordInsertTest(col int, wantActiveParameter int) signatureHelpTestCase {
	return signatureHelpTestCase{
		name:  fmt.Sprintf("multi record %d-%d", col, wantActiveParameter),
		input: "insert into city (ID, Name, CountryCode) VALUES (123, 'aaa', '2020'), (456, 'bbb', '2021')",
		line:  0,
		col:   col,
		want: lsp.SignatureHelp{
			Signatures: []lsp.SignatureInformation{
				{
					Label:         "city (ID, Name, CountryCode)",
					Documentation: "city table columns",
					Parameters: []lsp.ParameterInformation{
						{
							Label:         "ID",
							Documentation: "`int(11)` PRI auto_increment",
						},
						{
							Label:         "Name",
							Documentation: "`char(35)`",
						},
						{
							Label:         "CountryCode",
							Documentation: "`char(3)` MUL",
						},
					},
				},
			},
			ActiveSignature: 0.0,
			ActiveParameter: float64(wantActiveParameter),
		},
	}
}

func TestSignatureHelpMain(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range signatureHelpTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.SignatureHelpParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: testFileURI,
					},
					Position: lsp.Position{
						Line:      tt.line,
						Character: tt.col,
					},
				},
			}
			var got lsp.SignatureHelp
			if err := tx.conn.Call(tx.ctx, "textDocument/signatureHelp", params, &got); err != nil {
				t.Fatal("conn.Call textDocument/signatureHelp:", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unmatch (- want, + got):\n%s", diff)
			}
		})
	}
}

func TestSignatureHelpNoneDBConnection(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{},
	}
	tx.addWorkspaceConfig(t, cfg)

	uri := "file:///Users/octref/Code/css-test/test.sql"
	for _, tt := range signatureHelpTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.SignatureHelpParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: uri,
					},
					Position: lsp.Position{
						Line:      tt.line,
						Character: tt.col,
					},
				},
			}
			// Without a DB connection, it is not possible to provide functions using the DB connection, so just make sure that no errors occur.
			var got lsp.SignatureHelp
			if err := tx.conn.Call(tx.ctx, "textDocument/signatureHelp", params, &got); err != nil {
				t.Fatal("conn.Call textDocument/signatureHelp:", err)
			}
		})
	}
}

func interBaseSignatureCache(t *testing.T) *database.DBCache {
	t.Helper()
	return &database.DBCache{
		Catalog: &database.CatalogCache{
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
			},
		},
	}
}

func wantProcedureSignature(activeParameter int) lsp.SignatureHelp {
	return lsp.SignatureHelp{
		Signatures: []lsp.SignatureInformation{
			{
				Label:         "MYPROC (IN_CODE, IN_AMOUNT)",
				Documentation: "MYPROC procedure — 2 input parameters, 1 output parameter",
				Parameters: []lsp.ParameterInformation{
					{
						Label:         "IN_CODE",
						Documentation: "`VARCHAR(3)` input NOT NULL",
					},
					{
						// Nullable is invalid, which is the normal case. No
						// nullability word appears at all.
						Label:         "IN_AMOUNT",
						Documentation: "`NUMERIC(18, 2)` input",
					},
				},
			},
		},
		ActiveSignature: 0.0,
		ActiveParameter: float64(activeParameter),
	}
}

func TestInterBaseSignatureHelpForExecuteProcedure(t *testing.T) {
	const input = "execute procedure myproc(123, 45)"
	cases := []struct {
		col                 int
		wantActiveParameter int
	}{
		{col: 25, wantActiveParameter: 0},
		{col: 28, wantActiveParameter: 0},
		{col: 29, wantActiveParameter: 1},
		{col: 31, wantActiveParameter: 1},
	}

	for _, tt := range cases {
		t.Run(fmt.Sprintf("col %d", tt.col), func(t *testing.T) {
			got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: tt.col},
				},
			}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
			if err != nil {
				t.Fatal("SignatureHelpWithDriver:", err)
			}
			if got == nil {
				t.Fatal("no signature help for a known procedure call")
			}
			if diff := cmp.Diff(wantProcedureSignature(tt.wantActiveParameter), *got); diff != "" {
				t.Errorf("signature help mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInterBaseSignatureHelpForSelectableProcedureCall(t *testing.T) {
	// The same signature, reached without EXECUTE PROCEDURE at all. This is
	// why the trigger is "the callee is a known procedure".
	const input = "select * from myproc("
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got == nil {
		t.Fatal("no signature help for a selectable procedure call")
	}
	if diff := cmp.Diff(wantProcedureSignature(0), *got); diff != "" {
		t.Errorf("signature help mismatch (-want +got):\n%s", diff)
	}
}

func TestInterBaseSignatureHelpOmitsUnknownNullability(t *testing.T) {
	const input = "execute procedure myproc("
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got == nil {
		t.Fatal("no signature help")
	}

	params := got.Signatures[0].Parameters
	if len(params) != 2 {
		t.Fatalf("got %d parameters, want 2", len(params))
	}
	if !strings.Contains(params[0].Documentation, "NOT NULL") {
		t.Errorf("a proven-non-nullable parameter lost its NOT NULL: %q", params[0].Documentation)
	}
	// The assertion that fails against a renderer that maps unknown to a word.
	for _, forbidden := range []string{"NOT NULL", "nullable", "unknown", "NULL"} {
		if strings.Contains(params[1].Documentation, forbidden) {
			t.Errorf("unknown nullability rendered %q in %q", forbidden, params[1].Documentation)
		}
	}
}

func TestSignatureHelpUnknownCalleeReturnsNil(t *testing.T) {
	for _, input := range []string{
		"select upper(",
		"execute procedure nosuchproc(",
		"select * from city",
	} {
		got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
			TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
				Position:     lsp.Position{Line: 0, Character: len(input)},
			},
		}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
		if err != nil {
			t.Fatalf("SignatureHelpWithDriver(%q): %v", input, err)
		}
		if got != nil {
			t.Errorf("SignatureHelpWithDriver(%q) = %+v, want nil", input, got)
		}
	}
}

func TestSignatureHelpWithoutCatalogReturnsNil(t *testing.T) {
	// The window before the metadata catalog categories land, and every other
	// driver: today's behaviour, unchanged.
	const input = "execute procedure myproc("
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, &database.DBCache{}, dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got != nil {
		t.Errorf("SignatureHelpWithDriver = %+v, want nil with no catalog", got)
	}
}

// --- The tests below this line go beyond the brief. They pin exact,
// verified boundary and cross-driver behaviour the brief's own test set
// does not cover: immediately after the closing parenthesis, a nested call,
// realistic incremental typing after a trailing comma, and the fact that the
// new branch gates on catalog presence alone, not on driver identity.

func TestInterBaseSignatureHelpAfterClosingParenResetsToFirstParameter(t *testing.T) {
	// parseutil.EnclosingCall (Task 4) treats a node's End() boundary
	// inclusively (ast.IsEnclose), so the character position immediately
	// after the closing ")" still reports Inside=true for the call. At that
	// position CallInfo's matched ast.IdentifierList no longer encloses the
	// cursor, so CallInfo.ActiveParameter falls back to its documented
	// "empty argument list" case: 0. The result is signature help that
	// lingers one character past the call and resets to the first
	// parameter, rather than disappearing or staying on the last parameter.
	// This is inherited from EnclosingCall/ActiveParameter, unmodified by
	// this task; it is pinned here rather than assumed.
	const input = "execute procedure myproc(123, 45)"
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got == nil {
		t.Fatal("no signature help immediately after the closing paren")
	}
	if diff := cmp.Diff(wantProcedureSignature(0), *got); diff != "" {
		t.Errorf("signature help mismatch (-want +got):\n%s", diff)
	}
}

func TestInterBaseSignatureHelpAfterTrailingComma(t *testing.T) {
	// The realistic incremental-typing case: the user has just typed a
	// comma and a space and nothing else, with no closing paren anywhere in
	// the buffer yet.
	const input = "execute procedure myproc(123, "
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got == nil {
		t.Fatal("no signature help right after a trailing comma")
	}
	if diff := cmp.Diff(wantProcedureSignature(1), *got); diff != "" {
		t.Errorf("signature help mismatch (-want +got):\n%s", diff)
	}
}

func TestInterBaseSignatureHelpInsideNestedCall(t *testing.T) {
	// A pre-existing parser limitation, not introduced here: parseFunctions
	// (parser/parser.go) is consumed by the same prefix-group pass that
	// builds the outer call's own FunctionLiteral, so it never gets a
	// chance to re-match "OTHERCALL(" against the Parenthesis that pass
	// already built for the outer call — "OTHERCALL(1, 2)" used as an
	// argument to MYPROC never becomes its own ast.FunctionLiteral. As a
	// result EnclosingCall's bottom-matched FunctionLiteral is still MYPROC
	// at every position here, but its independently bottom-matched
	// ast.IdentifierList is OTHERCALL's own "1, 2" once the cursor is
	// inside OTHERCALL's parens: the two matches come from different,
	// unrelated nodes. The user sees MYPROC's signature with an active
	// parameter that tracks position inside OTHERCALL's own argument list,
	// not MYPROC's — actively misleading, and not fixable from this task's
	// file (internal/handler/signature_help.go): the fix, if any, belongs
	// in parseutil.EnclosingCall (Task 4's file), which this task must
	// consume rather than rewrite. Pinned here so the exact behaviour is
	// visible rather than silently trusted.
	const input = "execute procedure myproc(othercall(1, 2), 3)"
	innerOpen := strings.Index(input, "othercall(") + len("othercall(")
	innerSecondArg := strings.Index(input, "1, 2") + len("1, ")
	afterInnerCallCloses := strings.Index(input, "), 3") + len("), ")
	cases := []struct {
		name                string
		col                 int
		wantActiveParameter int
	}{
		{"cursor on othercall's first argument", innerOpen, 0},
		{"cursor on othercall's second argument", innerSecondArg, 1},
		{"cursor on myproc's own next argument, after othercall(...) closes", afterInnerCallCloses, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: tt.col},
				},
			}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
			if err != nil {
				t.Fatal("SignatureHelpWithDriver:", err)
			}
			if got == nil {
				t.Fatal("no signature help inside a nested call")
			}
			// Still MYPROC's signature in every case: EnclosingCall never
			// finds OTHERCALL as a FunctionLiteral at all, per the
			// limitation above.
			if diff := cmp.Diff(wantProcedureSignature(tt.wantActiveParameter), *got); diff != "" {
				t.Errorf("signature help mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSignatureHelpFiresForAnyDriverWhenCatalogIsPresent(t *testing.T) {
	// procedureCallSignature gates on dbCache.HasCatalog() alone, the same
	// catalog-presence-only pattern Task 5 used for generateTableCandidates
	// (internal/completer/candidates.go), documented there as safe in
	// production only because DBCache.Catalog is populated exclusively by
	// an InterBase repository's CatalogRepository implementation, never for
	// any other driver's repo. This test proves that guarantee, not a
	// driver check, is what actually gates signature help too: under a
	// non-InterBase driver, with the one DBCache state no real
	// non-InterBase repository can ever produce (a populated .Catalog), the
	// procedure branch still fires. This documents the real gate; it is not
	// a bug for this task to fix.
	const input = "execute procedure myproc(123, 45)"
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 25},
		},
	}, interBaseSignatureCache(t), dialect.DatabaseDriverMySQL)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got == nil {
		t.Fatal("expected signature help: the gate is catalog presence, not driver identity")
	}
	if diff := cmp.Diff(wantProcedureSignature(0), *got); diff != "" {
		t.Errorf("signature help mismatch (-want +got):\n%s", diff)
	}
}
