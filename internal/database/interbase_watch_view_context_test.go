package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

// Read actual settled fixture catalog values, then use the existing deterministic
// capability reader and production compact builder. These are real compact
// fingerprints, not hand-built records/deltas. SQLite TEXT sources are fixture
// values, not native BLOB identifiers; no native scan/counter claim is made.
func watchedViewFixtureFingerprint(t *testing.T, db *sql.DB) *schema.CatalogFingerprint {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'RDB$%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var families []string
	for rows.Next() {
		var family string
		if err := rows.Scan(&family); err != nil {
			t.Fatal(err)
		}
		families = append(families, family)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var values []watchedMappingRow
	for _, family := range families {
		// family is a catalog table name discovered from this disposable in-memory
		// test schema, not production object-filter SQL or a user-supplied name.
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(family, `"`, `""`)+`"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			cells := make([]any, len(columns))
			dest := make([]any, len(cells))
			for i := range dest {
				dest[i] = &cells[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			row := watchedMappingRow{Family: family, Values: make(map[string]any, len(columns))}
			for i, column := range columns {
				row.Values[column] = cells[i]
			}
			values = append(values, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return watchedMappingFingerprint(t, values...)
}

func watchedOrdinaryFixtureCache(t *testing.T, repo *InterBaseDBRepository) *catalog.Cache {
	t.Helper()
	loader := metadata.NewLoader(nil)
	defer loader.Stop()
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, repo.MetadataPlan())
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	snapshot := loader.Snapshot()
	if snapshot.Degraded() {
		t.Fatal("ordinary full fixture cache degraded")
	}
	return snapshot.Cache
}

func assertWatchedViewProjections(t *testing.T, got, want *catalog.Cache) {
	t.Helper()
	for _, pair := range []struct {
		name      string
		got, want any
	}{
		{"relation membership", got.SchemaTables, want.SchemaTables},
		{"exact columns", got.ColumnsByID, want.ColumnsByID},
		{"legacy columns", got.ColumnsWithParent, want.ColumnsWithParent},
		{"exact views", got.Catalog.ViewsByID, want.Catalog.ViewsByID},
		{"legacy views", got.Catalog.Views, want.Catalog.Views},
		{"exact keys", got.PrimaryKeysByID, want.PrimaryKeysByID},
		{"legacy keys", got.PrimaryKeyColumns, want.PrimaryKeyColumns},
		{"FK groups", got.ForeignKeys, want.ForeignKeys},
	} {
		if !reflect.DeepEqual(pair.got, pair.want) {
			t.Fatalf("incremental view-context %s differs from ordinary full cache", pair.name)
		}
	}
	assertWatchedGraphParity(t, got.Dependencies, want.Dependencies)
	for _, name := range []string{"WatchView", "WatchSibling"} {
		id := catalog.ObjectID{Kind: catalog.ObjectKindView, Name: name}
		g, gok := got.Catalog.Objects.Lookup(id)
		w, wok := want.Catalog.Objects.Lookup(id)
		if !gok || !wok || g != w || !reflect.DeepEqual(got.Catalog.Objects.LookupUnquoted(id.Kind, id.Name), want.Catalog.Objects.LookupUnquoted(id.Kind, id.Name)) {
			t.Fatal("exact/folded view membership differs")
		}
	}
}

func TestWatchedViewContextIncrementalMergeOrdinaryFullParity(t *testing.T) {
	ctx := context.Background()
	fault := &interBaseFixtureFault{}
	db := openInterBaseScalableFixtureWithFault(t, 2, fault)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 3}
	oldSource := "SELECT x.KEY_A AS Changed, y.KEY_B AS Stable, x.NOTES AS Keep FROM T001 x JOIN T001 y ON x.KEY_A=y.KEY_A"
	newSource := "SELECT x.LABEL AS Changed, y.KEY_B AS Stable, x.NOTES AS Keep FROM T002 x JOIN T001 y ON x.KEY_A=y.KEY_A"
	siblingSource := "SELECT KEY_B AS Sibling FROM T001 /*" + strings.Repeat("unchanged", 1000) + "*/"
	if _, err := db.Exec(`ALTER TABLE "RDB$VIEW_RELATIONS" ADD COLUMN "RDB$CONTEXT_NAME" TEXT`); err != nil {
		t.Fatal(err)
	}
	for _, view := range []struct{ name, source string }{{"WatchView", oldSource}, {"WatchSibling", siblingSource}} {
		if _, err := db.Exec(`INSERT INTO "RDB$RELATIONS" ("RDB$RELATION_NAME","RDB$VIEW_BLR","RDB$VIEW_SOURCE","RDB$OWNER_NAME","RDB$SYSTEM_FLAG") VALUES (?,X'01',?,'OWNER',0)`, view.name, view.source); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]any{{"WatchView", 0, "T001", "x"}, {"WatchView", 1, "T001", "y"}, {"WatchSibling", 0, "T001", "s"}} {
		if _, err := db.Exec(`INSERT INTO "RDB$VIEW_RELATIONS" ("RDB$VIEW_NAME","RDB$VIEW_CONTEXT","RDB$RELATION_NAME","RDB$CONTEXT_NAME") VALUES (?,?,?,?)`, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]any{{"WatchView", "Changed", "D_KEY", 0, 0, "KEY_A"}, {"WatchView", "Stable", "D_KEY", 1, 1, "KEY_B"}, {"WatchView", "Keep", "D_NOTES", 2, 0, "NOTES"}, {"WatchSibling", "Sibling", "D_KEY", 0, 0, "KEY_B"}} {
		if _, err := db.Exec(`INSERT INTO "RDB$RELATION_FIELDS" ("RDB$RELATION_NAME","RDB$FIELD_NAME","RDB$FIELD_SOURCE","RDB$FIELD_POSITION","RDB$VIEW_CONTEXT","RDB$BASE_FIELD","RDB$SYSTEM_FLAG") VALUES (?,?,?,?,?,?,0)`, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]any{{"WatchView", "T001", "KEY_A"}, {"WatchView", "T001", "KEY_B"}, {"WatchView", "T001", "NOTES"}, {"WatchSibling", "T001", "KEY_B"}} {
		if _, err := db.Exec(`INSERT INTO "RDB$DEPENDENCIES" ("RDB$DEPENDENT_NAME","RDB$DEPENDENT_TYPE","RDB$DEPENDED_ON_NAME","RDB$DEPENDED_ON_TYPE","RDB$FIELD_NAME") VALUES (?,0,?,0,?)`, args...); err != nil {
			t.Fatal(err)
		}
	}
	previous := watchedViewFixtureFingerprint(t, db)
	base := watchedOrdinaryFixtureCache(t, repo)
	original := watchedOrdinaryFixtureCache(t, repo) // Separate objects prove deep immutability, not a shallow copy.
	oldView, ok := base.ViewByID("WatchView")
	if !ok || len(oldView.Columns) != 3 || oldView.Columns[0].Type != "INTEGER" || oldView.ViewSource.String != oldSource {
		t.Fatal("baseline view fixture is not the intended old state")
	}
	sibling, ok := base.ViewByID("WatchSibling")
	if !ok {
		t.Fatal("missing sibling fixture")
	}
	unrelatedDomain, ok := base.DomainByID("D_KEY")
	if !ok {
		t.Fatal("missing unchanged domain")
	}
	oldGraph := base.Dependencies

	// Persistent exact view/header/column identities, owner/security/system flag
	// unchanged: only context 0's base, Changed's base field/domain, one edge and
	// the existing view's source text change. Context 1 and both siblings remain.
	for _, change := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE "RDB$VIEW_RELATIONS" SET "RDB$RELATION_NAME"='T002' WHERE "RDB$VIEW_NAME"='WatchView' AND "RDB$VIEW_CONTEXT"=0`, nil},
		{`UPDATE "RDB$RELATION_FIELDS" SET "RDB$BASE_FIELD"='LABEL',"RDB$FIELD_SOURCE"='D_LABEL' WHERE "RDB$RELATION_NAME"='WatchView' AND "RDB$FIELD_NAME"='Changed'`, nil},
		{`UPDATE "RDB$RELATIONS" SET "RDB$VIEW_SOURCE"=? WHERE "RDB$RELATION_NAME"='WatchView'`, []any{newSource}},
		{`UPDATE "RDB$DEPENDENCIES" SET "RDB$DEPENDED_ON_NAME"='T002',"RDB$FIELD_NAME"='LABEL' WHERE "RDB$DEPENDENT_NAME"='WatchView' AND "RDB$FIELD_NAME"='KEY_A'`, nil},
	} {
		result, err := db.Exec(change.sql, change.args...)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			t.Fatal("view-context fixture did not update exactly one row")
		}
	}
	candidate := watchedViewFixtureFingerprint(t, db)
	if previous.Equal(candidate) || previous.RecordCount() != candidate.RecordCount() {
		t.Fatal("context change did not produce persistent-row compact fingerprint change")
	}
	checkRoutes := func(fp *schema.CatalogFingerprint, baseRelation, baseField, domain string) {
		t.Helper()
		context0, context1, column, domainLink := false, false, false, false
		fp.VisitRecords(func(row schema.FingerprintRecord) bool {
			for _, link := range row.Links {
				if row.Family == "RDB$VIEW_RELATIONS" && link.From.Parent == "WatchView" {
					if link.From.Name == "0" {
						if link.To.Name != baseRelation || link.Field != "x" {
							t.Error("wrong context-0 base/alias routing")
						}
						context0 = true
					}
					if link.From.Name == "1" && link.To.Name == "T001" && link.Field == "y" {
						context1 = true
					}
				}
				if row.Family == "RDB$RELATION_FIELDS" && link.From.Kind == "view column" && link.From.Parent == "WatchView" && link.From.Name == "Changed" {
					if link.To.Name != "0" || link.Field != baseField {
						t.Error("old/wrong base-field link survived")
					}
					column = true
				}
				if row.Family == "RDB$RELATION_FIELDS" && link.From.Name == "WatchView" && link.To.Kind == "domain" && link.Field == "Changed" {
					if link.To.Name != domain {
						t.Error("old/wrong field-source link survived")
					}
					domainLink = true
				}
			}
			return true
		})
		if !context0 || !context1 || !column || !domainLink {
			t.Fatal("actual compact fixture fingerprint lacks required view/field routes")
		}
	}
	checkRoutes(previous, "T001", "KEY_A", "D_KEY")
	checkRoutes(candidate, "T002", "LABEL", "D_LABEL")

	var sourceQueries []watchedRecordedQuery
	fault.onQueryArgs = func(query string, args []driver.NamedValue) {
		if strings.Contains(query, "r.RDB$VIEW_SOURCE") || strings.Contains(query, "f.RDB$COMPUTED_SOURCE") {
			values := make([]any, len(args))
			for i, arg := range args {
				values[i] = arg.Value
			}
			sourceQueries = append(sourceQueries, watchedRecordedQuery{query, values})
		}
	}
	delta, err := repo.ReadWatchedDelta(ctx, base, previous, candidate)
	fault.onQueryArgs = nil
	if err != nil {
		t.Fatal(err)
	}
	viewID := catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "WatchView"}
	columnID := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "WatchView"}
	if delta.FullReason != "" || len(delta.Deleted) != 0 || !reflect.DeepEqual(delta.Owners[catalog.MetadataViews], []catalog.ObjectID{viewID}) || !reflect.DeepEqual(delta.Owners[catalog.MetadataColumnsCurrent], []catalog.ObjectID{columnID}) {
		t.Fatal("persistent context selected fallback/deletion/incomplete view or column coverage")
	}
	viewPatch := delta.Patches[catalog.MetadataViews].Cache
	if changed, ok := viewPatch.ViewByID("WatchView"); !ok || len(changed.Columns) != 3 || changed.ViewSource.String != newSource {
		t.Fatal("scoped complete view replacement missing updated source/sibling columns")
	}
	if _, ok := viewPatch.ViewByID("WatchSibling"); ok {
		t.Fatal("unrelated sibling source appeared in patch")
	}
	if len(sourceQueries) != 3 {
		t.Fatalf("expected one view-source and two scoped column projections, got %d", len(sourceQueries))
	}
	for _, query := range sourceQueries {
		filter := "rf.RDB$RELATION_NAME IN (?)"
		if strings.Contains(query.sql, "r.RDB$VIEW_SOURCE") {
			filter = "r.RDB$RELATION_NAME IN (?)"
		}
		if !strings.Contains(query.sql, filter) || strings.Contains(query.sql, "WatchView") || !reflect.DeepEqual(query.args, []any{"WatchView"}) {
			t.Fatal("view/column source projection was not exact single-name parameterized")
		}
	}
	got, err := metadata.MergeObjects(ctx, base, delta)
	if err != nil {
		t.Fatal(err)
	}
	want := watchedOrdinaryFixtureCache(t, repo) // Ordinary jobs; never ReadWatchedFull/readWatchedCategory for expected data.
	assertWatchedViewProjections(t, got, want)
	assertWatchedViewProjections(t, base, original)
	newView, ok := got.ViewByID("WatchView")
	if !ok || len(newView.Columns) != 3 || newView.Columns[0].Name != "Changed" || newView.Columns[0].Type != "VARCHAR(20)" || newView.Columns[0].Facts == nil || newView.Columns[0].Facts.DomainName.String != "D_LABEL" || newView.Columns[0].Default.String != "'domain'" || newView.Columns[0].Facts.DomainDefault.String != " DEFAULT 'domain' " || newView.ViewSource.String != newSource {
		t.Fatal("new field/type/domain/default/source facts missing")
	}
	for _, index := range []int{1, 2} {
		if !reflect.DeepEqual(newView.Columns[index], oldView.Columns[index]) {
			t.Fatal("unchanged columns inside affected view lost")
		}
	}
	if current, ok := got.ViewByID("WatchSibling"); !ok || current != sibling {
		t.Fatal("unrelated sibling descriptor identity changed")
	}
	if current, ok := got.DomainByID("D_KEY"); !ok || current != unrelatedDomain {
		t.Fatal("unrelated domain descriptor identity changed")
	}
	if base.Dependencies != oldGraph {
		t.Fatal("old graph replaced in base")
	}
	// Independent schema accessor exposes base mapping facts that the ordinary
	// bulk cache intentionally does not model as ColumnDesc fields.
	relation, err := schema.New(db).View(ctx, "WatchView")
	if err != nil {
		t.Fatal(err)
	}
	if relation == nil || len(relation.Columns) != 3 || relation.Columns[0].BaseRelation.String != "T002" || relation.Columns[0].BaseField.String != "LABEL" || relation.Columns[1].BaseRelation.String != "T001" || relation.Columns[1].BaseField.String != "KEY_B" {
		t.Fatal("settled independent view base mapping incorrect")
	}
	oldEdge := catalog.DependencyEdge{From: catalog.DependencyRef{Kind: catalog.ObjectKindView, Name: "WatchView"}, To: catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "T001"}, Field: "KEY_A"}
	newEdge := oldEdge
	newEdge.To.Name = "T002"
	newEdge.Field = "LABEL"
	seenNew := false
	for _, edge := range got.Dependencies.Edges() {
		if edge == oldEdge {
			t.Fatal("old context edge survived")
		}
		if edge == newEdge {
			seenNew = true
		}
	}
	if !seenNew {
		t.Fatal("updated context edge missing")
	}
	t.Logf("persistent view context incremental parity: compact records=%d; complete view columns=3; source projections=%d exact WatchView bind; sibling source bytes excluded=%d; ordinary full expected path", candidate.RecordCount(), len(sourceQueries), len(sibling.ViewSource.String))
}
