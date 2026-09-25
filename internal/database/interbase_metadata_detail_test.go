package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// required bulk category is still loading must join/share that in-flight
// load, not trigger a second, independent read of the same data. Unlike a
// pure timing race (which could pass for the wrong reason if the bulk job
// happens to finish before the detail goroutine starts), this test uses the
// fixture's gate/entered mechanism to PROVE the columns query has genuinely
// started -- and is still blocked -- before the detail request is issued,
// then asserts zero additional prepares happen while it stays blocked.
func TestInterBaseMetadataProviderDetailJoinsInFlightBulkLoad(t *testing.T) {
	const columnsQueryMatch = "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY rf.RDB$RELATION_NAME"

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var columnsMatches atomic.Int64
	fault := &interBaseFixtureFault{match: columnsQueryMatch, stage: "gate", gate: release, entered: entered, matches: &columnsMatches}
	db := openInterBaseSchemaFixtureWithFault(t, fault)
	insertInterBaseDetailRegressionFixtures(t, db)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3, MaxDetailBytes: 64 << 20})
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	provider := &interBaseTestProvider{db: repo}
	factory := func(context.Context) (metadata.Provider, error) { return provider, nil }
	lease, err := registry.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "test://" + t.Name()}, metadata.Scope{Principal: "TESTER"}, factory)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(lease.Release)

	// Block until the bulk columns query has genuinely started (and is now
	// parked on the gate) before issuing any detail request.
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		t.Fatalf("bulk columns query did not gate: %v", ctx.Err())
	}

	// Confirm the gated query genuinely ran exactly once so far (the bulk
	// columns job's own attempt), before the detail request enters.
	if got := columnsMatches.Load(); got != 1 {
		close(release)
		t.Fatalf("columns query match count before Detail() = %d, want 1 (the bulk job's own attempt)", got)
	}

	// Fire the detail request while the columns bulk query is still
	// blocked mid-flight. It Requires Relations+ColumnsCurrent, so it must
	// join the same in-flight columns load rather than starting a second,
	// independent one.
	req := metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "CUSTOMER"}, Facet: "columns", Consistency: metadata.Committed}
	detailDone := make(chan struct{})
	var detail metadata.Detail
	var detailErr error
	go func() {
		defer close(detailDone)
		detail, detailErr = lease.Detail(ctx, req)
	}()

	// While the gate stays closed, the detail request must be waiting on
	// the SAME in-flight bulk category, not have triggered a second,
	// independent columns query: the match count must stay at 1.
	select {
	case <-detailDone:
		close(release)
		t.Fatalf("Detail() returned before the gated bulk columns query was released: err=%v", detailErr)
	case <-time.After(50 * time.Millisecond):
	}
	if got := columnsMatches.Load(); got != 1 {
		close(release)
		t.Fatalf("columns query match count while gated = %d, want 1 (Detail must join the in-flight load, not start a second query)", got)
	}

	close(release)

	select {
	case <-detailDone:
	case <-ctx.Done():
		t.Fatalf("Detail() did not complete after releasing the gate: %v", ctx.Err())
	}
	if detailErr != nil {
		t.Fatalf("Detail() error = %v", detailErr)
	}
	if len(detail.Columns) == 0 {
		t.Fatal("Detail() returned no columns for CUSTOMER")
	}

	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	// Exactly one columns query ran in total: the gated bulk job's own
	// attempt. The detail request never triggered a second one, whether
	// it arrived before or after the gate opened.
	if got := columnsMatches.Load(); got != 1 {
		t.Fatalf("columns query match count after settlement = %d, want 1 (exactly the bulk job's own read)", got)
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

	// This asserts byte-for-byte STORAGE/RETRIEVAL preservation only: the
	// SQLite-backed fixture stores and returns the Go string's bytes
	// unchanged, so it never passes through the driver's real NONE-charset/
	// CP1252 decoding (that decode only exists in the cgo native driver,
	// exercised over a live attachment). Real CP1252 decoding coverage
	// (raw catalog bytes -> correctly decoded UTF-8, e.g. the "CRÉATION"
	// assertion) lives in
	// internal/handler/interbase_catalog_text_live_test.go's
	// TestInterBaseLiveLegacyCatalogSingleton, which is build-tagged
	// interbase && cgo && linux && amd64 and requires
	// SQLS_LEGACY_CATALOG_CONFIG/CHARSET/DOCUMENT to run against a real
	// database; it is a live-only test with no in-process mock equivalent
	// possible for the native decode step itself.
	t.Run("non-ASCII source bytes preserved verbatim through the retained-fact path (storage/retrieval only, not real CP1252 decoding)", func(t *testing.T) {
		procedure := catalog.ObjectID{Kind: ObjectKindProcedure, Name: "CP1252_SOURCE_PROC"}
		got, err := lease.Detail(ctx, metadata.Request{Object: procedure, Facet: "source", Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(CP1252-range-byte procedure source) error = %v", err)
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

	t.Run("table ddl is bounded and function ddl remains unsupported", func(t *testing.T) {
		count := interBaseFixtureCountPrepares(t)
		tableDDL, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "PARENT"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		if err != nil {
			t.Fatalf("Detail(table ddl) error = %v", err)
		}
		if !strings.Contains(tableDDL.Text, `CREATE TABLE "PARENT"`) || !strings.Contains(tableDDL.Text, `CONSTRAINT "PK_PARENT" PRIMARY KEY`) {
			t.Fatalf("Detail(table ddl).Text = %q, want rendered PARENT DDL", tableDDL.Text)
		}
		if tableDDL.Provenance != "interbase:table-ddl" {
			t.Fatalf("Detail(table ddl).Provenance = %q, want interbase:table-ddl", tableDDL.Provenance)
		}
		if _, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindFunction, Name: "F_LTRIM"}, Facet: "ddl", Consistency: metadata.Committed}); err == nil {
			t.Fatalf("Detail(function ddl) error = nil, want unsupported")
		}
		if got := count(); got == 0 {
			t.Fatalf("queries for table ddl = %d, want bounded provider read", got)
		}
	})
}

func TestInterBaseTableDDLCoalescesAcrossLeases(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 3}
	ctx := context.Background()
	registry := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3, MaxDetailBytes: 64 << 20})
	t.Cleanup(func() { _ = registry.Close(ctx) })
	factory := func(context.Context) (metadata.Provider, error) { return &interBaseTestProvider{db: repo}, nil }
	key := metadata.Key{Engine: "interbase", URL: "test://" + t.Name()}
	scope := metadata.Scope{Principal: "TESTER"}
	firstLease, err := registry.Acquire(ctx, key, scope, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(firstLease.Release)
	secondLease, err := registry.Acquire(ctx, key, scope, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secondLease.Release)
	if err := metadatatest.WaitReady(ctx, firstLease); err != nil {
		t.Fatal(err)
	}

	count := interBaseFixtureCountPrepares(t)
	object := catalog.ObjectID{Kind: ObjectKindTable, Name: "PARENT"}
	hover, err := firstLease.Detail(ctx, metadata.Request{Object: object, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
	if err != nil {
		t.Fatalf("first lease table DDL: %v", err)
	}
	reads := count()
	if reads != 11 {
		t.Fatalf("first table DDL prepared %d statements, want the bounded schema.Table read of 11", reads)
	}
	t.Logf("first table DDL provider read prepared %d statements; the equivalent second lease prepared zero additional statements", reads)
	definition, err := secondLease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "parent"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
	if err != nil {
		t.Fatalf("second lease table DDL: %v", err)
	}
	if got := count(); got != reads {
		t.Fatalf("fixture query count after second equivalent DDL = %d, first = %d; expected one coalesced Load", got, reads)
	}
	if definition.Text != hover.Text || definition.Provenance != "interbase:table-ddl" {
		t.Fatalf("second detail = (%q, %q), want same DDL and table provenance", definition.Text, definition.Provenance)
	}
	if hover.Object.Name != "PARENT" || definition.Object.Name != "PARENT" {
		t.Fatalf("canonical Detail.Object names = %q, %q; want PARENT for both spellings", hover.Object.Name, definition.Object.Name)
	}
}

func TestInterBaseTableDDLEquivalentSpellingJoinsInFlightLoad(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	var matches atomic.Int64
	entered := make(chan struct{}, 1)
	fault := &interBaseFixtureFault{match: "__not_gated_until_ready__", stage: "gate", gate: release, entered: entered, matches: &matches}
	db := openInterBaseSchemaFixtureWithFault(t, fault)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 3}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3, MaxDetailBytes: 64 << 20})
	t.Cleanup(func() { closeRelease(); _ = registry.Close(context.Background()) })
	factory := func(context.Context) (metadata.Provider, error) { return &interBaseTestProvider{db: repo}, nil }
	key := metadata.Key{Engine: "interbase", URL: "test://" + t.Name()}
	scope := metadata.Scope{Principal: "TESTER"}
	a, err := registry.Acquire(ctx, key, scope, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Release)
	b, err := registry.Acquire(ctx, key, scope, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Release)
	if err := metadatatest.WaitReady(ctx, a); err != nil {
		t.Fatal(err)
	}
	fault.match = "RDB$RELATION_CONSTRAINTS"

	firstDone := make(chan metadata.Detail, 1)
	firstErr := make(chan error, 1)
	go func() {
		value, err := a.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "PARENT"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		firstDone <- value
		firstErr <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatalf("first table DDL did not enter gated provider query: %v", ctx.Err())
	}
	if got := matches.Load(); got != 1 {
		closeRelease()
		t.Fatalf("gated query matches before alias request = %d, want 1", got)
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan metadata.Detail, 1)
	secondErr := make(chan error, 1)
	go func() {
		close(secondStarted)
		value, err := b.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "parent"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
		secondDone <- value
		secondErr <- err
	}()
	<-secondStarted
	select {
	case <-secondDone:
		closeRelease()
		t.Fatal("equivalent-spelling Detail returned while the shared table DDL load remained gated")
	case <-time.After(50 * time.Millisecond):
	}
	if got := matches.Load(); got != 1 {
		closeRelease()
		t.Fatalf("equivalent-spelling request started %d table-detail queries while first was gated, want one", got)
	}
	closeRelease()
	first, second := <-firstDone, <-secondDone
	if err := <-firstErr; err != nil {
		t.Fatalf("first table DDL: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("equivalent-spelling table DDL: %v", err)
	}
	if first.Object.Name != "PARENT" || second.Object.Name != "PARENT" || first.Text != second.Text {
		t.Fatalf("coalesced details not canonical/equal: first=%+v second=%+v", first.Object, second.Object)
	}
	if got := matches.Load(); got != 3 {
		t.Fatalf("provider table-detail matched query count = %d, want three schema.Table statements in one Load", got)
	}
}

func TestInterBaseTableDDLUsesExactNameAndRejectsAmbiguity(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	insertInterBaseMixedCaseTableFixtures(t, db)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 3}
	_, lease := acquireInterBaseTestLease(t, repo)
	ctx := context.Background()
	got, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "Foo"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
	if err != nil {
		t.Fatalf("exact mixed-case Foo DDL: %v", err)
	}
	if !strings.Contains(got.Text, `CREATE TABLE "Foo"`) || strings.Contains(got.Text, `UPPER_FOO_ONLY_COLUMN`) {
		t.Fatalf("Foo DDL = %q, want exact Foo table only", got.Text)
	}
	count := interBaseFixtureCountPrepares(t)
	_, err = lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "foo"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed})
	if !errors.Is(err, catalog.ErrAmbiguous) {
		t.Fatalf("ambiguous folded table DDL error = %v, want catalog.ErrAmbiguous", err)
	}
	if got := count(); got != 0 {
		t.Fatalf("ambiguous table name triggered %d fixture queries, want no provider read", got)
	}
}

func TestInterBaseTableDDLInvalidationDiscardsInFlightResult(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	// Do not gate the initial bulk load: table DDL's later
	// RDB$RELATION_CONSTRAINTS lookup overlaps a similarly named catalog
	// query, so enable the gate only after the shared snapshot is ready.
	fault := &interBaseFixtureFault{match: "__table_ddl_not_gated_yet__", stage: "gate", gate: release, entered: entered}
	db := openInterBaseSchemaFixtureWithFault(t, fault)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 3}
	_, lease := acquireInterBaseTestLease(t, repo)
	fault.match = "RDB$RELATION_CONSTRAINTS"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "PARENT"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed}
	done := make(chan error, 1)
	go func() { _, err := lease.Detail(ctx, request); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		t.Fatalf("table DDL did not enter gated provider read: %v", ctx.Err())
	}
	if err := lease.Invalidate(); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, metadata.ErrStale) {
			t.Fatalf("in-flight DDL error = %v, want metadata.ErrStale", err)
		}
	case <-ctx.Done():
		t.Fatalf("in-flight DDL did not settle after gate release: %v", ctx.Err())
	}
}

func TestInterBaseTableDDLCoverageIncludesSourceDialect(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	ctx := context.Background()
	request := metadata.Request{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "PARENT"}, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed}
	dialect1Plan, err := (&InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 1}).MetadataDetailPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	dialect3Plan, err := (&InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 3}).MetadataDetailPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	cache := &catalog.Cache{SchemaTables: map[string][]string{"": {"PARENT"}}}
	dialect1Coverage, err := dialect1Plan.CoverageForCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	dialect3Coverage, err := dialect3Plan.CoverageForCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	if dialect1Coverage == dialect3Coverage {
		t.Fatalf("source dialect variants share coverage %q; rendered text would be cached under a dialect-agnostic key", dialect1Coverage)
	}
	var rendered []string
	for _, tc := range []struct {
		dialect int
		want    string
	}{{1, "CREATE TABLE PARENT"}, {3, `CREATE TABLE "PARENT"`}} {
		t.Run(fmt.Sprintf("dialect-%d", tc.dialect), func(t *testing.T) {
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: tc.dialect}
			_, lease := acquireInterBaseTestLease(t, repo)
			got, err := lease.Detail(ctx, request)
			if err != nil {
				t.Fatalf("table DDL: %v", err)
			}
			if !strings.Contains(got.Text, tc.want) {
				t.Fatalf("source Dialect %d table DDL = %q, want %q", tc.dialect, got.Text, tc.want)
			}
			rendered = append(rendered, got.Text)
		})
	}
	if len(rendered) != 2 || rendered[0] == rendered[1] {
		t.Fatalf("table DDL source-dialect variants are not distinct: %q", rendered)
	}
}

func TestInterBaseTableDDLWaitsForExactIdentityCategory(t *testing.T) {
	repo := &InterBaseDBRepository{SQLDialect: 3, SourceSQLDialect: 3}
	plan, err := repo.MetadataDetailPlan(metadata.Request{
		Object: catalog.ObjectID{Kind: ObjectKindTable, Name: "PARENT"},
		Facet:  "ddl", Consistency: metadata.Committed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Requires) != 1 || plan.Requires[0] != MetadataRelations {
		t.Fatalf("table DDL Requires = %v, want only MetadataRelations for exact cached-name resolution", plan.Requires)
	}
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
	// get silently truncated or misresolved. This fixture is SQLite-backed
	// and never truncates a stored TEXT value, so this only proves the
	// metadata pipeline carries a long identifier through end to end; it
	// does not exercise the real driver's own identifier-length cast
	// workaround (interBaseBulkIdentifierCastWidth's CAST(... AS
	// VARCHAR(67)) projection), which only runs against a live database.
	longName := "LONG_IDENTIFIER_EXCEEDING_31_BYTES_OK"
	if len(longName) <= 31 {
		t.Fatalf("fixture bug: longName %q is only %d bytes, want >31", longName, len(longName))
	}
	if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`,
		longName, 70002, 0, 0, nil, "BEGIN\n  SELECT 1 FROM RDB$DATABASE;\nEND", nil, interBaseFixed("OWNER"), 0); err != nil {
		t.Fatal(err)
	}
	// Non-ASCII (CP1252-range) source bytes: this only exercises the
	// STORAGE/RETRIEVAL path (this SQLite-backed fixture stores and
	// returns the Go string's bytes unchanged) -- it never passes through
	// the real driver's NONE-charset/CP1252 decoding, which only exists in
	// the cgo native driver over a live attachment. See the "non-ASCII
	// source bytes preserved verbatim" subtest below for the full
	// disclosure and pointer to the live decoding coverage.
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
