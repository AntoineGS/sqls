package sqlsymbol

import "testing"

// Mutation caught: disabling the default-off policy is ignored or query
// advisories are emitted by the default Diagnostics path.
func TestDiagnosticQueryLogicAdvisoriesDefaultOff(t *testing.T) {
	a, err := AnalyzeDiagnostics("SELECT T.ID FROM T WHERE T.ID NOT IN (SELECT U.V FROM U);", interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range a.Diagnostics(newDiagnosticFixtureCatalog()) {
		if finding.Code == "interbase-nullable-not-in" {
			t.Fatalf("default diagnostics unexpectedly emitted nullable NOT IN: %+v", finding)
		}
	}
}

// Mutation caught: the analyzer stops distinguishing a nullable NOT IN
// projection from an explicitly non-null projection in its own subquery.
func TestDiagnosticQueryLogicNullableNotInProjection(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"nullable projection", "SELECT T.ID FROM T WHERE T.ID NOT IN (SELECT U.V FROM U);", 1},
		{"filtered projection", "SELECT T.ID FROM T WHERE T.ID NOT IN (SELECT U.V FROM U WHERE U.V IS NOT NULL);", 0},
		{"nested where does not refine projection", "SELECT T.ID FROM T WHERE T.ID NOT IN (SELECT U.V FROM U WHERE EXISTS (SELECT 1 FROM U X WHERE U.V IS NOT NULL));", 1},
		{"shadowed alias does not refine projection", "SELECT T.ID FROM T WHERE T.ID NOT IN (SELECT U.V FROM U WHERE EXISTS (SELECT 1 FROM U U WHERE U.V IS NOT NULL));", 1},
		{"correlated subquery", "SELECT T.ID FROM T WHERE T.ID NOT IN (SELECT U.V FROM U WHERE U.ID=T.ID);", 0},
		{"unknown metadata", "SELECT T.ID FROM T WHERE T.ID NOT IN (SELECT U.V FROM U);", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog := newDiagnosticFixtureCatalog()
			if tc.name == "unknown metadata" {
				catalog.relationsKnown = false
				catalog.relations = nil
			}
			a, err := AnalyzeDiagnostics(tc.sql, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			options := DiagnosticOptions{Rules: map[string]string{"interbase-nullable-not-in": "warning"}}
			got := findingsWithCode(a.DiagnosticsWithOptions(catalog, options), "interbase-nullable-not-in")
			if len(got) != tc.want {
				t.Fatalf("got %d findings, want %d: %+v", len(got), tc.want, got)
			}
			if tc.want == 1 && got[0].Span != (Span{Start: 45, End: 48}) {
				t.Fatalf("finding span = %+v, want projected U.V span [45,48)", got[0].Span)
			}
		})
	}
}

// Mutation caught: relation names are used instead of binding the WHERE
// reference to the right-side source occurrence; that would miss U.ID's
// structural NULL extension or warn on an IS NULL anti-join.
func TestDiagnosticQueryLogicOuterJoinNullRejection(t *testing.T) {
	cases := []struct {
		name, sql string
		want      int
	}{
		{"null-rejecting comparison", "SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE U.V > 0;", 1},
		{"outer-joined not-null key", "SELECT T.ID FROM T LEFT OUTER JOIN U ON U.ID=T.ID WHERE U.ID > 0;", 1},
		{"negated is-not-null preserves null rows", "SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE NOT (U.V IS NOT NULL);", 0},
		{"nested query predicate is not outer filter", "SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE EXISTS (SELECT 1 FROM U X WHERE U.V IS NOT NULL);", 0},
		{"anti join", "SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE U.ID IS NULL;", 0},
		{"null-preserving OR", "SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE U.V > 0 OR T.ID IS NULL;", 0},
		{"coalesce", "SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE COALESCE(U.V, 0) > 0;", 0},
		{"multiple joins", "SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID LEFT JOIN T X ON X.ID=T.ID WHERE U.V > 0;", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := AnalyzeDiagnostics(tc.sql, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			options := DiagnosticOptions{Rules: map[string]string{"interbase-outer-join-filter": "warning"}}
			got := findingsWithCode(a.DiagnosticsWithOptions(newDiagnosticFixtureCatalog(), options), "interbase-outer-join-filter")
			if len(got) != tc.want {
				t.Fatalf("got %d findings, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

// Mutation caught: an explicit severity override fails to activate a rule
// whose default policy is off, or findings ignore the configured severity.
func TestDiagnosticQueryLogicPolicySeverityOverride(t *testing.T) {
	a, err := AnalyzeDiagnostics("SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE U.V > 0;", interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	got := findingsWithCode(a.DiagnosticsWithOptions(newDiagnosticFixtureCatalog(), DiagnosticOptions{Rules: map[string]string{"interbase-outer-join-filter": "information"}}), "interbase-outer-join-filter")
	if len(got) != 1 || got[0].Severity != 3 {
		t.Fatalf("got %+v, want one information finding", got)
	}
}

// Mutation caught: query-logic analysis only runs on standalone statements
// and misses the same recognized embedded SQL shape inside a procedure.
func TestDiagnosticQueryLogicEmbeddedSQL(t *testing.T) {
	sql := "CREATE PROCEDURE P AS BEGIN FOR SELECT T.ID FROM T LEFT JOIN U ON U.ID=T.ID WHERE U.V > 0 INTO :X DO BEGIN END END"
	a, err := AnalyzeDiagnostics(sql, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	options := DiagnosticOptions{Rules: map[string]string{"interbase-outer-join-filter": "warning"}}
	got := findingsWithCode(a.DiagnosticsWithOptions(newDiagnosticFixtureCatalog(), options), "interbase-outer-join-filter")
	if len(got) != 1 {
		t.Fatalf("got %d embedded findings, want 1: %+v", len(got), got)
	}
}

// Mutation caught: known NOT NULL destinations are not checked against
// possibly nullable INSERT...SELECT sources, or local IS NOT NULL proof is
// incorrectly shared/mutated across expression facts.
func TestDiagnosticQueryLogicNullableAssignment(t *testing.T) {
	cases := []struct {
		name, sql string
		want      int
	}{
		{"nullable source", "INSERT INTO T (ID) SELECT V FROM U;", 1},
		{"locally refined source", "INSERT INTO T (ID) SELECT V FROM U WHERE V IS NOT NULL;", 0},
		{"nested where does not refine", "INSERT INTO T (ID) SELECT U.V FROM U WHERE EXISTS (SELECT 1 FROM U X WHERE U.V IS NOT NULL);", 1},
		{"shadowed alias does not refine", "INSERT INTO T (ID) SELECT U.V FROM U WHERE EXISTS (SELECT 1 FROM U U WHERE U.V IS NOT NULL);", 1},
		{"OR does not refine", "INSERT INTO T (ID) SELECT V FROM U WHERE V IS NOT NULL OR ID=0;", 1},
		{"unrelated query does not refine", "SELECT V FROM U WHERE V IS NOT NULL; INSERT INTO T (ID) SELECT V FROM U;", 1},
		{"different bound column does not refine", "INSERT INTO T (ID) SELECT U.V FROM U JOIN T X ON X.ID=U.ID WHERE X.V IS NOT NULL;", 1},
		{"coalesce removes null", "INSERT INTO T (ID) SELECT COALESCE(V, 0) FROM U;", 0},
		{"case may preserve null", "INSERT INTO T (ID) SELECT CASE WHEN ID > 0 THEN V ELSE 0 END FROM U;", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := AnalyzeDiagnostics(tc.sql, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			options := DiagnosticOptions{Rules: map[string]string{"interbase-nullable-assignment": "warning"}}
			got := findingsWithCode(a.DiagnosticsWithOptions(newDiagnosticFixtureCatalog(), options), "interbase-nullable-assignment")
			if len(got) != tc.want {
				t.Fatalf("got %d findings, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

func findingsWithCode(findings []Finding, code string) []Finding {
	var out []Finding
	for _, finding := range findings {
		if finding.Code == code {
			out = append(out, finding)
		}
	}
	return out
}
