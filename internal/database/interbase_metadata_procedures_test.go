package database

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestInterBaseMetadataProceduresMatchLegacyAndUseConstantQueries(t *testing.T) {
	for _, dialect := range []int{1, 3} {
		t.Run(fmt.Sprintf("dialect-%d", dialect), func(t *testing.T) {
			db := openInterBaseSchemaFixture(t)
			insertProcedureParityFixtures(t, db)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: dialect}
			want, err := repo.DescribeProcedures(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			count := interBaseFixtureCountPrepares(t)
			patch, err := repo.runMetadataRead(context.Background(), repo.readMetadataProcedures)
			if err != nil {
				t.Fatal(err)
			}
			got := procedureMapValues(patch.Cache.Catalog.Procedures)
			sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
			sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("procedure descriptors differ\n got: %#v\nwant: %#v", got, want)
			}
			wantType := "TIMESTAMP"
			if dialect == 1 {
				wantType = "DATE"
			}
			var dateParameter *ProcedureParameterDesc
			for _, procedure := range got {
				if procedure.Name != "EDGE_PROC" {
					continue
				}
				for _, parameter := range procedure.InputParameters {
					if parameter.Name == "DATE_VALUE" {
						dateParameter = parameter
					}
				}
			}
			if dateParameter == nil {
				t.Fatal("EDGE_PROC DATE_VALUE parameter is missing")
			}
			if dateParameter.Type != wantType {
				t.Fatalf("DATE_VALUE type = %q for Dialect %d, want %q", dateParameter.Type, dialect, wantType)
			}
			if patch.Count != len(want) {
				t.Fatalf("Count = %d, want %d", patch.Count, len(want))
			}
			if queries := count(); queries != 3 {
				t.Fatalf("queries = %d, want 2 data + 1 width", queries)
			}
		})
	}
}

func TestInterBaseMetadataProceduresRejectInvalidChildrenAtomically(t *testing.T) {
	for _, test := range []struct {
		name      string
		parameter string
		wantError string
	}{
		{name: "unknown direction", parameter: `"RDB$PARAMETER_TYPE"=2`, wantError: "invalid parameter type"},
		{name: "missing direction", parameter: `"RDB$PARAMETER_TYPE"=NULL`, wantError: "invalid parameter type"},
		{name: "missing position", parameter: `"RDB$PARAMETER_NUMBER"=NULL`, wantError: "invalid position"},
		{name: "negative position", parameter: `"RDB$PARAMETER_NUMBER"=-1`, wantError: "invalid position"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openInterBaseSchemaFixture(t)
			if _, err := db.Exec(`UPDATE "RDB$PROCEDURE_PARAMETERS" SET ` + test.parameter + ` WHERE "RDB$PARAMETER_NAME" LIKE 'EMAIL%'`); err != nil {
				t.Fatal(err)
			}
			patch, err := (&InterBaseDBRepository{}).readMetadataProcedures(context.Background(), db, 127)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("readMetadataProcedures error = %v, want %q", err, test.wantError)
			}
			if patch.Cache != nil || patch.Count != 0 {
				t.Fatalf("failed category published patch=%+v", patch)
			}
		})
	}
	t.Run("duplicate position", func(t *testing.T) {
		db := openInterBaseSchemaFixture(t)
		if _, err := db.Exec(`UPDATE "RDB$PROCEDURE_PARAMETERS" SET "RDB$PARAMETER_NUMBER"=0 WHERE "RDB$PARAMETER_NAME" LIKE 'CODE%'`); err != nil {
			t.Fatal(err)
		}
		patch, err := (&InterBaseDBRepository{}).readMetadataProcedures(context.Background(), db, 127)
		if err == nil || !strings.Contains(err.Error(), "duplicate input parameter position") {
			t.Fatalf("readMetadataProcedures error = %v, want duplicate position", err)
		}
		if patch.Cache != nil || patch.Count != 0 {
			t.Fatalf("failed category published patch=%+v", patch)
		}
	})
}

func TestInterBaseMetadataProceduresUseConstantQueries(t *testing.T) {
	for _, size := range []int{1, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			db := openInterBaseScalableFixture(t, 1)
			insertMetadataProcedureFixtures(t, db, size)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			count := interBaseFixtureCountPrepares(t)
			patch, err := repo.runMetadataRead(context.Background(), repo.readMetadataProcedures)
			if err != nil {
				t.Fatal(err)
			}
			if got := patch.Count; got != size {
				t.Fatalf("procedure count = %d, want %d", got, size)
			}
			if queries := count(); queries != 3 {
				t.Fatalf("queries = %d, want 2 data + 1 width", queries)
			}
		})
	}
}

func procedureMapValues(procedures map[string]*ProcedureDesc) []*ProcedureDesc {
	result := make([]*ProcedureDesc, 0, len(procedures))
	for _, procedure := range procedures {
		result = append(result, procedure)
	}
	return result
}

func insertMetadataProcedureFixtures(t *testing.T, db *sql.DB, size int) {
	t.Helper()
	for i := 1; i <= size; i++ {
		name := fmt.Sprintf("BULK_PROC_%03d", i)
		if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURES" ("RDB$PROCEDURE_NAME","RDB$PROCEDURE_ID","RDB$PROCEDURE_INPUTS","RDB$PROCEDURE_OUTPUTS","RDB$DESCRIPTION","RDB$PROCEDURE_SOURCE","RDB$OWNER_NAME","RDB$SYSTEM_FLAG") VALUES (?,?,?,?,?,?,?,?)`, name, 50000+i, 0, 0, " description ", "BEGIN END", "OWNER", 0); err != nil {
			t.Fatal(err)
		}
	}
}

func insertProcedureParityFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_LENGTH","RDB$FIELD_SCALE","RDB$FIELD_TYPE","RDB$FIELD_SUB_TYPE","RDB$SYSTEM_FLAG","RDB$NULL_FLAG") VALUES (?,?,?,?,?,?,?)`, "EXPLICIT_NULLABLE", 4, 0, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$SYSTEM_FLAG") VALUES (?,?,?)`, "PROC_DATE", 35, 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"NO_PARAMS", "EDGE_PROC"} {
		if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`, name, 60000, 3, 1, "  description verbatim  ", "BEGIN\n  SELECT 'source';\nEND", nil, interBaseFixed("OWNER"), 0); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(name string, position, direction int, source string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURE_PARAMETERS" VALUES (?,?,?,?,?,?,?)`, interBaseFixed(name), "EDGE_PROC", position, direction, source, " parameter description ", 0); err != nil {
			t.Fatal(err)
		}
	}
	// Insert out of position order; the catalog loader must restore the driver's order.
	insert("MISSING", 3, 0, "NO_SUCH_DOMAIN")
	insert("OUT", 0, 1, "LEGACY_FLAG")
	insert("INLINE", 2, 0, "RDB$1")
	insert("NULLABLE", 1, 0, "EXPLICIT_NULLABLE")
	insert("USER_DOMAIN", 0, 0, "EMAIL_ADDRESS")
	insert("DATE_VALUE", 4, 0, "PROC_DATE")
}
