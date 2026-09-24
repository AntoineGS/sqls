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
