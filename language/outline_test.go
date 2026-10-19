package language_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func TestQueryOutlineConvertsQueryAndRelationRanges(t *testing.T) {
	text := "SELECT '𝄞', (SELECT 1 FROM T) FROM U"
	outline, err := language.QueryOutline(context.Background(), text, interBaseOutlineVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(outline.Queries) != 2 {
		t.Fatalf("query count = %d, want 2: %+v", len(outline.Queries), outline.Queries)
	}
	inner := outline.Queries[1]
	if inner.Role != "column" || inner.Parent != 0 {
		t.Fatalf("inner query = %+v, want column child of main query", inner)
	}
	if inner.Range.Start != (language.Position{Line: 0, UTF16Column: 14}) {
		t.Errorf("inner start = %+v, want line 0 UTF-16 column 14", inner.Range.Start)
	}
	if inner.Range.End != (language.Position{Line: 0, UTF16Column: 29}) {
		t.Errorf("inner end = %+v, want line 0 UTF-16 column 29", inner.Range.End)
	}
	if len(inner.Relations) != 1 || inner.Relations[0].Name != "T" || inner.Relations[0].NameText != "T" || inner.Relations[0].NameRange == nil {
		t.Fatalf("inner relations = %+v, want base T with name range", inner.Relations)
	}
	if got := languageRangeText(t, text, *inner.Relations[0].NameRange); got != "T" {
		t.Errorf("name range text = %q, want T", got)
	}
	if got := outline.Queries[0].Relations[0].Name; got != "U" {
		t.Errorf("outer relation name = %q, want U", got)
	}
}

func TestQueryOutlineReturnsRolesAndDerivedLinks(t *testing.T) {
	text := "SELECT (SELECT MIN(l.D) FROM (SELECT D, K FROM BRANDNAMES) l WHERE l.K = s.K) AS BRAND, s.* FROM (SELECT * FROM INVENTORYMASTER a) s"
	outline, err := language.QueryOutline(context.Background(), text, interBaseOutlineVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(outline.Queries) != 4 {
		t.Fatalf("query count = %d, want 4: %+v", len(outline.Queries), outline.Queries)
	}
	want := []struct {
		parent, derived int
		role, roleName  string
	}{
		{-1, 3, "main", ""},
		{0, 2, "column", "BRAND"},
		{1, -1, "derived", "L"},
		{0, -1, "derived", "S"},
	}
	for i, expected := range want {
		query := outline.Queries[i]
		if query.ID != i || query.Parent != expected.parent || query.Role != expected.role || query.RoleName != expected.roleName {
			t.Errorf("query %d = %+v, want parent=%d role=%q name=%q", i, query, expected.parent, expected.role, expected.roleName)
		}
		if i == 0 || i == 1 {
			if len(query.Relations) != 1 || query.Relations[0].DerivedQuery != expected.derived {
				t.Errorf("query %d relations = %+v, want derived link %d", i, query.Relations, expected.derived)
			}
		}
	}
	if got := outline.Queries[2].Relations[0]; got.Name != "BRANDNAMES" || got.NameText != "BRANDNAMES" || got.NameRange == nil {
		t.Errorf("base relation = %+v, want BRANDNAMES and source range", got)
	}
	if got := outline.Queries[3].Relations[0]; got.Name != "INVENTORYMASTER" || got.Alias != "A" || got.AliasText != "a" {
		t.Errorf("aliased base relation = %+v, want INVENTORYMASTER / A / a", got)
	}
}

func TestQueryOutlineConvertsDerivedAndQuotedNameRanges(t *testing.T) {
	text := `SELECT * FROM (SELECT * FROM (SELECT * FROM "Mixed.Name") d) e`
	outline, err := language.QueryOutline(context.Background(), text, interBaseOutlineVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(outline.Queries) != 3 || outline.Queries[0].Relations[0].DerivedQuery != 1 || outline.Queries[1].Relations[0].DerivedQuery != 2 {
		t.Fatalf("outline = %+v, want nested derived-query links", outline.Queries)
	}
	base := outline.Queries[2].Relations[0]
	if base.Name != "Mixed.Name" || base.NameText != `"Mixed.Name"` || base.NameRange == nil || languageRangeText(t, text, *base.NameRange) != `"Mixed.Name"` {
		t.Errorf("quoted base relation = %+v, want exact source spelling/range", base)
	}
	if got := outline.Queries[1].Relations[0]; got.Name != "" || got.NameText != "" || got.NameRange != nil || got.Alias != "D" || got.AliasText != "d" {
		t.Errorf("derived relation = %+v, want empty base name and alias D", got)
	}
}

func TestQueryOutlineHandlesOtherAcceptedSQLShapes(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "comments with SQL-like text", text: `/* SELECT X FROM MISSING WHERE <% @X='Y' ? '1=1' : "a<>'K'" %> */ SELECT * FROM T`, want: 1},
		{name: "where exists", text: "SELECT * FROM T WHERE EXISTS (SELECT 1 FROM U)", want: 2},
		{name: "having", text: "SELECT * FROM T GROUP BY T.ID HAVING (SELECT 1 FROM U) > 0", want: 2},
		{name: "join condition", text: "SELECT * FROM T JOIN U ON T.ID = (SELECT V.ID FROM V)", want: 2},
		{name: "order by other", text: "SELECT * FROM T ORDER BY (SELECT 1 FROM U)", want: 2},
		{name: "union", text: "SELECT X FROM T UNION ALL SELECT X FROM U", want: 2},
		{name: "insert select", text: "INSERT INTO TARGET (ID) SELECT ID FROM SOURCE", want: 2},
		{name: "malformed tail", text: "SELECT * FROM T WHERE EXISTS (SELECT * FROM U", want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outline, err := language.QueryOutline(context.Background(), tt.text, interBaseOutlineVariant())
			if err != nil {
				t.Fatal(err)
			}
			if len(outline.Queries) != tt.want {
				t.Fatalf("query count = %d, want %d: %+v", len(outline.Queries), tt.want, outline.Queries)
			}
			for _, query := range outline.Queries {
				if !validLanguageRange(tt.text, query.Range) {
					t.Errorf("query %d has invalid range %+v", query.ID, query.Range)
				}
				for _, relation := range query.Relations {
					if relation.NameRange != nil && !validLanguageRange(tt.text, *relation.NameRange) {
						t.Errorf("relation name has invalid range %+v", *relation.NameRange)
					}
					if relation.AliasRange != (language.Range{}) && !validLanguageRange(tt.text, relation.AliasRange) {
						t.Errorf("relation alias has invalid range %+v", relation.AliasRange)
					}
				}
			}
		})
	}
}

func TestQueryOutlineNonInterBaseIsEmptyAndChecksCancellation(t *testing.T) {
	outline, err := language.QueryOutline(context.Background(), "SELECT * FROM T", dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL})
	if err != nil || len(outline.Queries) != 0 {
		t.Fatalf("non-InterBase outline = %+v, %v, want empty", outline, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := language.QueryOutline(ctx, "SELECT 1", interBaseOutlineVariant()); !errors.Is(err, language.ErrCanceled) {
		t.Fatalf("canceled QueryOutline error = %v, want ErrCanceled", err)
	}
}

func TestQueryOutlinePropagatesLexingErrors(t *testing.T) {
	if _, err := language.QueryOutline(context.Background(), "SELECT * FROM T WHERE NAME = 'unfinished", interBaseOutlineVariant()); err == nil {
		t.Fatal("QueryOutline returned nil error for an unclosed SQL string")
	}
}

func interBaseOutlineVariant() dialect.DriverVariant {
	return dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
}

func validLanguageRange(text string, r language.Range) bool {
	start, err := language.ByteOffset(text, r.Start)
	if err != nil {
		return false
	}
	end, err := language.ByteOffset(text, r.End)
	return err == nil && start <= end && end <= len(text)
}

func languageRangeText(t *testing.T, text string, r language.Range) string {
	t.Helper()
	start, err := language.ByteOffset(text, r.Start)
	if err != nil {
		t.Fatalf("invalid range start %+v: %v", r.Start, err)
	}
	end, err := language.ByteOffset(text, r.End)
	if err != nil {
		t.Fatalf("invalid range end %+v: %v", r.End, err)
	}
	return strings.TrimSpace(text[start:end])
}
