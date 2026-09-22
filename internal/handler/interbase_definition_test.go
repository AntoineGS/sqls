package handler

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

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

// unsupportedDDLError is a local stand-in for the driver's structured
// unsupported-DDL error. It satisfies errors.Is against
// database.ErrUnsupportedDDL and exposes the detail through the same
// unexported-interface shape database.UnsupportedDDLDetail matches on, so no
// error string is ever compared.
type unsupportedDDLError struct {
	object  string
	name    string
	feature string
}

func (e *unsupportedDDLError) Error() string {
	return "stub: DDL is unavailable for this object"
}

func (e *unsupportedDDLError) Unwrap() error { return database.ErrUnsupportedDDL }

func (e *unsupportedDDLError) UnsupportedDDLDetail() (string, string, string) {
	return e.object, e.name, e.feature
}

func testSnapshotTarget(kind database.ObjectKind, name, source string) snapshotTarget {
	target := snapshotTarget{kind: kind, name: name}
	if source != "" {
		target.source = sql.NullString{String: source, Valid: true}
	}
	return target
}

func TestSnapshotBodyForSuccessUsesTheGeneratedDDL(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END")
	body, note, ok := snapshotBodyFor(`CREATE PROCEDURE "MYPROC" AS BEGIN END`, nil, target)
	if !ok {
		t.Fatal("snapshotBodyFor ok = false, want true")
	}
	if body != `CREATE PROCEDURE "MYPROC" AS BEGIN END` {
		t.Errorf("body = %q, want the generated DDL", body)
	}
	if note != "" {
		t.Errorf("note = %q, want no note on success", note)
	}
}

func TestSnapshotBodyForUnsupportedDDLUsesTheVerbatimSource(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN\n  SUSPEND;\nEND")
	err := &unsupportedDDLError{
		object:  "procedure",
		name:    "MYPROC",
		feature: `parameter "IN_AMOUNT" nullability is unknown`,
	}

	body, note, ok := snapshotBodyFor("", err, target)
	if !ok {
		t.Fatal("snapshotBodyFor ok = false, want true")
	}
	if body != "BEGIN\n  SUSPEND;\nEND" {
		t.Errorf("body = %q, want the verbatim catalog source", body)
	}
	if strings.Contains(body, "CREATE") {
		t.Error("body contains a synthesized CREATE header; a declaration must never be fabricated")
	}
	for _, want := range []string{"procedure", "MYPROC", `parameter "IN_AMOUNT" nullability is unknown`} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to name %q", note, want)
		}
	}
}

func TestSnapshotBodyForUnsupportedDDLWithoutDetailDegrades(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindView, "MYVIEW", "SELECT ID FROM CITY")

	body, note, ok := snapshotBodyFor("", database.ErrUnsupportedDDL, target)
	if !ok {
		t.Fatal("snapshotBodyFor ok = false, want true")
	}
	if body != "SELECT ID FROM CITY" {
		t.Errorf("body = %q, want the verbatim view source", body)
	}
	if note == "" {
		t.Error("note is empty; the reader must be told the body is not executable DDL")
	}
	if strings.Contains(note, "stub:") {
		t.Errorf("note = %q, want no driver message text", note)
	}
}

func TestSnapshotBodyForWritesNothing(t *testing.T) {
	cases := []struct {
		name   string
		target snapshotTarget
		err    error
	}{
		{
			name:   "object not found",
			target: testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END"),
			err:    database.ErrObjectNotFound,
		},
		{
			name:   "unsupported DDL and no source",
			target: testSnapshotTarget(database.ObjectKindProcedure, "NOSOURCE", ""),
			err:    &unsupportedDDLError{object: "procedure", name: "NOSOURCE", feature: "nullability is unknown"},
		},
		{
			name:   "any other error",
			target: testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END"),
			err:    errors.New("connection reset"),
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			body, note, ok := snapshotBodyFor("", tt.err, tt.target)
			if ok {
				t.Fatalf("snapshotBodyFor ok = true, want false (body=%q note=%q)", body, note)
			}
		})
	}
}

func TestRenderSnapshotBanner(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END")
	sc := testSnapshotContext()
	now := time.Date(2026, 9, 19, 10, 4, 11, 0, time.UTC)

	content, bannerLines := renderSnapshot(target, sc, now, "BEGIN\n  SUSPEND;\nEND", "")

	want := `-- sqls: read-only snapshot of InterBase PROCEDURE "MYPROC"
-- connection: local_ib    generated: 2026-09-19T10:04:11Z
-- Editing this file does not change the database.
BEGIN
  SUSPEND;
END
`
	if content != want {
		t.Errorf("content =\n%q\nwant\n%q", content, want)
	}
	if bannerLines != 3 {
		t.Errorf("bannerLines = %d, want 3", bannerLines)
	}
	if strings.Contains(content, sc.identity) {
		t.Error("the banner leaks the connection identity")
	}
}

func TestRenderSnapshotKeepsTheNoteInsideComments(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END")
	// A detail string that contains a newline must not break out of the comment
	// and turn into executable-looking text.
	note := "Executable DDL could not be reproduced: procedure\nsomething else entirely"

	content, bannerLines := renderSnapshot(target, testSnapshotContext(), time.Unix(0, 0).UTC(), "BEGIN END", note)

	lines := strings.Split(content, "\n")
	if bannerLines != 5 {
		t.Fatalf("bannerLines = %d, want 5 (3 banner + 2 note lines)", bannerLines)
	}
	for i := 0; i < bannerLines; i++ {
		if !strings.HasPrefix(lines[i], "--") {
			t.Errorf("line %d = %q, want a comment", i, lines[i])
		}
	}
}

func TestRenderSnapshotKeepsTheBannerThreeLines(t *testing.T) {
	// The same rule applied to the connection label. It is the user's own
	// config alias rather than attacker-controlled data, so this is a
	// consistency guard: without it a multi-line alias would both break out of
	// the comment and make bannerLines wrong, pushing snapshotRange into the
	// banner and returning a range over the wrong text.
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END")
	sc := testSnapshotContext()
	sc.label = "local\nDROP TABLE USERS; --"

	content, bannerLines := renderSnapshot(target, sc, time.Unix(0, 0).UTC(), "BEGIN END", "")

	if bannerLines != 3 {
		t.Fatalf("bannerLines = %d, want 3", bannerLines)
	}
	lines := strings.Split(content, "\n")
	for i := 0; i < bannerLines; i++ {
		if !strings.HasPrefix(lines[i], "--") {
			t.Errorf("line %d = %q, want a comment", i, lines[i])
		}
	}
	if !strings.Contains(lines[1], "local DROP TABLE USERS; --") {
		t.Errorf("banner line = %q, want the label collapsed onto one line", lines[1])
	}
}

func TestSnapshotRange(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		bannerLines int
		object      string
		want        lsp.Range
	}{
		{
			name:        "first occurrence after the banner",
			content:     "-- sqls: MYPROC\n-- b\n-- c\nCREATE PROCEDURE \"MYPROC\" AS\nBEGIN END\n",
			bannerLines: 3,
			object:      "MYPROC",
			want: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 18},
				End:   lsp.Position{Line: 3, Character: 18},
			},
		},
		{
			name:        "name absent from the body",
			content:     "-- a\n-- b\n-- c\nBEGIN\n  SUSPEND;\nEND\n",
			bannerLines: 3,
			object:      "MYPROC",
			want:        lsp.Range{},
		},
		{
			name:        "character offset is counted in UTF-16 units",
			content:     "-- a\n-- b\n-- c\n/* 𝕏𝕏 */ MYPROC\n",
			bannerLines: 3,
			object:      "MYPROC",
			want: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 11},
				End:   lsp.Position{Line: 3, Character: 11},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := snapshotRange(tt.content, tt.bannerLines, tt.object); got != tt.want {
				t.Errorf("snapshotRange = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSnapshotRangeSkipsTheBannersOwnNameMention proves the search starts
// after the banner rather than at line 0. The banner's own first line always
// names the object in quotes ("-- sqls: ... InterBase PROCEDURE \"MYPROC\""),
// so an off-by-one that started the loop one line too early would find a
// false match there instead of correctly reporting that the body never
// mentions the name.
func TestSnapshotRangeSkipsTheBannersOwnNameMention(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "")
	content, bannerLines := renderSnapshot(target, testSnapshotContext(), time.Unix(0, 0).UTC(), "BEGIN\n  SUSPEND;\nEND", "")

	if !strings.Contains(strings.Split(content, "\n")[0], "MYPROC") {
		t.Fatal("test is vacuous: the banner's first line does not mention the object name")
	}

	got := snapshotRange(content, bannerLines, "MYPROC")
	if got != (lsp.Range{}) {
		t.Errorf("snapshotRange = %+v, want the zero range (the body never mentions MYPROC; a non-zero result means the search read into the banner)", got)
	}
}

// TestRenderSnapshotThenSnapshotRangeAgreeOnTheSameBytes feeds renderSnapshot's
// own return values into snapshotRange, rather than a hand-authored content
// string, so the proof is against the exact bytes that would be written to
// disk. The table covers the cases at the banner/body boundary that an
// off-by-one or a bytes/offset mismatch would get wrong: the name on the
// body's very first line (the loop must start at bannerLines, not
// bannerLines+1), CRLF line endings (a trailing \r kept on the split line
// must not shift the offset of a match that precedes it), a body that already
// ends in "\n" and one that does not (both must produce the same searchable
// shape), an empty body (no panic, a clean zero range), and a note combined
// with a first-body-line match (the note's line count must compose correctly
// into bannerLines).
func TestRenderSnapshotThenSnapshotRangeAgreeOnTheSameBytes(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	cases := []struct {
		name string
		body string
		note string
		want lsp.Range
	}{
		{
			name: "name on the body's first line",
			body: "MYPROC AS BEGIN END",
			want: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 0},
				End:   lsp.Position{Line: 3, Character: 0},
			},
		},
		{
			name: "CRLF line endings",
			body: "SELECT 1 FROM DUMMY\r\nWHERE MYPROC = 1\r\n",
			want: lsp.Range{
				Start: lsp.Position{Line: 4, Character: 6},
				End:   lsp.Position{Line: 4, Character: 6},
			},
		},
		{
			name: "trailing newline already present",
			body: "CREATE PROCEDURE \"MYPROC\" AS BEGIN END\n",
			want: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 18},
				End:   lsp.Position{Line: 3, Character: 18},
			},
		},
		{
			name: "trailing newline absent",
			body: "CREATE PROCEDURE \"MYPROC\" AS BEGIN END",
			want: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 18},
				End:   lsp.Position{Line: 3, Character: 18},
			},
		},
		{
			name: "empty body",
			body: "",
			want: lsp.Range{},
		},
		{
			name: "note shifts the banner, then the name is on the body's first line",
			body: "MYPROC",
			note: "line one\nline two",
			want: lsp.Range{
				Start: lsp.Position{Line: 5, Character: 0},
				End:   lsp.Position{Line: 5, Character: 0},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "")
			content, bannerLines := renderSnapshot(target, testSnapshotContext(), now, tt.body, tt.note)

			if got := snapshotRange(content, bannerLines, target.name); got != tt.want {
				t.Errorf("snapshotRange = %+v, want %+v (content=%q bannerLines=%d)", got, tt.want, content, bannerLines)
			}
		})
	}
}

// TestRenderSnapshotNormalizesExactlyOneTrailingNewline proves a body that
// already ends in "\n" and one that does not produce byte-identical content:
// the append-if-missing branch must neither double a newline that is already
// there nor leave the body unterminated.
func TestRenderSnapshotNormalizesExactlyOneTrailingNewline(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "")
	now := time.Unix(0, 0).UTC()

	withNewline, bannerLines1 := renderSnapshot(target, testSnapshotContext(), now, "CREATE PROCEDURE \"MYPROC\" AS BEGIN END\n", "")
	withoutNewline, bannerLines2 := renderSnapshot(target, testSnapshotContext(), now, "CREATE PROCEDURE \"MYPROC\" AS BEGIN END", "")

	if withNewline != withoutNewline {
		t.Errorf("content differs depending on whether the body already had a trailing newline:\nwith:    %q\nwithout: %q", withNewline, withoutNewline)
	}
	if bannerLines1 != bannerLines2 {
		t.Errorf("bannerLines differ: %d vs %d", bannerLines1, bannerLines2)
	}
	if strings.HasSuffix(withNewline, "\n\n") {
		t.Error("content ends with a doubled newline")
	}
}
