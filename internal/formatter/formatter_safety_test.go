package formatter

import (
	"os"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/lsp"
)

func formatInterBase(t *testing.T, input string) string {
	t.Helper()
	got, err := FormatWithDriverVariant(input,
		lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 2, InsertSpaces: true}},
		&config.Config{LowercaseKeywords: false},
		dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	if err != nil {
		t.Fatalf("format %q: %v", input, err)
	}
	if len(got) != 1 {
		t.Fatalf("format %q: got %d edits, want 1", input, len(got))
	}
	return got[0].NewText
}

func TestFormatRegressions(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{
			name:  "where with and",
			input: "select a.id from items a where a.active = 1 and o.qty > 2",
			want:  "SELECT\n  a.id\nFROM\n  items a\nWHERE\n  a.active = 1\n  AND o.qty > 2",
		},
		{
			name:  "incomplete member keeps the next clause",
			input: "select a. from t",
			want:  "SELECT\n  a.\nFROM\n  t",
		},
		{
			name:  "values keep every comma",
			input: "insert into t (a, b, c) values (1, 'x', current_date)",
			want:  "INSERT INTO t (\n  a,\n  b,\n  c\n)\nVALUES(\n  1,\n  'x',\n  CURRENT_DATE\n)",
		},
		{
			name:  "order by number and direction",
			input: "select a from t order by 2 desc",
			want:  "SELECT\n  a\nFROM\n  t\nORDER BY\n  2 DESC",
		},
		{
			name:  "procedure arguments",
			input: "execute procedure sp_do 1, 'a'",
			want:  "EXECUTE PROCEDURE sp_do 1,\n'a'",
		},
		{
			name:  "column types",
			input: "create table t (a integer not null, b varchar(10) default 'x', primary key (a))",
			want:  "CREATE TABLE t (\n  a INTEGER NOT NULL,\n  b VARCHAR(10) DEFAULT 'x',\n  PRIMARY KEY(a)\n)",
		},
		{
			name:  "double minus is not a comment",
			input: "select a from t where x = - -1",
			want:  "SELECT\n  a\nFROM\n  t\nWHERE\n  x = - -1",
		},
		{
			name:  "charset introducers",
			input: "select _utf8'abc', _win1252 'x' from rdb$database",
			want:  "SELECT\n  _utf8 'abc',\n  _win1252 'x'\nFROM\n  rdb$database",
		},
		{
			name:  "concatenation",
			input: "select a||' '||b from t",
			want:  "SELECT\n  a || ' ' || b\nFROM\n  t",
		},
		{
			name:  "comment before word",
			input: "select /* keep */ x from t",
			want:  "SELECT\n  /* keep */ x\nFROM\n  t",
		},
		{
			name:  "block comment after list comma",
			input: "select a, /* note */ b, c from t",
			want:  "SELECT\n  a,\n  /* note */ b,\n  c\nFROM\n  t",
		},
		{
			name:  "line comment after list comma",
			input: "select a, -- note\nb from t",
			want:  "SELECT\n  a,\n  -- note\n  b\nFROM\n  t",
		},
		{
			name:  "comment after argument comma",
			input: "select f(a, /* n */ b) from t",
			want:  "SELECT\n  f(a, /* n */ b)\nFROM\n  t",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatInterBase(t, tt.input); got != tt.want {
				t.Fatalf("formatted =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// Every statement must format; verifyFormatted already proves the result is
// equivalent, so this asserts the formatter is useful on everyday InterBase SQL.
func TestFormatInterBaseCorpus(t *testing.T) {
	corpus := []string{
		"select first 10 skip 20 r.rdb$relation_name from rdb$relations r where r.rdb$system_flag = 0 order by 1",
		"update orders set status = 'X', qty = qty - 1 where id = :id",
		"delete from orders where created < current_date - 30",
		"select o.id, sum(l.qty * l.price) as total from orders o left join lines l on l.order_id = o.id group by o.id having sum(l.qty) > 10",
		"select a from t union select b from u",
		"select 'it''s' from rdb$database",
		"select cast(x as varchar(10)), extract(year from d) from t",
		"select a.status, a.type from t a where a.name is not null and a.qty between 1 and 5 or a.code in (1, 2, 3)",
		"select name from t where name like 'A%' and coalesce(x, 0) <> -1",
		"select a || ' ' || b from t",
		"select * from t; select * from u;",
		"-- header\nselect x -- trailing\nfrom t /* block */ where y = 1",
		"select (select max(id) from u where u.t_id = t.id) as m from t",
		"select * from sp_list(:a, 2)",
		"insert into t (a, b) select a, b from u",
		"select a.id from items a where a.active = 1 and a.position > 2",
		"select count(distinct a) from t",
		"select a from t where not exists (select 1 from u where u.a = t.a)",
		"select * from t where a = :a and b = null and c = current_timestamp",
		"execute procedure sp_do 1, 'a'",
		"SET TERM ^ ;\nCREATE PROCEDURE P1 RETURNS (X INTEGER) AS\nBEGIN\n  X = 1;\n  SUSPEND;\nEND^\nSET TERM ; ^",
	}
	for _, input := range corpus {
		formatInterBase(t, input)
	}
}

// A generated lookup query: nested derived tables, scalar subqueries, about
// eighty joins and comments between list items.
func TestFormatInterBaseLookupSelect(t *testing.T) {
	input, err := os.ReadFile("testdata/interbase/lookup_select.sql")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/interbase/lookup_select.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got := formatInterBase(t, string(input)) + "\n"; got != string(want) {
		t.Fatalf("formatted =\n%s", got)
	}
}
