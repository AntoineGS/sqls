package dialect

import (
	"reflect"
	"testing"
)

func TestInterBaseSQLVariantRoundTrip(t *testing.T) {
	tests := []struct {
		sqlDialect  int
		wantVariant SQLVariant
		wantBack    int
	}{
		{sqlDialect: 0, wantVariant: SQLVariantInterBase3, wantBack: 3},
		{sqlDialect: 1, wantVariant: SQLVariantInterBase1, wantBack: 1},
		{sqlDialect: 3, wantVariant: SQLVariantInterBase3, wantBack: 3},
	}

	for _, tt := range tests {
		got := InterBaseSQLVariant(tt.sqlDialect)
		if got != tt.wantVariant {
			t.Errorf("InterBaseSQLVariant(%d) = %q, want %q", tt.sqlDialect, got, tt.wantVariant)
		}
		if back := got.InterBaseSQLDialect(); back != tt.wantBack {
			t.Errorf("InterBaseSQLVariant(%d).InterBaseSQLDialect() = %d, want %d", tt.sqlDialect, back, tt.wantBack)
		}
	}

	if got, want := SQLVariantDefault.InterBaseSQLDialect(), 3; got != want {
		t.Errorf("SQLVariantDefault.InterBaseSQLDialect() = %d, want %d", got, want)
	}
	if got, want := SQLVariant("nonsense").InterBaseSQLDialect(), 3; got != want {
		t.Errorf("unknown variant InterBaseSQLDialect() = %d, want %d", got, want)
	}
}

func TestDialectForDriverVariant(t *testing.T) {
	tests := []struct {
		name           string
		dv             DriverVariant
		wantSQLDialect int
		wantGeneric    bool
	}{
		{
			name:           "interbase default variant is dialect 3",
			dv:             DriverVariant{Driver: DatabaseDriverInterBase},
			wantSQLDialect: 3,
		},
		{
			name:           "interbase dialect 1 variant",
			dv:             DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase1},
			wantSQLDialect: 1,
		},
		{
			name:           "interbase dialect 3 variant",
			dv:             DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase3},
			wantSQLDialect: 3,
		},
		{
			name:        "unknown driver stays generic",
			dv:          DriverVariant{Driver: DatabaseDriver("mock"), Variant: SQLVariantInterBase1},
			wantGeneric: true,
		},
		{
			name:        "postgresql stays generic",
			dv:          DriverVariant{Driver: DatabaseDriverPostgreSQL},
			wantGeneric: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DialectForDriverVariant(tt.dv)
			if tt.wantGeneric {
				if _, ok := got.(*GenericSQLDialect); !ok {
					t.Fatalf("DialectForDriverVariant(%#v) = %T, want *GenericSQLDialect", tt.dv, got)
				}
				return
			}
			ib, ok := got.(*InterBaseDialect)
			if !ok {
				t.Fatalf("DialectForDriverVariant(%#v) = %T, want *InterBaseDialect", tt.dv, got)
			}
			if ib.SQLDialect != tt.wantSQLDialect {
				t.Fatalf("resolved SQLDialect = %d, want %d", ib.SQLDialect, tt.wantSQLDialect)
			}
		})
	}
}

func TestDialectForDriverDelegatesToDefaultVariant(t *testing.T) {
	ib, ok := DialectForDriver(DatabaseDriverInterBase).(*InterBaseDialect)
	if !ok {
		t.Fatalf("DialectForDriver(interbase) = %T, want *InterBaseDialect", DialectForDriver(DatabaseDriverInterBase))
	}
	if got, want := ib.SQLDialect, 3; got != want {
		t.Errorf("DialectForDriver(interbase).SQLDialect = %d, want %d", got, want)
	}
	if _, ok := DialectForDriver(DatabaseDriver("mock")).(*GenericSQLDialect); !ok {
		t.Error("unknown drivers should retain generic parsing")
	}
}

func TestInterBaseKeywordsByVariant(t *testing.T) {
	dialect1 := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase1})
	dialect3 := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase3})
	defaultVariant := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase})

	for _, word := range []string{"TIME", "TIMESTAMP"} {
		if containsString(dialect1, word) {
			t.Errorf("Dialect 1 keywords must not contain %q; that type does not exist in Dialect 1", word)
		}
		if !containsString(dialect3, word) {
			t.Errorf("Dialect 3 keywords must contain %q", word)
		}
	}
	for _, word := range []string{"SELECT", "GENERATOR", "ROWS", "DATE"} {
		if !containsString(dialect1, word) || !containsString(dialect3, word) {
			t.Errorf("both InterBase dialects must contain %q", word)
		}
	}
	for _, unsupported := range []string{"FIRST", "SKIP", "AUTONOMOUS", "WITH_LOCK"} {
		if containsString(dialect1, unsupported) || containsString(dialect3, unsupported) {
			t.Errorf("InterBase completion must not suggest Firebird syntax %q", unsupported)
		}
	}
	if !reflect.DeepEqual(defaultVariant, dialect3) {
		t.Error("the default InterBase variant must use the Dialect 3 keyword list")
	}
	if len(dialect3) != len(dialect1)+2 {
		t.Errorf("Dialect 3 keywords = %d words, want %d (Dialect 1 plus TIME and TIMESTAMP)", len(dialect3), len(dialect1)+2)
	}

	// Function lists are identical for both variants.
	fn1 := DataBaseFunctionsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase1})
	fn3 := DataBaseFunctionsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase3})
	if !reflect.DeepEqual(fn1, fn3) {
		t.Error("InterBase function lists must be identical for both dialects")
	}
}

func TestVariantLookupsAreUnchangedForOtherDrivers(t *testing.T) {
	drivers := []DatabaseDriver{
		DatabaseDriverMySQL, DatabaseDriverMySQL8, DatabaseDriverMySQL57, DatabaseDriverMySQL56,
		DatabaseDriverPostgreSQL, DatabaseDriverSQLite3, DatabaseDriverMssql, DatabaseDriverOracle,
		DatabaseDriverH2, DatabaseDriverVertica, DatabaseDriverClickhouse, DatabaseDriver("mock"),
	}
	for _, driver := range drivers {
		dv := DriverVariant{Driver: driver, Variant: SQLVariantInterBase1}
		if got, want := DataBaseKeywordsForVariant(dv), DataBaseKeywords(driver); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a variant must not change the keyword list", driver)
		}
		if got, want := DataBaseFunctionsForVariant(dv), DataBaseFunctions(driver); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a variant must not change the function list", driver)
		}
	}
}
