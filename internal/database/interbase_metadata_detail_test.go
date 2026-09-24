package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// interBaseTestProvider adapts InterBaseDBRepository to metadata.Provider
// using exactly the same MetadataPlan/MetadataDetailPlan a real
// metadata/interbase provider delegates to. This is what makes this test's
// query counts a proof about the actual adaptation, not a stand-in.
type interBaseTestProvider struct{ db *InterBaseDBRepository }

func (p *interBaseTestProvider) Plan() catalog.MetadataPlan { return p.db.MetadataPlan() }
func (p *interBaseTestProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	return p.db.MetadataDetailPlan(req)
}
func (p *interBaseTestProvider) Close() error { return nil }

func acquireInterBaseTestLease(t *testing.T, db *InterBaseDBRepository) (*metadata.Registry, *metadata.Lease) {
	t.Helper()
	ctx := context.Background()
	registry := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3, MaxDetailBytes: 64 << 20})
	t.Cleanup(func() { _ = registry.Close(ctx) })
	factory := func(context.Context) (metadata.Provider, error) { return &interBaseTestProvider{db: db}, nil }
	lease, err := registry.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "test://" + t.Name()}, metadata.Scope{Principal: "TESTER"}, factory)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(lease.Release)
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	return registry, lease
}

// TestInterBaseMetadataProviderDetailReusesBulkFacts is Task A5 Step 1's
// decisive query-count regression: every listed (first, second) pair issues
// zero additional relation/column/procedure-source reads for the second
// step. The registry settles fully (every bulk category reaches a terminal
// state) before either step, which is the same synchronous checkpoint the
// existing bulk-query tests use with interBaseFixtureCountPrepares; the
// fixture's prepare counter is process-global and asserted not to run tests
// in parallel with each other, so measuring across still-running background
// jobs would be inherently racy rather than a query-count proof. Settling
// first also proves something strictly stronger than the required "second
// costs zero relative to first": every detail request here costs zero
// queries at all once its bulk category is ready, in either order.
func TestInterBaseMetadataProviderDetailReusesBulkFacts(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	insertInterBaseDetailRegressionFixtures(t, db)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	table := catalog.ObjectID{Kind: ObjectKindTable, Name: "CUSTOMER"}
	procedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: "ADD_CUSTOMER"}
	ddlProcedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: "Mixed_Case_Proc"}

	requests := map[string]metadata.Request{
		"columns":     {Object: table, Facet: "columns", Consistency: metadata.Committed},
		"description": {Object: table, Facet: "description", Dialect: 3, Consistency: metadata.Committed},
		"source":      {Object: procedure, Facet: "source", Consistency: metadata.Committed},
		"ddl":         {Object: ddlProcedure, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed},
		// procedure-category requests the SAME object as "ddl" (ddlProcedure),
		// under the "source" facet, which Requires only MetadataProcedures —
		// the actual procedure category, not an unrelated one. This is what
		// "loaded procedure then ddl" below is required to exercise: the
		// procedure category already loaded, then a DDL request against the
		// exact same procedure, costing nothing extra.
		"procedure-category": {Object: ddlProcedure, Facet: "source", Consistency: metadata.Committed},
	}

	cases := []struct {
		name          string
		first, second string
		wantRepeat    int
	}{
		{"explorer then hover", "columns", "description", 0},
		{"hover then explorer", "description", "columns", 0},
		{"source then definition", "source", "source", 0},
		{"loaded procedure then ddl", "procedure-category", "ddl", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, lease := acquireInterBaseTestLease(t, repo)
			count := interBaseFixtureCountPrepares(t)

			first, err := lease.Detail(context.Background(), requests[c.first])
			if err != nil {
				t.Fatalf("first Detail(%s) error = %v", c.first, err)
			}
			if got := count(); got != 0 {
				t.Fatalf("queries after first Detail(%s) = %d, want 0 (bulk facts already settled)", c.first, got)
			}

			second, err := lease.Detail(context.Background(), requests[c.second])
			if err != nil {
				t.Fatalf("second Detail(%s) error = %v", c.second, err)
			}
			if got := count(); got != int64(c.wantRepeat) {
				t.Fatalf("queries after second Detail(%s) = %d, want %d", c.second, got, c.wantRepeat)
			}

			if c.first == c.second {
				if first.Text != second.Text {
					t.Fatalf("identical request returned different text: %q vs %q", first.Text, second.Text)
				}
			}
		})
	}
}

// TestInterBaseMetadataProviderDetailJoinsInFlightBulkLoad is Review Focus
// #3's actual scenario, which TestInterBaseMetadataProviderDetailReusesBulkFacts
// deliberately does not exercise: a detail request arriving WHILE its
// required bulk category may still be loading must join/share that
// in-flight load, not just avoid re-querying after everything has already
// settled. It compares the total query count of a baseline run (full
// settlement first, no racing) against a run that fires every request
// concurrently immediately after Acquire, before ever waiting for
// readiness. If joining an in-flight bulk category were broken (a bypassed
// dependency wait, a duplicate job launch, a race in the new Views/Columns
// DependsOn wiring), the racing run's total would diverge from the
// deterministic baseline.
func TestInterBaseMetadataProviderDetailJoinsInFlightBulkLoad(t *testing.T) {
	newFixtureRepo := func(t *testing.T) *InterBaseDBRepository {
		t.Helper()
		db := openInterBaseSchemaFixture(t)
		insertInterBaseDetailRegressionFixtures(t, db)
		return &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	}

	requests := []metadata.Request{
		{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "CUSTOMER"}, Facet: "columns", Consistency: metadata.Committed},
		{Object: catalog.ObjectID{Kind: ObjectKindProcedure, Name: "ADD_CUSTOMER"}, Facet: "source", Consistency: metadata.Committed},
		{Object: catalog.ObjectID{Kind: ObjectKindDomain, Name: "EMAIL_ADDRESS"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed},
		{Object: catalog.ObjectID{Kind: ObjectKindTrigger, Name: "CUSTOMER_BI"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed},
		{Object: catalog.ObjectID{Kind: ObjectKindIndex, Name: "IDX_CUSTOMER_PK"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed},
		{Object: catalog.ObjectID{Kind: ObjectKindGenerator, Name: "GEN_CUSTOMER_ID"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed},
		{Object: catalog.ObjectID{Kind: ObjectKindView, Name: "CUSTOMER_VIEW"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed},
	}

	// Baseline: settle the whole plan first, with no detail request racing
	// it, then serve every one of the same requests; this is the
	// deterministic per-fixture total the racing run below must match.
	baselineCounter := interBaseFixtureCountPrepares(t)
	_, baselineLease := acquireInterBaseTestLease(t, newFixtureRepo(t))
	for _, req := range requests {
		if _, err := baselineLease.Detail(context.Background(), req); err != nil {
			t.Fatalf("baseline Detail(%s) error = %v", req.Facet, err)
		}
	}
	baselineQueries := baselineCounter()

	// Concurrent: acquire WITHOUT waiting for readiness, then fire every
	// request concurrently -- racing the still-scheduling/still-running
	// bulk plan -- before ever settling, then finish settling and compare.
	concurrentCounter := interBaseFixtureCountPrepares(t)
	ctx := context.Background()
	registry := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3, MaxDetailBytes: 64 << 20})
	t.Cleanup(func() { _ = registry.Close(ctx) })
	provider := &interBaseTestProvider{db: newFixtureRepo(t)}
	factory := func(context.Context) (metadata.Provider, error) { return provider, nil }
	lease, err := registry.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "test://" + t.Name()}, metadata.Scope{Principal: "TESTER"}, factory)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(lease.Release)

	var wg sync.WaitGroup
	errs := make([]error, len(requests))
	for i, req := range requests {
		wg.Add(1)
		go func(i int, req metadata.Request) {
			defer wg.Done()
			_, errs[i] = lease.Detail(ctx, req)
		}(i, req)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racing Detail(%s) error = %v", requests[i].Facet, err)
		}
	}

	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if got := concurrentCounter(); got != baselineQueries {
		t.Fatalf("total queries racing detail requests against the in-flight bulk load = %d, want %d (same as the fully-settled baseline; joining an in-flight bulk category must not cause an extra query)", got, baselineQueries)
	}
}

// TestInterBaseMetadataProviderDetailRichFacts asserts the actual rendered
// content, not just query counts: long/mixed-case identifiers, Domain
// nullability/precision/scale, and dialect 1 vs 3 rendering differences all
// come from retained bulk facts.
func TestInterBaseMetadataProviderDetailRichFacts(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	insertInterBaseDetailRegressionFixtures(t, db)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	_, lease := acquireInterBaseTestLease(t, repo)
	ctx := context.Background()

	t.Run("mixed case procedure ddl quotes the exact identifier", func(t *testing.T) {
		procedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: "Mixed_Case_Proc"}
		d3, err := lease.Detail(ctx, metadata.Request{Object: procedure, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(dialect 3) error = %v", err)
		}
		if !strings.Contains(d3.Text, `"Mixed_Case_Proc"`) {
			t.Fatalf("dialect-3 ddl = %q, want quoted mixed-case name", d3.Text)
		}
		if !strings.Contains(d3.Text, "TIMESTAMP") {
			t.Fatalf("dialect-3 ddl = %q, want TIMESTAMP parameter type", d3.Text)
		}
	})

	// Dialect 1 cannot represent a mixed-case identifier at all (it has no
	// quoting), so the dialect-sensitive TIMESTAMP-vs-DATE rendering is
	// exercised on a separate, all-uppercase procedure sharing the same
	// system-generated timestamp domain.
	t.Run("procedure ddl renders dialect-correct type from one retained load", func(t *testing.T) {
		procedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: "TS_PROC"}
		d3, err := lease.Detail(ctx, metadata.Request{Object: procedure, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(dialect 3) error = %v", err)
		}
		if !strings.Contains(d3.Text, "TIMESTAMP") {
			t.Fatalf("dialect-3 ddl = %q, want TIMESTAMP parameter type", d3.Text)
		}

		d1, err := lease.Detail(ctx, metadata.Request{Object: procedure, Facet: "ddl", Dialect: 1, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(dialect 1) error = %v", err)
		}
		if !strings.Contains(d1.Text, "DATE") || strings.Contains(d1.Text, "TIMESTAMP") {
			t.Fatalf("dialect-1 ddl = %q, want DATE instead of TIMESTAMP", d1.Text)
		}
	})

	t.Run("domain nullability precision scale", func(t *testing.T) {
		domain := catalog.ObjectID{Kind: ObjectKindDomain, Name: "EMAIL_ADDRESS"}
		got, err := lease.Detail(ctx, metadata.Request{Object: domain, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(domain ddl) error = %v", err)
		}
		if !strings.Contains(got.Text, "NOT NULL") || !strings.Contains(got.Text, "CHECK") {
			t.Fatalf("domain ddl = %q, want NOT NULL and CHECK clauses", got.Text)
		}
	})

	t.Run("scaled numeric domain precision and scale", func(t *testing.T) {
		domain := catalog.ObjectID{Kind: ObjectKindDomain, Name: "CUSTOMER_AMOUNT"}
		got, err := lease.Detail(ctx, metadata.Request{Object: domain, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(scaled numeric domain ddl) error = %v", err)
		}
		if !strings.Contains(got.Text, "NUMERIC(9, 2)") {
			t.Fatalf("domain ddl = %q, want NUMERIC(9, 2)", got.Text)
		}
	})

	t.Run("view ddl from retained columns", func(t *testing.T) {
		view := catalog.ObjectID{Kind: ObjectKindView, Name: "CUSTOMER_VIEW"}
		got, err := lease.Detail(ctx, metadata.Request{Object: view, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(view ddl) error = %v", err)
		}
		if !strings.Contains(got.Text, "CREATE VIEW") || !strings.Contains(got.Text, "SELECT ID FROM CUSTOMER") {
			t.Fatalf("view ddl = %q, want CREATE VIEW wrapping the retained source", got.Text)
		}
	})

	t.Run("index ddl direction from retained facts", func(t *testing.T) {
		index := catalog.ObjectID{Kind: ObjectKindIndex, Name: "IDX_CUSTOMER_PK"}
		got, err := lease.Detail(ctx, metadata.Request{Object: index, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(index ddl) error = %v", err)
		}
		if !strings.Contains(got.Text, "CREATE UNIQUE ASCENDING INDEX") {
			t.Fatalf("index ddl = %q, want CREATE UNIQUE ASCENDING INDEX", got.Text)
		}
	})

	t.Run("trigger ddl from retained type code", func(t *testing.T) {
		trigger := catalog.ObjectID{Kind: ObjectKindTrigger, Name: "CUSTOMER_BI"}
		got, err := lease.Detail(ctx, metadata.Request{Object: trigger, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(trigger ddl) error = %v", err)
		}
		if !strings.Contains(got.Text, "BEFORE INSERT") {
			t.Fatalf("trigger ddl = %q, want BEFORE INSERT", got.Text)
		}
	})

	t.Run("generator ddl", func(t *testing.T) {
		generator := catalog.ObjectID{Kind: ObjectKindGenerator, Name: "GEN_CUSTOMER_ID"}
		got, err := lease.Detail(ctx, metadata.Request{Object: generator, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(generator ddl) error = %v", err)
		}
		if got.Text != `CREATE GENERATOR "GEN_CUSTOMER_ID"` {
			t.Fatalf("generator ddl = %q", got.Text)
		}
	})

	t.Run("unquoted lookup resolves case-folded name", func(t *testing.T) {
		procedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: "add_customer"}
		got, err := lease.Detail(ctx, metadata.Request{Object: procedure, Facet: "source", Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(unquoted procedure source) error = %v", err)
		}
		if !strings.Contains(got.Text, "NEW_ID = 1") {
			t.Fatalf("source = %q, want the ADD_CUSTOMER body", got.Text)
		}
	})

	// TestInterBaseMetadataProviderDetailAmbiguousUnquotedLookup is the
	// required regression for Ruling R6, exercised through the real
	// InterBase detail path rather than catalog.ObjectIndex directly: two
	// exact-identity domains that legitimately share the same case-folded
	// name must both remain independently resolvable by exact ObjectID, and
	// an unquoted lookup that matches neither exactly must report the
	// ambiguity explicitly rather than silently choosing one.
	t.Run("ambiguous unquoted lookup does not silently pick a winner", func(t *testing.T) {
		exactMixed := catalog.ObjectID{Kind: ObjectKindDomain, Name: "Ambig_Domain"}
		exactUpper := catalog.ObjectID{Kind: ObjectKindDomain, Name: "AMBIG_DOMAIN"}

		mixed, err := lease.Detail(ctx, metadata.Request{Object: exactMixed, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(exact mixed-case domain) error = %v", err)
		}
		if !strings.Contains(mixed.Text, `"Ambig_Domain"`) {
			t.Fatalf("mixed-case domain ddl = %q, want the exact mixed-case identity", mixed.Text)
		}

		upper, err := lease.Detail(ctx, metadata.Request{Object: exactUpper, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(exact upper-case domain) error = %v", err)
		}
		if !strings.Contains(upper.Text, `"AMBIG_DOMAIN"`) {
			t.Fatalf("upper-case domain ddl = %q, want the exact upper-case identity", upper.Text)
		}
		if mixed.Text == upper.Text {
			t.Fatal("two distinct exact identities collapsed onto one rendered DDL")
		}

		ambiguous := catalog.ObjectID{Kind: ObjectKindDomain, Name: "ambig_domain"}
		_, err = lease.Detail(ctx, metadata.Request{Object: ambiguous, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if !errors.Is(err, catalog.ErrAmbiguous) {
			t.Fatalf("Detail(ambiguous unquoted domain name) error = %v, want errors.Is(err, catalog.ErrAmbiguous)", err)
		}
		if errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("Detail(ambiguous unquoted domain name) error = %v, want ErrAmbiguous, not ErrObjectNotFound", err)
		}
	})

	t.Run("long identifier round-trips through exact identity", func(t *testing.T) {
		longName := "LONG_IDENTIFIER_EXCEEDING_31_BYTES_OK"
		procedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: longName}
		got, err := lease.Detail(ctx, metadata.Request{Object: procedure, Facet: "source", Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(long-identifier procedure source) error = %v", err)
		}
		if !strings.Contains(got.Text, "SELECT 1 FROM RDB$DATABASE") {
			t.Fatalf("source = %q, want the seeded body", got.Text)
		}
	})

	t.Run("CP1252 source text retained verbatim", func(t *testing.T) {
		procedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: "CP1252_SOURCE_PROC"}
		got, err := lease.Detail(ctx, metadata.Request{Object: procedure, Facet: "source", Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(CP1252 procedure source) error = %v", err)
		}
		want := "BEGIN\n  --\xe9\xe8\xe0 M\xfcller caf\xe9\n  SELECT 1 FROM RDB$DATABASE;\nEND"
		if got.Text != want {
			t.Fatalf("source = %q, want %q (non-ASCII bytes preserved verbatim)", got.Text, want)
		}
	})

	t.Run("ordered composite key preserved in index ddl", func(t *testing.T) {
		// PARENT's primary key is a composite (PARENT_B, PARENT_A), seeded
		// deliberately out of position order (see openInterBaseSchemaFixture)
		// so a loader trusting insertion order instead of segment position
		// would fail this. Segment order in the rendered DDL must match
		// RDB$FIELD_POSITION exactly: (PARENT_B, PARENT_A), not reversed.
		index := catalog.ObjectID{Kind: ObjectKindIndex, Name: "IDX_PARENT_PK"}
		got, err := lease.Detail(ctx, metadata.Request{Object: index, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(composite index ddl) error = %v", err)
		}
		if !strings.Contains(got.Text, `("PARENT_B", "PARENT_A")`) {
			t.Fatalf("index ddl = %q, want ordered composite key (PARENT_B, PARENT_A)", got.Text)
		}
	})

	t.Run("consistency levels not yet supported", func(t *testing.T) {
		for _, consistency := range []metadata.Consistency{metadata.TransactionLocal, metadata.ConsistentSnapshot} {
			_, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "CUSTOMER"}, Facet: "columns", Consistency: consistency})
			if err == nil {
				t.Fatalf("Detail(%s) error = nil, want metadata.ErrUnsupported", consistency)
			}
		}
	})

	t.Run("table and function ddl remain unsupported, not a full refetch", func(t *testing.T) {
		count := interBaseFixtureCountPrepares(t)
		if _, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "CUSTOMER"}, Facet: "ddl", Consistency: metadata.Committed}); err == nil {
			t.Fatalf("Detail(table ddl) error = nil, want unsupported")
		}
		if _, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindFunction, Name: "F_LTRIM"}, Facet: "ddl", Consistency: metadata.Committed}); err == nil {
			t.Fatalf("Detail(function ddl) error = nil, want unsupported")
		}
		if got := count(); got != 0 {
			t.Fatalf("queries for unsupported ddl facets = %d, want 0 (no full-catalog refetch)", got)
		}
	})
}

// TestInterBaseMetadataProviderDetailColumnsDoNotMergeAcrossExactIdentities
// is the required Critical-bug regression: interBaseResolveRelationName
// correctly resolves an exact table name, but reading columns via the
// shared upper-cased ColumnDescs/ColumnDatabaseKey path would silently
// merge two exact-identity tables ("Foo" and FOO) that fold to the same
// case into one answer. A columns/description request for exact "Foo" must
// return only Foo's columns, a request for exact FOO must return only
// FOO's columns (disjoint from Foo's), and an unquoted request for "foo"
// must report the ambiguity explicitly rather than silently merging or
// picking one.
func TestInterBaseMetadataProviderDetailColumnsDoNotMergeAcrossExactIdentities(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	insertInterBaseDetailRegressionFixtures(t, db)
	insertInterBaseMixedCaseTableFixtures(t, db)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	_, lease := acquireInterBaseTestLease(t, repo)
	ctx := context.Background()

	mixed := catalog.ObjectID{Kind: ObjectKindTable, Name: "Foo"}
	upper := catalog.ObjectID{Kind: ObjectKindTable, Name: "FOO"}

	mixedDetail, err := lease.Detail(ctx, metadata.Request{Object: mixed, Facet: "columns", Consistency: metadata.Committed})
	if err != nil {
		t.Fatalf("Detail(exact mixed-case table columns) error = %v", err)
	}
	mixedNames := columnNames(mixedDetail.Columns)
	if len(mixedNames) != 1 || mixedNames[0] != "FOO_ONLY_COLUMN" {
		t.Fatalf("Foo columns = %v, want exactly [FOO_ONLY_COLUMN]", mixedNames)
	}

	upperDetail, err := lease.Detail(ctx, metadata.Request{Object: upper, Facet: "columns", Consistency: metadata.Committed})
	if err != nil {
		t.Fatalf("Detail(exact upper-case table columns) error = %v", err)
	}
	upperNames := columnNames(upperDetail.Columns)
	if len(upperNames) != 1 || upperNames[0] != "UPPER_FOO_ONLY_COLUMN" {
		t.Fatalf("FOO columns = %v, want exactly [UPPER_FOO_ONLY_COLUMN]", upperNames)
	}

	// description must not describe a merged Frankenstein table either.
	mixedDesc, err := lease.Detail(ctx, metadata.Request{Object: mixed, Facet: "description", Dialect: 3, Consistency: metadata.Committed})
	if err != nil {
		t.Fatalf("Detail(exact mixed-case table description) error = %v", err)
	}
	if !strings.Contains(mixedDesc.Description.Body, "FOO_ONLY_COLUMN") || strings.Contains(mixedDesc.Description.Body, "UPPER_FOO_ONLY_COLUMN") {
		t.Fatalf("Foo description = %q, want only FOO_ONLY_COLUMN, not UPPER_FOO_ONLY_COLUMN", mixedDesc.Description.Body)
	}

	// An unquoted lookup that matches both "Foo" and FOO exactly-differently
	// must report the ambiguity, not silently merge or pick one.
	ambiguous := catalog.ObjectID{Kind: ObjectKindTable, Name: "foo"}
	_, err = lease.Detail(ctx, metadata.Request{Object: ambiguous, Facet: "columns", Consistency: metadata.Committed})
	if !errors.Is(err, catalog.ErrAmbiguous) {
		t.Fatalf("Detail(ambiguous unquoted table name) error = %v, want errors.Is(err, catalog.ErrAmbiguous)", err)
	}
}

func columnNames(columns []*ColumnDesc) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.Name)
	}
	return names
}

// insertInterBaseMixedCaseTableFixtures seeds two exact-identity tables,
// "Foo" and FOO, whose names fold to the same upper-cased value but whose
// columns are deliberately disjoint (FOO_ONLY_COLUMN vs
// UPPER_FOO_ONLY_COLUMN) so a silent column-merge across the two exact
// identities is directly observable.
func insertInterBaseMixedCaseTableFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$FIELD_LENGTH","RDB$SYSTEM_FLAG") VALUES (?,?,?,?)`,
		"D_FOO_COLUMN", 8, 4, 0); err != nil {
		t.Fatal(err)
	}
	relation := func(name string, id int) {
		if _, err := db.Exec(`INSERT INTO "RDB$RELATIONS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, id, nil, nil, nil, interBaseFixed("SYSDBA"), nil, 8, 1, nil, 0,
			interBaseFixed("PERSISTENT"), 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	field := func(relationName, name string, position int) {
		if _, err := db.Exec(`INSERT INTO "RDB$RELATION_FIELDS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			interBaseFixed(name), relationName, "D_FOO_COLUMN", position, nil, position, nil, 0, nil,
			nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	relation("Foo", 90001)
	field("Foo", "FOO_ONLY_COLUMN", 0)
	relation("FOO", 90002)
	field("FOO", "UPPER_FOO_ONLY_COLUMN", 0)
}

// insertInterBaseDetailRegressionFixtures seeds a mixed-case procedure whose
// only DATE_VALUE-shaped parameter renders differently per dialect (TIMESTAMP
// vs DATE), so dialect-correct rendering can be asserted from one retained
// Load shared across dialects.
func insertInterBaseDetailRegressionFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	// SystemFlag=1 marks this an inline-type (system-generated) domain, so
	// parameterDefinition renders its type inline instead of by domain-name
	// reference — otherwise the Dialect 1/3 TIMESTAMP-vs-DATE distinction
	// this fixture exists to exercise would never surface in the DDL text.
	if _, err := db.Exec(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$SYSTEM_FLAG","RDB$NULL_FLAG") VALUES (?,?,?,?)`, "TS_DOMAIN", 35, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`,
		"Mixed_Case_Proc", 70000, 1, 0, "  mixed case  ", "BEGIN\n  SELECT 1 FROM RDB$DATABASE;\nEND", nil, interBaseFixed("OWNER"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURE_PARAMETERS" VALUES (?,?,?,?,?,?,?)`,
		interBaseFixed("WHEN"), "Mixed_Case_Proc", 0, 0, "TS_DOMAIN", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`,
		"TS_PROC", 70001, 1, 0, nil, "BEGIN\n  SELECT 1 FROM RDB$DATABASE;\nEND", nil, interBaseFixed("OWNER"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURE_PARAMETERS" VALUES (?,?,?,?,?,?,?)`,
		interBaseFixed("TS_PARAM"), "TS_PROC", 0, 0, "TS_DOMAIN", nil, 0); err != nil {
		t.Fatal(err)
	}
	// A long identifier (>31 bytes), InterBase's classic identifier-length
	// edge case: pre-Firebird-4 InterBase limits identifiers to 31 bytes,
	// but this driver's bulk queries cast through
	// interBaseBulkIdentifierCastWidth (67 bytes), so a name in the 32-67
	// range must round-trip through the exact-identity path exactly, not
	// get silently truncated or misresolved.
	longName := "LONG_IDENTIFIER_EXCEEDING_31_BYTES_OK"
	if len(longName) <= 31 {
		t.Fatalf("fixture bug: longName %q is only %d bytes, want >31", longName, len(longName))
	}
	if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`,
		longName, 70002, 0, 0, nil, "BEGIN\n  SELECT 1 FROM RDB$DATABASE;\nEND", nil, interBaseFixed("OWNER"), 0); err != nil {
		t.Fatal(err)
	}
	// CP1252/non-ASCII source text: the NONE-charset/CP1252 decoding path
	// this whole effort cares about must preserve non-ASCII bytes in
	// retained source verbatim, never mangling them on the retained-fact
	// path this task builds.
	if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`,
		"CP1252_SOURCE_PROC", 70003, 0, 0, nil, "BEGIN\n  --\xe9\xe8\xe0 M\xfcller caf\xe9\n  SELECT 1 FROM RDB$DATABASE;\nEND", nil, interBaseFixed("OWNER"), 0); err != nil {
		t.Fatal(err)
	}
	// Two exact-identity domains whose names fold to the same case-insensitive
	// value: this is the ambiguity fixture Ruling R6 requires. Both remain
	// independently resolvable by exact ObjectID; an unquoted name matching
	// neither exactly must be reported as ambiguous.
	for _, name := range []string{"Ambig_Domain", "AMBIG_DOMAIN"} {
		if _, err := db.Exec(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$FIELD_LENGTH","RDB$SYSTEM_FLAG") VALUES (?,?,?,?)`,
			name, 8, 4, 0); err != nil {
			t.Fatal(err)
		}
	}
}
