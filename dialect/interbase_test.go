package dialect

import "testing"

func TestInterBaseDialect1Syntax(t *testing.T) {
	d := &InterBaseDialect{}

	if !d.IsIdentifierPart('$') {
		t.Error("InterBase identifiers should allow '$' after the first character")
	}
	if d.IsIdentifierStart('$') {
		t.Error("'$' should not start an InterBase identifier")
	}
	if d.IsDelimitedIdentifierStart('"') {
		t.Error("InterBase Dialect 1 should not treat double quotes as delimited identifiers")
	}
	if !d.IsPlaceHolderStart('?') {
		t.Error("InterBase Dialect 1 should accept positional '?' placeholders")
	}
	if d.IsPlaceHolderStart('$') {
		t.Error("InterBase Dialect 1 should not use '$' as a placeholder")
	}
}

func TestDatabaseDriverInterBasePlumbing(t *testing.T) {
	if got := DialectForDriver(DatabaseDriverInterBase); got == nil {
		t.Fatal("DialectForDriver returned nil for InterBase")
	} else if _, ok := got.(*InterBaseDialect); !ok {
		t.Fatalf("DialectForDriver(InterBase) = %T, want *InterBaseDialect", got)
	}
	if _, ok := DialectForDriver(DatabaseDriver("mock")).(*GenericSQLDialect); !ok {
		t.Fatal("unknown drivers should retain generic parsing")
	}
}

func TestInterBaseKeywordsAndFunctions(t *testing.T) {
	for _, word := range []string{"CONTAINING", "GENERATOR"} {
		if got := (&InterBaseDialect{}).MatchKeyword(word); got != Matched {
			t.Errorf("InterBase keyword %q kind = %v, want Matched", word, got)
		}
	}

	keywords := DataBaseKeywords(DatabaseDriverInterBase)
	for _, want := range []string{"SELECT", "ROWS", "GENERATOR"} {
		if !containsString(keywords, want) {
			t.Errorf("InterBase keywords do not contain %q", want)
		}
	}

	functions := DataBaseFunctions(DatabaseDriverInterBase)
	for _, want := range []string{"GEN_ID", "COUNT", "SUM", "UPPER"} {
		if !containsString(functions, want) {
			t.Errorf("InterBase functions do not contain %q", want)
		}
	}
	for _, unsupported := range []string{"FIRST", "SKIP", "AUTONOMOUS", "WITH_LOCK"} {
		if containsString(keywords, unsupported) {
			t.Errorf("InterBase completion must not suggest Firebird syntax %q", unsupported)
		}
	}
	for _, unsupported := range []string{"IIF", "RDB$GET_CONTEXT", "RDB$SET_CONTEXT", "TRIM", "GEN_UUID"} {
		if containsString(functions, unsupported) {
			t.Errorf("InterBase completion must not suggest unsupported builtin %q", unsupported)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
