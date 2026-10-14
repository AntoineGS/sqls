package database

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

type watchedMappingRow struct {
	Family string
	Values map[string]any
}

// Offline capability discovery deliberately offers a superset. The production
// builder admits only manifest-labelled fields and builds real compact records.
func watchedMappingFingerprint(t *testing.T, rows ...watchedMappingRow) *schema.CatalogFingerprint {
	t.Helper()
	families := strings.Fields("RELATIONS RELATION_FIELDS VIEW_RELATIONS FIELDS FIELD_DIMENSIONS PROCEDURES PROCEDURE_PARAMETERS TRIGGERS TRIGGER_MESSAGES INDICES INDEX_SEGMENTS RELATION_CONSTRAINTS REF_CONSTRAINTS CHECK_CONSTRAINTS GENERATORS EXCEPTIONS FUNCTIONS FUNCTION_ARGUMENTS DEPENDENCIES CHARACTER_SETS COLLATIONS USER_PRIVILEGES ROLES SECURITY_CLASSES DATABASE")
	columns := strings.Fields("ACL ARGUMENT_POSITION BASE_FIELD BYTES_PER_CHARACTER CHARACTER_LENGTH CHARACTER_SET_ID CHARACTER_SET_NAME COLLATION_ATTRIBUTES COLLATION_ID COLLATION_NAME COMPUTED_BLR COMPUTED_SOURCE CONSTRAINT_NAME CONSTRAINT_TYPE CONST_NAME_UQ CONTEXT_NAME DBKEY_LENGTH DEFAULT_CLASS DEFAULT_COLLATE_NAME DEFAULT_SOURCE DEFAULT_VALUE DEFERRABLE DELETE_RULE DEPENDED_ON_NAME DEPENDED_ON_TYPE DEPENDENT_NAME DEPENDENT_TYPE DESCRIPTION DIMENSION DIMENSIONS ENTRYPOINT EXCEPTION_NAME EXCEPTION_NUMBER EXPRESSION_BLR EXPRESSION_SOURCE EXTERNAL_DESCRIPTION EXTERNAL_FILE EXTERNAL_LENGTH EXTERNAL_SCALE EXTERNAL_TYPE FIELD_ID FIELD_LENGTH FIELD_NAME FIELD_POSITION FIELD_PRECISION FIELD_SCALE FIELD_SOURCE FIELD_SUB_TYPE FIELD_TYPE FLAGS FOREIGN_KEY FORMAT FUNCTION_NAME FUNCTION_TYPE GENERATOR_ID GENERATOR_NAME GRANTOR GRANT_OPTION INDEX_ID INDEX_INACTIVE INDEX_NAME INDEX_TYPE INITIALLY_DEFERRED LOWER_BOUND MATCH_OPTION MECHANISM MESSAGE MESSAGE_NUMBER MODULE_NAME NULL_FLAG NUMBER_OF_CHARACTERS OBJECT_TYPE OWNER_NAME PARAMETER_NAME PARAMETER_NUMBER PARAMETER_TYPE PRIVILEGE PROCEDURE_BLR PROCEDURE_ID PROCEDURE_INPUTS PROCEDURE_NAME PROCEDURE_OUTPUTS PROCEDURE_SOURCE RELATION_ID RELATION_NAME RELATION_TYPE RETURN_ARGUMENT ROLE_NAME SECURITY_CLASS SEGMENT_COUNT SEGMENT_LENGTH SYSTEM_FLAG TRIGGER_BLR TRIGGER_INACTIVE TRIGGER_NAME TRIGGER_SEQUENCE TRIGGER_SOURCE TRIGGER_TYPE UNIQUE_FLAG UPDATE_FLAG UPDATE_RULE UPPER_BOUND USER USER_TYPE VALIDATION_BLR VALIDATION_SOURCE VIEW_BLR VIEW_CONTEXT VIEW_NAME VIEW_SOURCE")
	f, err := schema.ReadCatalogFingerprint(context.Background(), nil, 64<<20, func(ctx context.Context, conn *sql.Conn, qs []schema.CatalogFingerprintQuery, visit schema.CatalogFingerprintVisitor) (schema.CatalogFingerprintStats, error) {
		var stats schema.CatalogFingerprintStats
		for i, q := range qs {
			stats.Queries++
			if len(q.Args) == 2 {
				if err := visit(i, []string{"RDB$FIELD_LENGTH"}, []any{int64(128)}); err != nil {
					return stats, err
				}
				continue
			}
			if strings.Contains(q.SQL, "JOIN RDB$FIELDS") {
				for _, family := range families {
					for _, col := range columns {
						if err := visit(i, nil, []any{"RDB$" + family, "RDB$" + col, int64(128)}); err != nil {
							return stats, err
						}
					}
				}
				continue
			}
			parts := strings.SplitN(strings.TrimPrefix(q.SQL, "SELECT "), " FROM ", 2)
			var labels []string
			for _, expr := range strings.Split(parts[0], ", ") {
				words := strings.Fields(expr)
				labels = append(labels, words[len(words)-1])
			}
			for _, row := range rows {
				if row.Family == parts[1] {
					values := make([]any, len(labels))
					for j, label := range labels {
						values[j] = row.Values[label]
					}
					stats.Rows++
					if err := visit(i, labels, values); err != nil {
						return stats, err
					}
				}
			}
		}
		return stats, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWatchedMappingDeletedParameterKeepsOldOwner(t *testing.T) {
	previous := watchedMappingFingerprint(t, watchedMappingRow{Family: "RDB$PROCEDURE_PARAMETERS", Values: map[string]any{"RDB$PROCEDURE_NAME": "P", "RDB$PARAMETER_NAME": "A", "RDB$PARAMETER_TYPE": int64(0), "RDB$PARAMETER_NUMBER": int64(0), "RDB$FIELD_SOURCE": "D"}})
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), previous, watchedMappingFingerprint(t))
	want := []catalog.ObjectID{{Kind: catalog.ObjectKindProcedure, Name: "P"}}
	if err != nil || got.FullReason != "" || !reflect.DeepEqual(got.Owners[catalog.MetadataProcedures], want) {
		t.Fatalf("old owner: %+v %v", got, err)
	}
}

func TestWatchedMappingEligibility(t *testing.T) {
	for _, flags := range [][2]any{{nil, int64(1)}, {int64(0), int64(1)}, {int64(1), nil}, {int64(-7), int64(0)}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			row := func(flag any) watchedMappingRow {
				return watchedMappingRow{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": "D", "RDB$SYSTEM_FLAG": flag}}
			}
			got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, row(flags[0])), watchedMappingFingerprint(t, row(flags[1])))
			if err != nil || got.FullReason != "mapping" || got.Affected != 0 || len(got.Owners) != 0 || len(got.Deleted) != 0 || len(got.DependencyOwners) != 0 {
				t.Fatalf("eligibility: %+v %v", got, err)
			}
		})
	}
}

func TestWatchedMappingDomainConsumersAndNoCallerExpansion(t *testing.T) {
	domain := watchedMappingRow{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": "RDB$GEN", "RDB$SYSTEM_FLAG": int64(1), "RDB$FIELD_LENGTH": int64(4)}}
	consumer := func(family, name string) watchedMappingRow {
		values := map[string]any{"RDB$FIELD_SOURCE": "RDB$GEN", "RDB$FIELD_NAME": "C", "RDB$PARAMETER_NAME": "A", "RDB$PARAMETER_NUMBER": int64(0), "RDB$PARAMETER_TYPE": int64(0)}
		if family == "RDB$RELATION_FIELDS" {
			values["RDB$RELATION_NAME"] = name
		} else {
			values["RDB$PROCEDURE_NAME"] = name
		}
		return watchedMappingRow{Family: family, Values: values}
	}
	caller := watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": "Q", "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": "P", "RDB$DEPENDED_ON_TYPE": int64(5)}}
	old := watchedMappingFingerprint(t, domain, consumer("RDB$RELATION_FIELDS", "T"), consumer("RDB$PROCEDURE_PARAMETERS", "P"), caller)
	domain.Values["RDB$FIELD_LENGTH"] = int64(8)
	new := watchedMappingFingerprint(t, domain, consumer("RDB$RELATION_FIELDS", "V"), consumer("RDB$PROCEDURE_PARAMETERS", "P"), caller)
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), old, new)
	if err != nil || got.FullReason != "" || got.Affected != 3 || len(got.Owners[catalog.MetadataDomains]) != 0 || len(got.Owners[catalog.MetadataColumnsCurrent]) != 2 || len(got.Owners[catalog.MetadataProcedures]) != 1 {
		t.Fatalf("support consumer routing %+v %v", got, err)
	}
}

func TestWatchedMappingDependencyOnlyAndSecurityAndVolume(t *testing.T) {
	dep := watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": "P", "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": "T", "RDB$DEPENDED_ON_TYPE": int64(0)}}
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t), watchedMappingFingerprint(t, dep))
	if err != nil || got.FullReason != "" || got.Affected != 0 || len(got.Owners) != 0 || len(got.DependencyOwners) != 1 {
		t.Fatalf("dependency-only %+v %v", got, err)
	}
	got, err = mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t), watchedMappingFingerprint(t, watchedMappingRow{Family: "RDB$USER_PRIVILEGES", Values: map[string]any{"RDB$USER": "U", "RDB$RELATION_NAME": "T"}}))
	if err != nil || got.FullReason != "visibility" {
		t.Fatalf("security %+v %v", got, err)
	}
	var many []watchedMappingRow
	for i := 0; i < 101; i++ {
		many = append(many, watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": fmt.Sprintf("P%03d", i)}})
	}
	got, err = mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t), watchedMappingFingerprint(t, many...))
	if err != nil || got.FullReason != "volume" || len(got.Owners) != 0 {
		t.Fatalf("volume %+v %v", got, err)
	}
}

func TestWatchedMappingExcludedHeadersDoNotCount(t *testing.T) {
	var rows []watchedMappingRow
	for i := 0; i < 101; i++ {
		rows = append(rows, watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": fmt.Sprintf("SYS%d", i), "RDB$SYSTEM_FLAG": int64(1)}})
	}
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t), watchedMappingFingerprint(t, rows...))
	if err != nil || got.FullReason != "" || got.Affected != 0 || len(got.Owners) != 0 {
		t.Fatalf("excluded owners counted: %+v %v", got, err)
	}
}

func TestWatchedMappingIndexFKViewAndCheckRouting(t *testing.T) {
	rows := []watchedMappingRow{
		{Family: "RDB$INDICES", Values: map[string]any{"RDB$INDEX_NAME": "IA", "RDB$RELATION_NAME": "A", "RDB$FOREIGN_KEY": "IB"}},
		{Family: "RDB$INDICES", Values: map[string]any{"RDB$INDEX_NAME": "IB", "RDB$RELATION_NAME": "B"}},
		{Family: "RDB$INDEX_SEGMENTS", Values: map[string]any{"RDB$INDEX_NAME": "IA", "RDB$FIELD_NAME": "C", "RDB$FIELD_POSITION": int64(0)}},
	}
	old := watchedMappingFingerprint(t, rows...)
	new := watchedMappingFingerprint(t, rows[:2]...)
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), old, new)
	if err != nil || got.FullReason != "" || len(got.Owners[catalog.MetadataForeignKeys]) != 2 || len(got.Owners[catalog.MetadataIndexes]) != 2 {
		t.Fatalf("index endpoints: %+v %v", got, err)
	}
	check := []watchedMappingRow{
		{Family: "RDB$RELATION_CONSTRAINTS", Values: map[string]any{"RDB$CONSTRAINT_NAME": "CK", "RDB$RELATION_NAME": "A", "RDB$CONSTRAINT_TYPE": "CHECK"}},
		{Family: "RDB$CHECK_CONSTRAINTS", Values: map[string]any{"RDB$CONSTRAINT_NAME": "CK", "RDB$TRIGGER_NAME": "CHECK_1"}},
		{Family: "RDB$TRIGGERS", Values: map[string]any{"RDB$TRIGGER_NAME": "CHECK_1", "RDB$RELATION_NAME": "A", "RDB$SYSTEM_FLAG": int64(1)}},
	}
	got, err = mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, check...), watchedMappingFingerprint(t, check[:1]...))
	if err != nil || got.FullReason != "" || got.Affected != 1 || len(got.Owners[catalog.MetadataTriggers]) != 0 || len(got.Owners[catalog.MetadataColumnsCurrent]) != 1 {
		t.Fatalf("check old owner: %+v %v", got, err)
	}
	view := func(base string) watchedMappingRow {
		return watchedMappingRow{Family: "RDB$VIEW_RELATIONS", Values: map[string]any{"RDB$VIEW_NAME": "V", "RDB$VIEW_CONTEXT": int64(0), "RDB$RELATION_NAME": base}}
	}
	got, err = mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, view("A")), watchedMappingFingerprint(t, view("B")))
	if err != nil || got.FullReason != "" || len(got.Owners[catalog.MetadataViews]) != 1 || len(got.Owners[catalog.MetadataColumnsCurrent]) != 1 {
		t.Fatalf("view context: %+v %v", got, err)
	}
}

func TestWatchedMappingExpandedDescriptorVolume(t *testing.T) {
	for _, count := range []int{99, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			old := []watchedMappingRow{{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "T", "RDB$DESCRIPTION": "old"}}}
			current := []watchedMappingRow{{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "T", "RDB$DESCRIPTION": "new"}}}
			for i := 0; i < count; i++ {
				row := watchedMappingRow{Family: "RDB$INDICES", Values: map[string]any{"RDB$INDEX_NAME": fmt.Sprintf("IX%d", i), "RDB$RELATION_NAME": "T"}}
				old = append(old, row)
				current = append(current, row)
			}
			got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, old...), watchedMappingFingerprint(t, current...))
			if err != nil {
				t.Fatal(err)
			}
			if count == 100 {
				if got.FullReason != "volume" || len(got.Owners) != 0 || len(got.DependencyOwners) != 0 {
					t.Fatalf("101 expanded descriptors bypassed limit: %+v", got)
				}
			} else if got.FullReason != "" || got.Affected != 100 {
				t.Fatalf("100 expanded descriptors did not remain incremental: %+v", got)
			}
		})
	}
}
