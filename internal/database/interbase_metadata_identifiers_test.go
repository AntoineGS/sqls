package database

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestInterBaseMetadataIdentifierUsesDiscoveredWidth(t *testing.T) {
	got := interBaseMetadataIdentifier("r.RDB$RELATION_NAME", 127)
	if got != "CAST(r.RDB$RELATION_NAME AS VARCHAR(127))" {
		t.Fatal(got)
	}
}

func TestInterBaseMetadataIdentifierWidthReadsCatalogCapacity(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	// Include widths that exceed the legacy cap, with a shared 67-byte prefix.
	if _, err := db.Exec(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_LENGTH","RDB$FIELD_TYPE") VALUES ('WIDE_A',127,14),('WIDE_B',127,37)`); err != nil {
		t.Fatal(err)
	}
	longPrefix := strings.Repeat("X", 67)
	if _, err := db.Exec(`INSERT INTO "RDB$RELATION_FIELDS" ("RDB$FIELD_NAME","RDB$RELATION_NAME","RDB$FIELD_SOURCE") VALUES (?, 'RDB$CATALOG','WIDE_A'), (?, 'RDB$CATALOG','WIDE_B')`, longPrefix+"A", longPrefix+"B"); err != nil {
		t.Fatal(err)
	}
	width, err := interBaseMetadataIdentifierWidth(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if width != 127 {
		t.Fatalf("width = %d, want 127", width)
	}
	query := `SELECT ` + interBaseMetadataIdentifier(`rf.RDB$FIELD_NAME`, width) + ` FROM RDB$RELATION_FIELDS rf WHERE rf.RDB$RELATION_NAME = 'RDB$CATALOG' ORDER BY rf.RDB$FIELD_NAME`
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] == got[1] || len(got[0]) <= 67 || len(got[1]) <= 67 {
		t.Fatalf("long identifiers lost identity: %#v", got)
	}
}

func TestInterBaseMetadataIdentifierWidthRejectsNullAndInvalid(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{{"null", nil}, {"zero", int64(0)}, {"negative", int64(-1)}}
	if strconv.IntSize == 32 {
		cases = append(cases, struct {
			name  string
			value any
		}{"overflow", int64(1 << 31)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &metadataTxState{width: tc.value}
			db := openMetadataTxDB(t, state)
			_, err := interBaseMetadataIdentifierWidth(context.Background(), db)
			if err == nil {
				t.Fatal("expected invalid identifier width error")
			}
		})
	}
}
