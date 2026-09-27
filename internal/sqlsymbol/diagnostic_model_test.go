package sqlsymbol

import (
	"strings"
	"testing"
)

func buildModel(t *testing.T, text string, c Catalog) (*Analysis, *diagnosticModel) {
	t.Helper()
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	return a, a.diagnosticModel(c)
}

// itemAt finds the item index of the lexeme starting at the offset of
// marker's occurrenceth occurrence (0-based) in text.
func itemAt(t *testing.T, m *diagnosticModel, text, marker string, occurrence int) int {
	t.Helper()
	from := 0
	offset := -1
	for i := 0; i <= occurrence; i++ {
		relative := strings.Index(text[from:], marker)
		if relative < 0 {
			t.Fatalf("occurrence %d of %q not found", i, marker)
		}
		offset = from + relative
		from = offset + len(marker)
	}
	for i, item := range m.items {
		if item.Span.Start == offset {
			return i
		}
	}
	t.Fatalf("no lexeme at offset %d for marker %q", offset, marker)
	return -1
}

func relationNames(refs []RelationRef) []string {
	names := make([]string, len(refs))
	for i, ref := range refs {
		if ref.Name.Key() != "" {
			names[i] = ref.Name.Key()
		} else if ref.Alias != nil {
			names[i] = "~" + ref.Alias.Key()
		}
	}
	return names
}

func TestDiagnosticModelPlainSelectNoTerminator(t *testing.T) {
	text := `SELECT ID FROM T`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	statements := m.Statements()
	if len(statements) != 1 || statements[0].Malformed {
		t.Fatalf("Statements() = %+v, want one complete statement", statements)
	}
	i := itemAt(t, m, text, "ID", 0)
	scope := m.RelationScope(i)
	if len(scope) != 1 || len(scope[0]) != 1 || scope[0][0].Name.Key() != "T" {
		t.Fatalf("RelationScope(ID) = %+v, want [[T]]", scope)
	}
	out, ok := m.TargetOutput(i)
	if !ok || !out.CountKnown || len(out.Columns) != 1 || !out.Columns[0].NameKnown || out.Columns[0].Name.Key() != "ID" {
		t.Fatalf("TargetOutput(ID) = %+v, %v, want one known ID column", out, ok)
	}
}

func TestDiagnosticModelMalformedStatementDoesNotHideLaterOne(t *testing.T) {
	text := `SELECT ; SELECT ID FROM T;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	statements := m.Statements()
	if len(statements) != 2 {
		t.Fatalf("Statements() = %+v, want two statements", statements)
	}
	if statements[0].Malformed {
		t.Fatalf("Statements()[0] = %+v, want a structurally complete (if empty) statement", statements[0])
	}
	if statements[1].Malformed {
		t.Fatalf("Statements()[1] = %+v, want the later statement modeled cleanly", statements[1])
	}
	i := itemAt(t, m, text, "ID", 0)
	scope := m.RelationScope(i)
	if len(scope) != 1 || len(scope[0]) != 1 || scope[0][0].Name.Key() != "T" {
		t.Fatalf("RelationScope(ID) = %+v, want [[T]] despite the earlier malformed statement", scope)
	}
}

func TestDiagnosticModelUnclosedParenRecoversAtNextStatement(t *testing.T) {
	text := `SELECT ID FROM (T; SELECT ID FROM T;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	statements := m.Statements()
	if len(statements) != 2 {
		t.Fatalf("Statements() = %+v, want two statements", statements)
	}
	if !statements[0].Malformed {
		t.Fatalf("Statements()[0] = %+v, want it marked malformed", statements[0])
	}
	if statements[1].Malformed {
		t.Fatalf("Statements()[1] = %+v, want the later statement recovered cleanly", statements[1])
	}
	i := itemAt(t, m, text, "ID", 1)
	scope := m.RelationScope(i)
	if len(scope) != 1 || len(scope[0]) != 1 || scope[0][0].Name.Key() != "T" {
		t.Fatalf("RelationScope(second ID) = %+v, want [[T]]", scope)
	}
}

func TestDiagnosticModelCTEOutputAndScope(t *testing.T) {
	text := `WITH Q AS (SELECT ID FROM T) SELECT Q.ID FROM Q;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())

	innerID := itemAt(t, m, text, "ID", 0)
	innerScope := m.RelationScope(innerID)
	if len(innerScope) != 1 || len(innerScope[0]) != 1 || innerScope[0][0].Name.Key() != "T" {
		t.Fatalf("RelationScope(inner ID) = %+v, want [[T]]", innerScope)
	}

	outerID := itemAt(t, m, text, "ID", 1)
	outerScope := m.RelationScope(outerID)
	if len(outerScope) != 1 || len(outerScope[0]) != 1 || outerScope[0][0].Name.Key() != "Q" {
		t.Fatalf("RelationScope(outer Q.ID) = %+v, want [[Q]]", outerScope)
	}

	ref := outerScope[0][0]
	columns, ok := m.RelationOutput(outerID, ref)
	if !ok || len(columns) != 1 || columns[0].Name != "ID" {
		t.Fatalf("RelationOutput(Q) = %+v, %v, want [{ID}]", columns, ok)
	}
}

func TestDiagnosticModelDerivedTableOutput(t *testing.T) {
	text := `SELECT D.ID FROM (SELECT ID FROM T) D;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())

	// A derived table's body is a child query of the outer SELECT, the same
	// nesting shape as a correlated subquery: RelationScope's innermost
	// (first) level must resolve to T, though the chain also walks out to
	// the outer query's own relations (which contain only the D alias
	// placeholder, not real columns) for correlation, exactly as the EXISTS
	// fixture below expects.
	innerID := itemAt(t, m, text, "ID", 1)
	innerScope := m.RelationScope(innerID)
	if len(innerScope) == 0 || len(innerScope[0]) != 1 || innerScope[0][0].Name.Key() != "T" {
		t.Fatalf("RelationScope(inner ID) = %+v, want innermost level [T]", innerScope)
	}

	outerID := itemAt(t, m, text, "D.ID", 0) + 2 // D . ID
	outerScope := m.RelationScope(outerID)
	if len(outerScope) != 1 || len(outerScope[0]) != 1 {
		t.Fatalf("RelationScope(outer D.ID) = %+v, want a single derived relation", outerScope)
	}
	ref := outerScope[0][0]
	if ref.Name.Key() != "" || ref.Alias == nil || ref.Alias.Key() != "D" {
		t.Fatalf("outer relation = %+v, want anonymous derived table aliased D", ref)
	}
	columns, ok := m.RelationOutput(outerID, ref)
	if !ok || len(columns) != 1 || columns[0].Name != "ID" {
		t.Fatalf("RelationOutput(D) = %+v, %v, want [{ID}]", columns, ok)
	}
}

func TestDiagnosticModelExistsResolvesInnerBeforeOuter(t *testing.T) {
	text := `SELECT T.ID FROM T WHERE EXISTS (SELECT 1 FROM U WHERE U.ID = T.ID);`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())

	innerUID := itemAt(t, m, text, "U.ID", 0) + 2
	scope := m.RelationScope(innerUID)
	if len(scope) != 2 {
		t.Fatalf("RelationScope(U.ID) chain = %+v, want inner+outer scopes", scope)
	}
	if len(scope[0]) != 1 || scope[0][0].Name.Key() != "U" {
		t.Fatalf("RelationScope(U.ID)[0] = %+v, want innermost [U]", scope[0])
	}
	if len(scope[1]) != 1 || scope[1][0].Name.Key() != "T" {
		t.Fatalf("RelationScope(U.ID)[1] = %+v, want outer [T]", scope[1])
	}

	correlatedTID := itemAt(t, m, text, "T.ID", 1) + 2
	correlatedScope := m.RelationScope(correlatedTID)
	if len(correlatedScope) != 2 || len(correlatedScope[0]) != 1 || correlatedScope[0][0].Name.Key() != "U" {
		t.Fatalf("RelationScope(correlated T.ID) = %+v, want inner U reachable before outer T", correlatedScope)
	}
}

func TestDiagnosticModelUnionMergesOutputShape(t *testing.T) {
	text := `SELECT ID FROM T UNION ALL SELECT ID FROM U;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())

	i := itemAt(t, m, text, "ID", 0)
	out, ok := m.UnionOutput(i)
	if !ok || !out.CountKnown || len(out.Columns) != 1 || !out.Columns[0].NameKnown || out.Columns[0].Name.Key() != "ID" {
		t.Fatalf("UnionOutput() = %+v, %v, want a merged single ID column", out, ok)
	}

	j := itemAt(t, m, text, "ID", 1)
	out2, ok2 := m.UnionOutput(j)
	if !ok2 || len(out2.Columns) != len(out.Columns) {
		t.Fatalf("UnionOutput() from second arm = %+v, %v, want the same merged shape", out2, ok2)
	}
}

func TestDiagnosticModelRecursiveCTEIsUnsupported(t *testing.T) {
	text := `WITH Q AS (SELECT ID FROM T UNION SELECT ID FROM Q) SELECT ID FROM Q;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	i := itemAt(t, m, text, "WITH", 0)
	if !m.Unsupported(i) {
		t.Fatalf("Unsupported(WITH) = false, want true for a recursive/unioned CTE")
	}
}

func TestDiagnosticModelMergeIsUnsupported(t *testing.T) {
	text := `MERGE INTO T USING U ON T.ID = U.ID WHEN MATCHED THEN UPDATE SET V = U.V;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	i := itemAt(t, m, text, "MERGE", 0)
	if !m.Unsupported(i) {
		t.Fatalf("Unsupported(MERGE) = false, want true")
	}
}

func TestDiagnosticModelNonRecursiveWithIsNotUnsupported(t *testing.T) {
	text := `WITH Q AS (SELECT ID FROM T) SELECT Q.ID FROM Q;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	i := itemAt(t, m, text, "SELECT", 0)
	if m.Unsupported(i) {
		t.Fatalf("Unsupported(WITH statement) = true, want false for a recognized non-recursive CTE")
	}
}

func TestDiagnosticModelDDLInvalidatesLaterUse(t *testing.T) {
	text := `CREATE TABLE T (ID INTEGER); SELECT ID FROM T;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	i := itemAt(t, m, text, "SELECT", 0)
	if !m.DDLInvalidated(i, Name{Text: "T"}) {
		t.Fatalf("DDLInvalidated(T) = false, want true after an earlier CREATE TABLE T")
	}
	if m.DDLInvalidated(i, Name{Text: "U"}) {
		t.Fatalf("DDLInvalidated(U) = true, want false: U was never touched by DDL")
	}
	create := itemAt(t, m, text, "CREATE", 0)
	if m.DDLInvalidated(create, Name{Text: "T"}) {
		t.Fatalf("DDLInvalidated(T) at the CREATE statement itself = true, want false before the DDL position")
	}
}

// --- Fix round 1: C1/C2 -- a WITH statement containing any nested SELECT
// (subquery or UNION arm) anywhere in a CTE body or the final SELECT is
// rejected wholesale rather than partially modeled.

func TestDiagnosticModelWithContainingSubqueryIsFullyUnsupported(t *testing.T) {
	text := `WITH Q AS (SELECT ID FROM T) SELECT Q.ID FROM Q WHERE EXISTS (SELECT 1 FROM U WHERE U.ID = Q.ID);`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	withPos := itemAt(t, m, text, "WITH", 0)
	if !m.Unsupported(withPos) {
		t.Fatalf("Unsupported(WITH) = false, want true: this WITH's final SELECT contains a nested subquery that isn't per-scope modeled")
	}
	innerUID := itemAt(t, m, text, "U.ID", 0) + 2
	if !m.Unsupported(innerUID) {
		t.Fatalf("Unsupported(U.ID) = false, want true: a subquery nested inside an unrecognized WITH must not silently expose a partial scope")
	}
	if scope := m.RelationScope(innerUID); scope != nil {
		t.Fatalf("RelationScope(U.ID) = %+v, want nil: an unrecognized WITH statement's content is not modeled at all, not partially modeled as [[Q]]", scope)
	}
}

func TestDiagnosticModelWithFinalUnionIsFullyUnsupported(t *testing.T) {
	text := `WITH Q AS (SELECT ID FROM T) SELECT ID FROM Q UNION SELECT V FROM U;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	withPos := itemAt(t, m, text, "WITH", 0)
	if !m.Unsupported(withPos) {
		t.Fatalf("Unsupported(WITH) = false, want true: a top-level UNION in the WITH statement's final SELECT is not modeled per-arm")
	}
}

// --- Fix round 1: C3 -- an explicit CTE column list renames the body's own
// output positionally; a count mismatch makes the CTE's output unknown.

func TestDiagnosticModelCTEExplicitColumnListRenamesOutput(t *testing.T) {
	text := `WITH Q (A) AS (SELECT ID FROM T) SELECT Q.A FROM Q;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	outerA := itemAt(t, m, text, "Q.A", 0) + 2
	scope := m.RelationScope(outerA)
	if len(scope) == 0 || len(scope[0]) != 1 {
		t.Fatalf("RelationScope(Q.A) = %+v, want a single Q relation", scope)
	}
	ref := scope[0][0]
	columns, ok := m.RelationOutput(outerA, ref)
	if !ok || len(columns) != 1 || columns[0].Name != "A" {
		t.Fatalf("RelationOutput(Q) = %+v, %v, want [{A}]: the explicit column list renames the body's own ID", columns, ok)
	}
}

func TestDiagnosticModelCTEExplicitColumnListCountMismatchIsUnknown(t *testing.T) {
	text := `WITH Q (A, B) AS (SELECT ID FROM T) SELECT ID FROM Q;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	i := itemAt(t, m, text, "FROM Q", 0) + 1
	scope := m.RelationScope(i)
	if len(scope) == 0 || len(scope[0]) != 1 {
		t.Fatalf("RelationScope(Q) = %+v, want a single Q relation", scope)
	}
	ref := scope[0][0]
	if _, ok := m.RelationOutput(i, ref); ok {
		t.Fatalf("RelationOutput(Q) ok = true, want false: the explicit column list count (2) does not match the body's own output count (1)")
	}
}

// --- Fix round 1: C4/I5 -- SET TERM custom delimiters split statements and
// are visible to DDL detection, for every DDL statement in the block.

func TestDiagnosticModelSetTermSplitsStatementsAndDDL(t *testing.T) {
	text := "SET TERM ^ ;\nCREATE TABLE A (ID INTEGER)^\nCREATE TABLE B (ID INTEGER)^\nSELECT ID FROM B^\nSET TERM ;^\n"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	statements := m.Statements()
	if len(statements) != 3 {
		t.Fatalf("Statements() = %+v, want 3 statements split across the SET TERM block", statements)
	}
	for i, stmt := range statements {
		if stmt.Malformed {
			t.Fatalf("Statements()[%d] = %+v, want cleanly modeled (not malformed)", i, stmt)
		}
	}
	selectPos := itemAt(t, m, text, "SELECT", 0)
	if !m.DDLInvalidated(selectPos, Name{Text: "A"}) {
		t.Fatalf("DDLInvalidated(A) = false, want true: A was CREATEd earlier in the same SET TERM block")
	}
	if !m.DDLInvalidated(selectPos, Name{Text: "B"}) {
		t.Fatalf("DDLInvalidated(B) = false, want true: B was CREATEd earlier in the same SET TERM block")
	}
	if m.DDLInvalidated(selectPos, Name{Text: "Z"}) {
		t.Fatalf("DDLInvalidated(Z) = true, want false: Z was never touched by DDL")
	}
	// The current fix only makes statement-boundary and DDL detection aware
	// of the SET TERM delimiter; the navigation binder's own token contexts
	// (buildContexts in resolve.go) still do not reset at a custom
	// delimiter, so this SELECT's contexts stay contextUnsupported and
	// RelationScope cannot resolve B here. Unsupported correctly reports
	// that instead of a false conclusion, which is the safe fallback: a
	// caller checking Unsupported before trusting RelationScope is not
	// misled.
	if !m.Unsupported(selectPos) {
		t.Fatalf("Unsupported(SELECT in SET TERM block) = false, want true: navigation's own context tracking does not reset at a SET TERM boundary")
	}
}

// --- Fix round 1: C5 -- UNION grouping only accepts a UNION token at the
// exact same nesting depth as the candidate sibling queries, so an unrelated
// UNION nested deeper inside one sibling cannot merge unrelated top-level
// siblings into a false UNION chain.

func TestDiagnosticModelUnionGroupingRespectsNestingDepth(t *testing.T) {
	text := `SELECT T.ID FROM T WHERE EXISTS (SELECT 1 FROM U WHERE U.ID IN (SELECT ID FROM T UNION SELECT ID FROM U)) AND EXISTS (SELECT 2 FROM U);`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	firstExists := itemAt(t, m, text, "SELECT 1", 0)
	if _, ok := m.UnionOutput(firstExists); ok {
		t.Fatalf("UnionOutput(first EXISTS) ok = true, want false: this EXISTS subquery merely contains an unrelated nested UNION at a deeper depth")
	}
	secondExists := itemAt(t, m, text, "SELECT 2", 0)
	if _, ok := m.UnionOutput(secondExists); ok {
		t.Fatalf("UnionOutput(second EXISTS) ok = true, want false: sibling EXISTS subqueries are not UNION arms of each other")
	}
}

// --- Fix round 1: I1 -- DDLInvalidated excludes positions within the DDL
// statement's own extent, matching its documented contract.

func TestDiagnosticModelDDLInvalidatedExcludesOwnStatementBody(t *testing.T) {
	text := `CREATE VIEW W AS SELECT ID FROM W;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	secondW := itemAt(t, m, text, "FROM W", 0) + 1
	if m.DDLInvalidated(secondW, Name{Text: "W"}) {
		t.Fatalf("DDLInvalidated(W) inside the CREATE VIEW's own body = true, want false: invalidation begins with the next statement, not partway through the DDL statement's own extent")
	}
}

// --- Fix round 1: I2/I3 -- star expansion resolves through the same
// relation-output path as RelationOutput, so it respects CTE shadowing and
// DDL invalidation instead of reading the catalog directly.

func TestDiagnosticModelStarExpansionRespectsCTEShadowing(t *testing.T) {
	text := `WITH T AS (SELECT ID FROM U) SELECT * FROM T;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	star := itemAt(t, m, text, "*", 0)
	out, ok := m.TargetOutput(star)
	if !ok || !out.CountKnown || len(out.Columns) != 1 || !out.Columns[0].NameKnown || out.Columns[0].Name.Key() != "ID" {
		t.Fatalf("TargetOutput(SELECT * FROM T) = %+v, %v, want the CTE T's single ID column, not the real table T's 4 columns", out, ok)
	}
}

func TestDiagnosticModelStarExpansionRespectsDDLInvalidation(t *testing.T) {
	text := `CREATE TABLE T (ID INTEGER); SELECT * FROM T;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	star := itemAt(t, m, text, "*", 0)
	out, ok := m.TargetOutput(star)
	if !ok {
		t.Fatalf("TargetOutput(SELECT * FROM T) ok = false, want true: the star token is inside a modeled query's own target list")
	}
	if out.CountKnown {
		t.Fatalf("TargetOutput(SELECT * FROM T).CountKnown = true, want false: T's catalog columns are stale after an earlier CREATE TABLE T in this same document")
	}
}

func TestDiagnosticModelRelationOutputRespectsDDLInvalidation(t *testing.T) {
	text := `CREATE TABLE T (ID INTEGER); SELECT ID FROM T;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	idPos := itemAt(t, m, text, "SELECT ID FROM T", 0) + 1
	scope := m.RelationScope(idPos)
	if len(scope) == 0 || len(scope[0]) != 1 {
		t.Fatalf("RelationScope(ID) = %+v, want a single T relation", scope)
	}
	ref := scope[0][0]
	if _, ok := m.RelationOutput(idPos, ref); ok {
		t.Fatalf("RelationOutput(T) ok = true, want false: T's catalog columns are stale after an earlier CREATE TABLE T in this same document")
	}
}

func TestDiagnosticModelKnownProcedureOutputInFrom(t *testing.T) {
	text := `SELECT X.OUT1 FROM P(1, 2) X;`
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	i := itemAt(t, m, text, "X.OUT1", 0) + 2
	scope := m.RelationScope(i)
	if len(scope) != 1 || len(scope[0]) != 1 {
		t.Fatalf("RelationScope(X.OUT1) = %+v, want a single callable relation", scope)
	}
	ref := scope[0][0]
	columns, ok := m.RelationOutput(i, ref)
	if !ok || len(columns) != 1 || columns[0].Name != "OUT1" {
		t.Fatalf("RelationOutput(P) = %+v, %v, want [{OUT1}]", columns, ok)
	}
}
