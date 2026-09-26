package language_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/formatter"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func TestLanguageParserBoundaryUsesUTF16Positions(t *testing.T) {
	cache := &catalog.Cache{
		SchemaTables: map[string][]string{"": {"CITY"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CITY"): {
				{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "ID"}, Type: "INTEGER"},
				{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "NAME"}, Type: "VARCHAR(35)"},
			},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataViews: catalog.MetadataReady},
	}
	service := language.NewService()

	t.Run("completion candidate uses UTF16 prefix", func(t *testing.T) {
		text := "SELECT '😀' FROM CITY WHERE I"
		position := language.Position{Line: 0, UTF16Column: len("SELECT '") + 2 + len("' FROM CITY WHERE I")}
		req := hoverRequest(cache, text)
		got, err := service.Complete(context.Background(), req, position)
		if err != nil {
			t.Fatal(err)
		}
		foundID := false
		for _, item := range got.Value.Items {
			if item.Label == "ID" {
				foundID = true
			}
			if item.Label == "NAME" {
				t.Fatalf("NAME survived filtering for typed prefix I; cursor/replacement prefix was misread: %#v", got.Value.Items)
			}
		}
		if !foundID {
			t.Fatalf("completion candidates = %#v, want ID", got.Value.Items)
		}
	})

	t.Run("signature active argument before CRLF", func(t *testing.T) {
		text := "execute procedure myproc('😀', 45,\r\n  6)"
		position := language.Position{Line: 0, UTF16Column: len("execute procedure myproc('") + 2 + len("', 45")}
		got, err := service.SignatureHelp(context.Background(), signatureRequest(signatureProcedureCache(), text), position)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Value.Signatures) != 1 || got.Value.ActiveParameter != 1 {
			t.Fatalf("signature help = %+v, want one signature with active argument 1", got.Value)
		}
	})

	t.Run("signature active argument on CRLF continuation line", func(t *testing.T) {
		text := "execute procedure myproc('😀',\r\n  45)"
		position := language.Position{Line: 1, UTF16Column: 4}
		got, err := service.SignatureHelp(context.Background(), signatureRequest(signatureProcedureCache(), text), position)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Value.Signatures) != 1 || got.Value.ActiveParameter != 1 {
			t.Fatalf("signature help = %+v, want one signature with active argument 1", got.Value)
		}
	})

	t.Run("hover target and UTF16 range", func(t *testing.T) {
		text := "SELECT '😀', ID FROM CITY\r\nWHERE ID = 1"
		position := language.Position{Line: 0, UTF16Column: 13}
		got, err := service.Hover(context.Background(), hoverRequest(cache, text), position)
		if err != nil {
			t.Fatal(err)
		}
		want := language.Range{Start: language.Position{Line: 0, UTF16Column: 13}, End: language.Position{Line: 0, UTF16Column: 15}}
		if got.Value.Range == nil || *got.Value.Range != want || !strings.Contains(got.Value.Markdown, "INTEGER") {
			t.Fatalf("hover = %+v, range=%v, want ID documentation and range %+v", got.Value, got.Value.Range, want)
		}
	})
}

func TestLanguageRejectsSplitSurrogateAndOutOfRangePositions(t *testing.T) {
	req := hoverRequest(hoverCache(), "SELECT '😀', ID FROM CITY\r\nWHERE ID=1")
	for name, pos := range map[string]language.Position{
		"split surrogate": {Line: 0, UTF16Column: 9},
		"past document":   {Line: 3, UTF16Column: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := language.NewService().Complete(context.Background(), req, pos); err != language.ErrPosition {
				t.Fatalf("Complete error = %v, want ErrPosition", err)
			}
			if _, err := language.NewService().SignatureHelp(context.Background(), signatureRequest(signatureProcedureCache(), req.Document.Text), pos); err != language.ErrPosition {
				t.Fatalf("SignatureHelp error = %v, want ErrPosition", err)
			}
			if _, err := language.NewService().Hover(context.Background(), req, pos); err != language.ErrPosition {
				t.Fatalf("Hover error = %v, want ErrPosition", err)
			}
		})
	}
}

func TestFormatEditRangeUsesUTF16AcrossEmojiAndCRLF(t *testing.T) {
	text := "SELECT '😀', ID FROM CITY\r\nWHERE ID='😀'"
	req := language.Request{
		Document: language.Document{URI: "memory:format-unicode", Text: text, Version: 1},
		Context:  language.Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	result, err := language.NewService().Format(context.Background(), req, language.FormatOptions{TabSize: 2, InsertSpaces: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Value) != 1 {
		t.Fatalf("format edits = %#v, want one", result.Value)
	}
	edit := result.Value[0]
	start, err := language.ByteOffset(text, edit.Range.Start)
	if err != nil {
		t.Fatalf("edit start: %v", err)
	}
	end, err := language.ByteOffset(text, edit.Range.End)
	if err != nil {
		t.Fatalf("edit end: %v", err)
	}
	if start != 0 || end != len(text) {
		t.Fatalf("format range = %+v (%d:%d), want entire source (%d bytes)", edit.Range, start, end, len(text))
	}
	direct, err := formatter.FormatWithDriverVariant(text, lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 2, InsertSpaces: true}}, &config.Config{LowercaseKeywords: true}, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	if err != nil {
		t.Fatal(err)
	}
	endPos, err := language.PositionAt(text, len(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(direct) != 1 || direct[0].Range.Start != (lsp.Position{Line: 0, Character: 0}) || direct[0].Range.End != (lsp.Position{Line: endPos.Line, Character: endPos.UTF16Column}) {
		t.Fatalf("direct formatter range = %#v, want UTF-16 [0:0-%d:%d]", direct, endPos.Line, endPos.UTF16Column)
	}
	formatted := text[:start] + edit.Text + text[end:]
	want := "select\n  '😀',\n  ID\nfrom\n  CITY\nwhere\n  ID = '😀'"
	if formatted != want {
		t.Fatalf("applied formatted edit = %q, want %q", formatted, want)
	}
}

func TestDirectFormatterAcceptsLoneCRLineBreak(t *testing.T) {
	text := "select x\rfrom t"
	edits, err := formatter.FormatWithDriverVariant(text, lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 2, InsertSpaces: true}}, &config.Config{LowercaseKeywords: true}, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	if err != nil {
		t.Fatalf("FormatWithDriverVariant(%q): %v", text, err)
	}
	end, err := language.PositionAt(text, len(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0].Range.Start != (lsp.Position{Line: 0, Character: 0}) || edits[0].Range.End != (lsp.Position{Line: end.Line, Character: end.UTF16Column}) {
		t.Fatalf("direct formatter edits = %#v, want full AST range ending at %+v", edits, end)
	}
}
