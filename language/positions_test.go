package language_test

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
)

func TestUTF16PositionRoundTrip(t *testing.T) {
	text := "-- 😀\r\nselect Café from T"
	want := strings.Index(text, "Café")
	pos, err := language.PositionAt(text, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := language.ByteOffset(text, pos)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("offset %d != %d", got, want)
	}
	if _, err := language.ByteOffset(text, language.Position{Line: 0, UTF16Column: 4}); err == nil {
		t.Fatal("accepted split surrogate")
	}
}

func TestNonBMPWidthIsTwoUTF16Units(t *testing.T) {
	// "😀x": the emoji is a single Unicode code point above U+FFFF, so it
	// occupies UTF16Column 0 AND 1 (a surrogate pair), and 'x' starts at
	// UTF16Column 2. These are direct, non-round-trip assertions: each
	// function is checked against an independently reasoned value, not
	// just against the other.
	text := "😀x"

	pos, err := language.PositionAt(text, 4)
	if err != nil {
		t.Fatal(err)
	}
	if pos != (language.Position{Line: 0, UTF16Column: 2}) {
		t.Fatalf("PositionAt(%q, 4) = %+v, want {Line:0 UTF16Column:2}", text, pos)
	}

	offset, err := language.ByteOffset(text, language.Position{Line: 0, UTF16Column: 2})
	if err != nil {
		t.Fatal(err)
	}
	if offset != 4 {
		t.Fatalf("ByteOffset(%q, {0,2}) = %d, want 4", text, offset)
	}

	multiline := "-- 😀\r\nselect Café from T"
	target := strings.Index(multiline, "Café")
	multilinePos, err := language.PositionAt(multiline, target)
	if err != nil {
		t.Fatal(err)
	}
	if multilinePos != (language.Position{Line: 1, UTF16Column: 7}) {
		t.Fatalf("PositionAt(multiline, %d) = %+v, want {Line:1 UTF16Column:7}", target, multilinePos)
	}
}

func TestPositionAtRejectsOffsetInsideFourByteEncoding(t *testing.T) {
	// "😀" is a 4-byte UTF-8 sequence; offsets 1-3 all land mid-encoding.
	text := "😀x"
	for offset := 1; offset <= 3; offset++ {
		if _, err := language.PositionAt(text, offset); err == nil {
			t.Fatalf("accepted offset %d inside 4-byte encoding", offset)
		}
	}
}

func TestByteOffsetRejectsMalformedPositions(t *testing.T) {
	text := "select 1\nfrom T"
	cases := []struct {
		name string
		pos  language.Position
	}{
		{"negative line", language.Position{Line: -1, UTF16Column: 0}},
		{"negative column", language.Position{Line: 0, UTF16Column: -1}},
		{"line out of range", language.Position{Line: 5, UTF16Column: 0}},
		{"column past end of line", language.Position{Line: 0, UTF16Column: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := language.ByteOffset(text, tc.pos); err == nil {
				t.Fatalf("accepted %+v", tc.pos)
			}
		})
	}
}

func TestByteOffsetAcceptsEndOfLine(t *testing.T) {
	text := "abc\ndef"
	pos := language.Position{Line: 0, UTF16Column: 3}
	offset, err := language.ByteOffset(text, pos)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 3 {
		t.Fatalf("offset = %d, want 3", offset)
	}
}

func TestPositionAtRejectsMalformedOffsets(t *testing.T) {
	text := "a\r\nb"
	cases := []struct {
		name   string
		offset int
	}{
		{"negative offset", -1},
		{"offset past end", len(text) + 1},
		{"offset inside CRLF", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := language.PositionAt(text, tc.offset); err == nil {
				t.Fatalf("accepted offset %d", tc.offset)
			}
		})
	}

	multi := "café"
	eIndex := strings.IndexRune(multi, 'é')
	if _, err := language.PositionAt(multi, eIndex+1); err == nil {
		t.Fatal("accepted offset inside UTF-8 encoding")
	}
}

func TestPositionAtFinalEmptyLine(t *testing.T) {
	text := "select 1\n"
	pos, err := language.PositionAt(text, len(text))
	if err != nil {
		t.Fatal(err)
	}
	if pos != (language.Position{Line: 1, UTF16Column: 0}) {
		t.Fatalf("got %+v", pos)
	}
	back, err := language.ByteOffset(text, pos)
	if err != nil {
		t.Fatal(err)
	}
	if back != len(text) {
		t.Fatalf("offset %d != %d", back, len(text))
	}
}

func TestPositionLFAndCRLFAgree(t *testing.T) {
	lf := "select 1\nfrom T"
	crlf := "select 1\r\nfrom T"
	target := "from T"

	lfPos, err := language.PositionAt(lf, strings.Index(lf, target))
	if err != nil {
		t.Fatal(err)
	}
	crlfPos, err := language.PositionAt(crlf, strings.Index(crlf, target))
	if err != nil {
		t.Fatal(err)
	}
	if lfPos != crlfPos {
		t.Fatalf("LF pos %+v != CRLF pos %+v", lfPos, crlfPos)
	}
	if lfPos != (language.Position{Line: 1, UTF16Column: 0}) {
		t.Fatalf("got %+v", lfPos)
	}
}

func TestPositionCombiningMarkRoundTrip(t *testing.T) {
	// "e" + COMBINING ACUTE ACCENT (U+0301): two separate UTF-16 code
	// units, not one grapheme cluster, matching LSP's own definition of
	// Position.
	text := "select e\u0301 from T"
	target := "from T"
	want := strings.Index(text, target)

	pos, err := language.PositionAt(text, want)
	if err != nil {
		t.Fatal(err)
	}
	// "select " (7) + "e" (1) + the combining mark (1) + " " (1) = 10: the
	// combining mark occupies its own column rather than merging into "e"'s.
	if pos != (language.Position{Line: 0, UTF16Column: 10}) {
		t.Fatalf("got %+v, want {Line:0 UTF16Column:10}", pos)
	}
	got, err := language.ByteOffset(text, pos)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("offset %d != %d", got, want)
	}
}

func baseStampInputs() (language.Stamp, language.Document, language.Context) {
	snapshot := &catalog.MetadataSnapshot{Generation: 3, Revision: 7}
	ctx := language.Context{
		Key:         metadata.Key{Engine: "interbase", URL: "db1"},
		Incarnation: 2,
		Snapshot:    snapshot,
	}
	document := language.Document{URI: "memory:one", Version: 5, Binding: 1}
	stamp := language.Stamp{
		URI:         document.URI,
		Version:     document.Version,
		Binding:     document.Binding,
		Incarnation: ctx.Incarnation,
		Generation:  snapshot.Generation,
		Revision:    snapshot.Revision,
		Key:         ctx.Key,
	}
	return stamp, document, ctx
}

func TestCurrent(t *testing.T) {
	stamp, document, ctx := baseStampInputs()
	if !language.Current(stamp, document, ctx) {
		t.Fatal("expected coherent stamp to be current")
	}

	t.Run("uri", func(t *testing.T) {
		d := document
		d.URI = "memory:two"
		if language.Current(stamp, d, ctx) {
			t.Fatal("expected mismatched URI to be stale")
		}
	})
	t.Run("version", func(t *testing.T) {
		d := document
		d.Version++
		if language.Current(stamp, d, ctx) {
			t.Fatal("expected mismatched version to be stale")
		}
	})
	t.Run("binding", func(t *testing.T) {
		d := document
		d.Binding++
		if language.Current(stamp, d, ctx) {
			t.Fatal("expected mismatched binding to be stale")
		}
	})
	t.Run("key", func(t *testing.T) {
		c := ctx
		c.Key = metadata.Key{Engine: "interbase", URL: "db2"}
		if language.Current(stamp, document, c) {
			t.Fatal("expected mismatched key to be stale")
		}
	})
	t.Run("incarnation", func(t *testing.T) {
		c := ctx
		c.Incarnation++
		if language.Current(stamp, document, c) {
			t.Fatal("expected mismatched incarnation to be stale")
		}
	})
	t.Run("generation", func(t *testing.T) {
		c := ctx
		snap := *c.Snapshot
		snap.Generation++
		c.Snapshot = &snap
		if language.Current(stamp, document, c) {
			t.Fatal("expected mismatched generation to be stale")
		}
	})
	t.Run("revision", func(t *testing.T) {
		c := ctx
		snap := *c.Snapshot
		snap.Revision++
		c.Snapshot = &snap
		if language.Current(stamp, document, c) {
			t.Fatal("expected mismatched revision to be stale")
		}
	})
	t.Run("nil snapshot uses zero generation and revision", func(t *testing.T) {
		zeroStamp := stamp
		zeroStamp.Generation = 0
		zeroStamp.Revision = 0
		c := ctx
		c.Snapshot = nil
		if !language.Current(zeroStamp, document, c) {
			t.Fatal("expected nil snapshot to compare as zero generation/revision")
		}
	})
}
