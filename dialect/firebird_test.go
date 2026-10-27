package dialect

import (
	"slices"
	"testing"
)

func TestFirebirdVariantMapsDialect(t *testing.T) {
	if FirebirdSQLVariant(1) != "firebird25-dialect-1" || FirebirdSQLVariant(3) != "firebird25-dialect-3" {
		t.Fatal("Firebird variant constants do not use the specified values")
	}
	if FirebirdSQLVariant(1).InterBaseSQLDialect() != 1 || FirebirdSQLVariant(3).InterBaseSQLDialect() != 3 || FirebirdSQLVariant(0).InterBaseSQLDialect() != 3 {
		t.Fatal("Firebird variants do not map to SQL dialects 1/3")
	}
	if !FirebirdSQLVariant(3).IsFirebird() || InterBaseSQLVariant(3).IsFirebird() {
		t.Fatal("IsFirebird is wrong")
	}
}

func TestFirebirdKeywordsExtendInterBase(t *testing.T) {
	fb := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: FirebirdSQLVariant(3)})
	ib := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: InterBaseSQLVariant(3)})
	for _, word := range []string{
		"BLOCK", "GLOBAL", "TEMPORARY", "PRESERVE", "RECREATE", "RETURNING", "MERGE", "MATCHED", "MATCHING", "SEQUENCE", "NEXT", "RESTART", "ROWS", "RECURSIVE", "MAPPING", "SIMILAR", "LEADING", "TRAILING", "BOTH", "CROSS", "INSENSITIVE", "RELEASE", "SAVEPOINT", "AUTONOMOUS", "COMMON", "CONNECT", "DISCONNECT", "TRANSACTION", "LIST", "IIF",
	} {
		if !slices.Contains(fb, word) {
			t.Errorf("Firebird keywords lack %s", word)
		}
	}
	for _, word := range ib {
		if !slices.Contains(fb, word) {
			t.Errorf("Firebird keywords dropped InterBase %s", word)
		}
	}
	for i := 1; i < len(fb); i++ {
		if fb[i-1] >= fb[i] {
			t.Errorf("Firebird keywords are not sorted and unique at %q, %q", fb[i-1], fb[i])
		}
	}
	fb1 := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: FirebirdSQLVariant(1)})
	ib1 := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: InterBaseSQLVariant(1)})
	for _, word := range ib1 {
		if !slices.Contains(fb1, word) {
			t.Errorf("Firebird Dialect 1 keywords dropped InterBase %s", word)
		}
	}
	for _, word := range []string{"TIME", "TIMESTAMP"} {
		if slices.Contains(fb1, word) {
			t.Errorf("Firebird Dialect 1 keywords must not contain %s", word)
		}
	}

	fns := DataBaseFunctionsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: FirebirdSQLVariant(3)})
	for _, fn := range []string{
		"ABS", "ACOS", "ASCII_CHAR", "ASCII_VAL", "ASIN", "ATAN", "ATAN2", "BIN_AND", "BIN_NOT", "BIN_OR", "BIN_SHL", "BIN_SHR", "BIN_XOR", "BIT_LENGTH", "CEIL", "CEILING", "CHAR_LENGTH", "CHAR_TO_UUID", "CHARACTER_LENGTH", "COALESCE", "COS", "COSH", "COT", "DATEADD", "DATEDIFF", "DECODE", "EXP", "FLOOR", "GEN_ID", "GEN_UUID", "HASH", "IIF", "LEFT", "LIST", "LN", "LOG", "LOG10", "LOWER", "LPAD", "MAXVALUE", "MINVALUE", "MOD", "NULLIF", "OCTET_LENGTH", "OVERLAY", "PI", "POSITION", "POWER", "RAND", "RDB$GET_CONTEXT", "RDB$SET_CONTEXT", "REPLACE", "REVERSE", "RIGHT", "ROUND", "RPAD", "SIGN", "SIN", "SINH", "SQRT", "SUBSTRING", "TAN", "TANH", "TRIM", "TRUNC", "UPPER", "UUID_TO_CHAR",
	} {
		if !slices.Contains(fns, fn) {
			t.Errorf("Firebird functions lack %s", fn)
		}
	}
	for _, fn := range DataBaseFunctionsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: InterBaseSQLVariant(3)}) {
		if !slices.Contains(fns, fn) {
			t.Errorf("Firebird functions dropped InterBase %s", fn)
		}
	}
	for i := 1; i < len(fns); i++ {
		if fns[i-1] >= fns[i] {
			t.Errorf("Firebird functions are not sorted and unique at %q, %q", fns[i-1], fns[i])
		}
	}
}

func TestFirebirdReservedWordsRespectSQLDialect(t *testing.T) {
	for _, word := range []string{"TIME", "TIMESTAMP"} {
		if IsInterBaseReservedWord(word, FirebirdSQLVariant(1)) {
			t.Errorf("Firebird Dialect 1 must not reserve %s", word)
		}
		if !IsInterBaseReservedWord(word, FirebirdSQLVariant(3)) {
			t.Errorf("Firebird Dialect 3 must reserve %s", word)
		}
	}
}

func TestFirebirdVariantDoesNotChangeOtherDriverLists(t *testing.T) {
	for _, driver := range []DatabaseDriver{DatabaseDriverMySQL8, DatabaseDriverPostgreSQL, DatabaseDriverSQLite3} {
		dv := DriverVariant{Driver: driver, Variant: FirebirdSQLVariant(3)}
		if got, want := DataBaseKeywordsForVariant(dv), DataBaseKeywords(driver); !slices.Equal(got, want) {
			t.Errorf("%s: Firebird variant changed keywords", driver)
		}
		if got, want := DataBaseFunctionsForVariant(dv), DataBaseFunctions(driver); !slices.Equal(got, want) {
			t.Errorf("%s: Firebird variant changed functions", driver)
		}
	}
}
