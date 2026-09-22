package handler

import (
	"database/sql"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

// definitionCatalog is the shared catalog fixture: one procedure with a
// verbatim body, one view with a verbatim body, one trigger whose Event is ""
// (the normal case until the driver-side accessor spec lands), and one
// procedure whose Source is invalid so the "nothing to write" path has a
// fixture.
func definitionCatalog() *database.DBCache {
	return &database.DBCache{
		Catalog: &database.CatalogCache{
			Procedures: map[string]*database.ProcedureDesc{
				"MYPROC": {
					Name:   "MYPROC",
					Source: sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
				},
				"NOSOURCE": {
					Name:   "NOSOURCE",
					Source: sql.NullString{},
				},
			},
			Views: map[string]*database.ViewDesc{
				"MYVIEW": {
					Name:       "MYVIEW",
					ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
				},
			},
			Triggers: map[string]*database.TriggerDesc{
				"MYTRIGGER": {
					Name:   "MYTRIGGER",
					Event:  "",
					Source: sql.NullString{String: "BEGIN\n  NEW.ID = 1;\nEND", Valid: true},
				},
			},
		},
	}
}

func definitionParamsAt(character int) lsp.DefinitionParams {
	return lsp.DefinitionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: character},
		},
	}
}

func TestResolveSnapshotTarget(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		character  int
		driver     dialect.DatabaseDriver
		dbCache    *database.DBCache
		wantOK     bool
		wantKind   database.ObjectKind
		wantName   string
		wantSource string
	}{
		{
			name:       "procedure",
			text:       "execute procedure myproc",
			character:  20,
			driver:     dialect.DatabaseDriverInterBase,
			dbCache:    definitionCatalog(),
			wantOK:     true,
			wantKind:   database.ObjectKindProcedure,
			wantName:   "MYPROC",
			wantSource: "BEGIN\n  SUSPEND;\nEND",
		},
		{
			name:       "view",
			text:       "select * from myview",
			character:  16,
			driver:     dialect.DatabaseDriverInterBase,
			dbCache:    definitionCatalog(),
			wantOK:     true,
			wantKind:   database.ObjectKindView,
			wantName:   "MYVIEW",
			wantSource: "SELECT ID FROM CITY",
		},
		{
			name:       "trigger reached by the identifier under the cursor",
			text:       "select mytrigger",
			character:  10,
			driver:     dialect.DatabaseDriverInterBase,
			dbCache:    definitionCatalog(),
			wantOK:     true,
			wantKind:   database.ObjectKindTrigger,
			wantName:   "MYTRIGGER",
			wantSource: "BEGIN\n  NEW.ID = 1;\nEND",
		},
		{
			name:      "unknown identifier",
			text:      "select * from nosuchthing",
			character: 16,
			driver:    dialect.DatabaseDriverInterBase,
			dbCache:   definitionCatalog(),
			wantOK:    false,
		},
		{
			name:      "no catalog yet",
			text:      "execute procedure myproc",
			character: 20,
			driver:    dialect.DatabaseDriverInterBase,
			dbCache:   &database.DBCache{},
			wantOK:    false,
		},
		{
			name:      "not interbase",
			text:      "execute procedure myproc",
			character: 20,
			driver:    dialect.DatabaseDriverPostgreSQL,
			dbCache:   definitionCatalog(),
			wantOK:    false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveSnapshotTarget(tt.text, definitionParamsAt(tt.character), tt.dbCache, tt.driver)
			if ok != tt.wantOK {
				t.Fatalf("resolveSnapshotTarget ok = %v, want %v (target=%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.kind, tt.wantKind)
			}
			// The catalog spelling, not the user's: it is what the banner and
			// the file name use.
			if got.name != tt.wantName {
				t.Errorf("name = %q, want %q", got.name, tt.wantName)
			}
			if got.source.String != tt.wantSource {
				t.Errorf("source = %q, want %q", got.source.String, tt.wantSource)
			}
		})
	}
}

func TestResolveSnapshotTargetIsCaseInsensitiveWithoutUpperCasingAtTheCallSite(t *testing.T) {
	// The accessors normalise the name they are given. This test fails if the
	// call site starts upper-casing, because then a mixed-case identifier would
	// still work and a broken accessor would go unnoticed — and it fails if the
	// accessors are exact-match, which is the D11 risk the contract flags.
	got, ok := resolveSnapshotTarget("execute procedure MyProc", definitionParamsAt(20), definitionCatalog(), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("a mixed-case identifier did not resolve; DBCache accessors must normalise the name they are given")
	}
	if got.name != "MYPROC" {
		t.Errorf("name = %q, want %q", got.name, "MYPROC")
	}
}

// definitionCatalogWithCollisions gives every family a name the others also
// use, so a resolution order that fell out of map iteration would be flaky
// across runs instead of deterministic. "DUP" exists as a procedure, a view
// and a trigger; "VIEWTRIG" exists as a view and a trigger only, with no
// procedure present, so it pins the second half of the chain independently of
// the first.
func definitionCatalogWithCollisions() *database.DBCache {
	return &database.DBCache{
		Catalog: &database.CatalogCache{
			Procedures: map[string]*database.ProcedureDesc{
				"DUP": {
					Name:   "DUP",
					Source: sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
				},
				"MY$PROC": {
					Name:   "MY$PROC",
					Source: sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
				},
			},
			Views: map[string]*database.ViewDesc{
				"DUP": {
					Name:       "DUP",
					ViewSource: sql.NullString{String: "SELECT 1 FROM DUP_VIEW", Valid: true},
				},
				"VIEWTRIG": {
					Name:       "VIEWTRIG",
					ViewSource: sql.NullString{String: "SELECT 1 FROM VIEWTRIG_VIEW", Valid: true},
				},
			},
			Triggers: map[string]*database.TriggerDesc{
				"DUP": {
					Name:   "DUP",
					Source: sql.NullString{String: "BEGIN\n  NEW.ID = 1;\nEND", Valid: true},
				},
				"VIEWTRIG": {
					Name:   "VIEWTRIG",
					Source: sql.NullString{String: "BEGIN\n  NEW.ID = 2;\nEND", Valid: true},
				},
			},
		},
	}
}

// TestResolveSnapshotTargetPrecedenceProcedureBeatsViewAndTrigger pins the
// resolution order for a name that exists in all three families sqls
// resolves through: Procedure wins over both View and Trigger. Run with
// -count=10 (or -race, which randomises map iteration order per run) to see
// this fail immediately if the implementation is ever rewritten to iterate
// the three maps instead of checking them in a fixed sequence.
func TestResolveSnapshotTargetPrecedenceProcedureBeatsViewAndTrigger(t *testing.T) {
	got, ok := resolveSnapshotTarget("select dup", definitionParamsAt(8), definitionCatalogWithCollisions(), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("resolveSnapshotTarget did not resolve a name present in all three families")
	}
	if got.kind != database.ObjectKindProcedure {
		t.Errorf("kind = %q, want %q (procedure must win over view and trigger)", got.kind, database.ObjectKindProcedure)
	}
	if got.name != "DUP" {
		t.Errorf("name = %q, want %q", got.name, "DUP")
	}
}

// TestResolveSnapshotTargetPrecedenceViewBeatsTrigger pins the second half of
// the chain: with no procedure present, View wins over Trigger.
func TestResolveSnapshotTargetPrecedenceViewBeatsTrigger(t *testing.T) {
	got, ok := resolveSnapshotTarget("select viewtrig", definitionParamsAt(10), definitionCatalogWithCollisions(), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("resolveSnapshotTarget did not resolve a name present in both view and trigger")
	}
	if got.kind != database.ObjectKindView {
		t.Errorf("kind = %q, want %q (view must win over trigger)", got.kind, database.ObjectKindView)
	}
	if got.name != "VIEWTRIG" {
		t.Errorf("name = %q, want %q", got.name, "VIEWTRIG")
	}
}

// TestResolveSnapshotTargetResolvesANameContainingADollarSign covers InterBase
// catalog names such as RDB$RELATIONS: '$' is an ordinary identifier
// character for the InterBase dialect (dialect.InterBaseDialect.
// IsIdentifierPart), so it lexes as part of one bare token, not as a
// delimiter. Lower-cased in the SQL text to also confirm this combines
// correctly with the accessors' case folding.
func TestResolveSnapshotTargetResolvesANameContainingADollarSign(t *testing.T) {
	got, ok := resolveSnapshotTarget("execute procedure my$proc", definitionParamsAt(22), definitionCatalogWithCollisions(), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("resolveSnapshotTarget did not resolve a name containing '$'")
	}
	if got.kind != database.ObjectKindProcedure || got.name != "MY$PROC" {
		t.Errorf("got kind=%q name=%q, want kind=%q name=%q", got.kind, got.name, database.ObjectKindProcedure, "MY$PROC")
	}
}

// TestResolveSnapshotTargetStripsQuotesFromADelimitedIdentifier is the
// cache/ObjectDDL seam in miniature. A delimited identifier's ast.Node.
// String() includes the surrounding quotes (token.SQLWord.String()), but
// CatalogCache's map keys never do — they come from catalogCacheKey applied
// to the descriptor's own Name field, which the catalog reports without quote
// decoration. Resolving must strip the quotes (ast.Identifier.NoQuoteString,
// the same accessor hover.go and parseutil already standardise on) before
// asking the cache, or a delimited identifier could never resolve at all.
// The mixed case inside the quotes doubles as confirmation that the returned
// name is the catalog's own spelling, not the user's.
func TestResolveSnapshotTargetStripsQuotesFromADelimitedIdentifier(t *testing.T) {
	got, ok := resolveSnapshotTarget(`execute procedure "MyProc"`, definitionParamsAt(20), definitionCatalog(), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal(`a delimited identifier ("MyProc") did not resolve; quotes must be stripped before the cache lookup`)
	}
	if got.kind != database.ObjectKindProcedure {
		t.Errorf("kind = %q, want %q", got.kind, database.ObjectKindProcedure)
	}
	if got.name != "MYPROC" {
		t.Errorf(`name = %q, want %q (the catalog's stored spelling, not "MyProc")`, got.name, "MYPROC")
	}
}

// TestResolveSnapshotTargetDelimitedIdentifierWithNoCatalogMatchFailsCleanly
// is the "predictably fail" half of the delimited-identifier contract: a
// delimited name containing a space that matches nothing in the catalog must
// come back as a clean miss, not a panic and not a false positive from
// comparing the quoted form.
func TestResolveSnapshotTargetDelimitedIdentifierWithNoCatalogMatchFailsCleanly(t *testing.T) {
	_, ok := resolveSnapshotTarget(`select "No Such Object"`, definitionParamsAt(10), definitionCatalog(), dialect.DatabaseDriverInterBase)
	if ok {
		t.Fatal(`resolveSnapshotTarget resolved a delimited identifier that names no catalog object`)
	}
}

// TestResolveSnapshotTargetQualifiedIdentifierResolvesTheChildIgnoringTheSchema
// covers "schema.object" member identifiers. InterBase has no schema
// namespace (ViewDesc.Schema etc. are always "" for InterBase), so the
// predictable behavior is: the cursor on the child resolves exactly as the
// bare name would, and the qualifier text is never consulted. The relation is
// placed in a FROM clause deliberately: parseutil.ExtractTable returns empty
// for a bare "select myproc." with no FROM, which would let a member-
// identifier test pass vacuously without the qualifier ever being parsed as
// one. This uses a real member identifier (parser/parser.go parseMemberIdentifier
// builds an *ast.MemberIdentifier with distinct Parent/Child *ast.Identifier
// nodes) confirmed by direct inspection during development, not assumed.
func TestResolveSnapshotTargetQualifiedIdentifierResolvesTheChildIgnoringTheSchema(t *testing.T) {
	got, ok := resolveSnapshotTarget("select * from someschema.MYVIEW", definitionParamsAt(30), definitionCatalog(), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("resolveSnapshotTarget did not resolve the child of a qualified identifier")
	}
	if got.kind != database.ObjectKindView || got.name != "MYVIEW" {
		t.Errorf("got kind=%q name=%q, want kind=%q name=%q", got.kind, got.name, database.ObjectKindView, "MYVIEW")
	}
}

// TestResolveSnapshotTargetQualifiedIdentifierSchemaPartFailsPredictably is the
// other half: the cursor on the qualifier itself looks up "someschema", which
// is not a catalog object, and must fail cleanly rather than resolve to
// something unrelated.
func TestResolveSnapshotTargetQualifiedIdentifierSchemaPartFailsPredictably(t *testing.T) {
	_, ok := resolveSnapshotTarget("select * from someschema.MYVIEW", definitionParamsAt(18), definitionCatalog(), dialect.DatabaseDriverInterBase)
	if ok {
		t.Fatal("resolveSnapshotTarget resolved a schema qualifier as if it were a catalog object")
	}
}

// TestResolveSnapshotTargetNilDBCacheDoesNotPanic exercises the exact seam
// HasCatalog exists to guard: DBCache.HasCatalog is nil-receiver-safe
// (dc != nil && dc.Catalog != nil), and resolveSnapshotTarget must rely on
// that rather than dereferencing dbCache itself before the gate.
func TestResolveSnapshotTargetNilDBCacheDoesNotPanic(t *testing.T) {
	got, ok := resolveSnapshotTarget("execute procedure myproc", definitionParamsAt(20), nil, dialect.DatabaseDriverInterBase)
	if ok {
		t.Fatalf("resolveSnapshotTarget resolved against a nil *DBCache: %+v", got)
	}
}
