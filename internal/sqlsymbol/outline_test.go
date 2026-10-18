package sqlsymbol

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestQueryOutlineNestedQueriesAndRelations(t *testing.T) {
	text := "SELECT (SELECT MIN(l.D) FROM (SELECT D, K FROM BRANDNAMES) l WHERE l.K = s.K) AS BRAND, s.* FROM (SELECT * FROM INVENTORYMASTER a) s"
	queries, err := QueryOutline(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 4 {
		t.Fatalf("query count = %d, want 4: %+v", len(queries), queries)
	}
	wantQueries := []struct {
		parent int
		role   string
		name   string
	}{
		{-1, "main", ""},
		{0, "column", "BRAND"},
		{1, "derived", "L"},
		{0, "derived", "S"},
	}
	for i, want := range wantQueries {
		if got := queries[i]; got.ID != i || got.Parent != want.parent || got.Role != want.role || got.RoleName != want.name {
			t.Errorf("query %d = {ID:%d Parent:%d Role:%q RoleName:%q}, want parent=%d role=%q name=%q", i, got.ID, got.Parent, got.Role, got.RoleName, want.parent, want.role, want.name)
		}
	}
	if got := queries[0].Relations; len(got) != 1 || got[0].Alias == nil || got[0].Alias.Key() != "S" || got[0].DerivedQuery != 3 {
		t.Errorf("main derived relation = %+v, want S -> query 3", got)
	}
	if got := queries[1].Relations; len(got) != 1 || got[0].Alias == nil || got[0].Alias.Key() != "L" || got[0].DerivedQuery != 2 {
		t.Errorf("column query derived relation = %+v, want L -> query 2", got)
	}
	if got := queries[2].Relations; len(got) != 1 || got[0].Name.Key() != "BRANDNAMES" || got[0].NameRange == nil || text[got[0].NameRange.Start:got[0].NameRange.End] != "BRANDNAMES" {
		t.Errorf("innermost relation = %+v, want BRANDNAMES with exact name span", got)
	}
	if got := queries[3].Relations; len(got) != 1 || got[0].Name.Key() != "INVENTORYMASTER" || got[0].Alias == nil || got[0].Alias.Key() != "A" {
		t.Errorf("source relation = %+v, want INVENTORYMASTER alias A", got)
	}
}

func TestQueryOutlineClassifiesSubqueryRoles(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		role      string
		roleName  string
		predicate string
	}{
		{name: "where exists", text: "SELECT * FROM T WHERE EXISTS (SELECT 1 FROM U)", role: "where", predicate: "EXISTS"},
		{name: "where without from", text: "SELECT 1 WHERE EXISTS (SELECT 1 FROM U)", role: "where", predicate: "EXISTS"},
		{name: "having", text: "SELECT * FROM T GROUP BY T.ID HAVING (SELECT 1 FROM U) > 0", role: "having"},
		{name: "join condition", text: "SELECT * FROM T JOIN U ON T.ID = (SELECT V.ID FROM V)", role: "join"},
		{name: "other", text: "SELECT * FROM T ORDER BY (SELECT 1 FROM U)", role: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries, err := QueryOutline(tt.text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			if len(queries) != 2 {
				t.Fatalf("query count = %d, want 2: %+v", len(queries), queries)
			}
			got := queries[1]
			if got.Parent != 0 || got.Role != tt.role || got.RoleName != tt.roleName || got.Predicate != tt.predicate {
				t.Fatalf("subquery = %+v, want parent=0 role=%q name=%q predicate=%q", got, tt.role, tt.roleName, tt.predicate)
			}
		})
	}
}

func TestQueryOutlineDerivedJoinNameSpans(t *testing.T) {
	text := "SELECT * FROM T LEFT JOIN (SELECT DISTINCT G FROM T) x ON x.G = T.G"
	queries, err := QueryOutline(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || queries[1].Role != "derived" || queries[1].RoleName != "X" {
		t.Fatalf("outline = %+v, want main and derived X", queries)
	}
	if len(queries[0].Relations) != 2 || len(queries[1].Relations) != 1 || queries[0].Relations[0].NameRange == nil || queries[1].Relations[0].NameRange == nil {
		t.Fatalf("main and derived relations = %+v / %+v, want base T names", queries[0].Relations, queries[1].Relations)
	}
	for _, relation := range []OutlineRelation{queries[0].Relations[0], queries[1].Relations[0]} {
		span := relation.NameRange
		if got := text[span.Start:span.End]; got != "T" {
			t.Errorf("name span text = %q, want T (relation %+v)", got, relation)
		}
	}
	if got := queries[0].Relations[1].DerivedQuery; got != 1 {
		t.Errorf("derived link = %d, want 1", got)
	}
}

func TestQueryOutlineIgnoresSQLLikeComments(t *testing.T) {
	text := `/* SELECT X FROM MISSING WHERE <% @X='Y' ? '1=1' : "a<>'K'" %> */
SELECT * FROM T -- SELECT Y FROM ALSO_MISSING
`
	queries, err := QueryOutline(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || len(queries[0].Relations) != 1 || queries[0].Relations[0].Name.Key() != "T" {
		t.Fatalf("outline = %+v, want only SELECT from T", queries)
	}
}

func TestQueryOutlineNestedDerivedAndQuotedNames(t *testing.T) {
	text := `SELECT * FROM (SELECT * FROM (SELECT * FROM "Mixed.Name") d) e`
	queries, err := QueryOutline(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 3 {
		t.Fatalf("query count = %d, want 3: %+v", len(queries), queries)
	}
	for i, wantRole := range []string{"main", "derived", "derived"} {
		if queries[i].Role != wantRole {
			t.Errorf("query %d role = %q, want %q", i, queries[i].Role, wantRole)
		}
	}
	if queries[0].Relations[0].DerivedQuery != 1 || queries[1].Relations[0].DerivedQuery != 2 {
		t.Fatalf("derived links = %d, %d, want 1, 2", queries[0].Relations[0].DerivedQuery, queries[1].Relations[0].DerivedQuery)
	}
	base := queries[2].Relations[0]
	if base.Name != (Name{Text: "Mixed.Name", Quoted: true}) || base.NameRange == nil || text[base.NameRange.Start:base.NameRange.End] != `"Mixed.Name"` {
		t.Fatalf("quoted relation = %+v, want exact quoted name span", base)
	}
}

func TestQueryOutlineUnionAndInsertSelect(t *testing.T) {
	t.Run("union arm", func(t *testing.T) {
		text := "SELECT X FROM T UNION ALL SELECT X FROM U"
		queries, err := QueryOutline(text, interBaseVariant())
		if err != nil {
			t.Fatal(err)
		}
		if len(queries) != 2 || queries[0].Role != "main" || queries[1].Role != "union" || queries[1].Parent != -1 {
			t.Fatalf("queries = %+v, want main and top-level union arm", queries)
		}
	})
	t.Run("insert source", func(t *testing.T) {
		text := "INSERT INTO TARGET (ID) SELECT ID FROM SOURCE"
		queries, err := QueryOutline(text, interBaseVariant())
		if err != nil {
			t.Fatal(err)
		}
		if len(queries) != 2 || queries[0].Kind != "INSERT" || queries[1].Kind != "SELECT" || queries[1].Role != "main" {
			t.Fatalf("queries = %+v, want INSERT target and top-level SELECT source", queries)
		}
		if got := queries[0].Relations; len(got) != 1 || got[0].Name.Key() != "TARGET" {
			t.Errorf("insert relations = %+v, want TARGET", got)
		}
		if got := queries[1].Relations; len(got) != 1 || got[0].Name.Key() != "SOURCE" {
			t.Errorf("source relations = %+v, want SOURCE", got)
		}
	})
}

func TestQueryOutlineBestEffortMalformedTail(t *testing.T) {
	text := "SELECT * FROM T WHERE EXISTS (SELECT * FROM U"
	queries, err := QueryOutline(text, interBaseVariant())
	if err != nil {
		t.Fatalf("QueryOutline returned error for lexer-accepted incomplete SQL: %v", err)
	}
	if len(queries) != 2 || queries[1].Role != "where" || queries[1].Range.End > len(text) {
		t.Fatalf("outline = %+v, want best-effort nested WHERE query", queries)
	}
}

func TestQueryOutlineReturnsLexingError(t *testing.T) {
	if _, err := QueryOutline("SELECT * FROM T WHERE NAME = 'unfinished", interBaseVariant()); err == nil {
		t.Fatal("QueryOutline returned nil error for an unclosed SQL string")
	}
}

func TestQueryOutlineSyntheticProfilerShape(t *testing.T) {
	lines := []string{
		"SELECT",
		"  (SELECT MIN(lkp_1.D)",
		"   FROM (",
		"     SELECT D, K",
		"     FROM BRANDNAMES",
		"   ) lkp_1",
		"   WHERE lkp_1.K = m.K",
		"  ) AS BRAND,",
		"  sub_q.*",
		"FROM (",
		"  SELECT m.ID, m.G",
		"  FROM INVENTORYMASTER m",
		") sub_q",
		"LEFT JOIN (",
		"  SELECT DISTINCT G",
		"  FROM T",
		") lkp_3 ON lkp_3.G = sub_q.G;",
	}
	for len(lines) < 60 {
		lines = append(lines, fmt.Sprintf("-- synthetic fixture padding %02d", len(lines)+1))
	}
	text := strings.Join(lines, "\n")
	if got := strings.Count(text, "\n") + 1; got != 60 {
		t.Fatalf("fixture lines = %d, want 60", got)
	}
	queries, err := QueryOutline(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 5 {
		t.Fatalf("query count = %d, want 5: %+v", len(queries), queries)
	}
	wantRoles := []string{"main", "column", "derived", "derived", "derived"}
	for i, want := range wantRoles {
		if queries[i].Role != want {
			t.Errorf("query %d role = %q, want %q", i, queries[i].Role, want)
		}
	}
	if queries[1].RoleName != "BRAND" || queries[2].RoleName != "LKP_1" || queries[3].RoleName != "SUB_Q" || queries[4].RoleName != "LKP_3" {
		t.Errorf("role names = %q, %q, %q, %q", queries[1].RoleName, queries[2].RoleName, queries[3].RoleName, queries[4].RoleName)
	}
}

func TestQueryOutlineUsesByteSpansAfterNonBMP(t *testing.T) {
	text := "SELECT '𝄞', (SELECT 1 FROM T) FROM U"
	queries, err := QueryOutline(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("query count = %d, want 2: %+v", len(queries), queries)
	}
	if got, want := queries[1].Range.Start, strings.Index(text, "SELECT 1"); got != want {
		t.Errorf("inner start byte = %d, want %d", got, want)
	}
	if got, want := queries[1].Range.End, strings.Index(text, ") FROM U"); got != want {
		t.Errorf("inner end byte = %d, want %d", got, want)
	}
}

func TestQueryOutlineNonInterBaseReturnsEmpty(t *testing.T) {
	queries, err := QueryOutline("SELECT * FROM T", dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL})
	if err != nil || len(queries) != 0 {
		t.Fatalf("QueryOutline = %+v, %v, want empty outline", queries, err)
	}
}
