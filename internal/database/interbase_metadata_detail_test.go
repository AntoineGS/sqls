package database

import (
	"context"
	"database/sql"
	"strings"
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
	}

	cases := []struct {
		name          string
		first, second string
		wantRepeat    int
	}{
		{"explorer then hover", "columns", "description", 0},
		{"hover then explorer", "description", "columns", 0},
		{"source then definition", "source", "source", 0},
		{"loaded procedure then ddl", "columns" /* stands in for "procedure-category": already settled */, "ddl", 0},
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
		if _, err := lease.Detail(ctx, metadata.Request{Object: ambiguous, Facet: "ddl", Dialect: 3, Consistency: metadata.Committed}); err == nil {
			t.Fatal("Detail(ambiguous unquoted domain name) error = nil, want an explicit ambiguity error")
		} else if strings.Contains(err.Error(), "not found") {
			t.Fatalf("Detail(ambiguous unquoted domain name) error = %v, want an ambiguity error, not not-found", err)
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
