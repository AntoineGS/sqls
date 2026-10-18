package formatter

import "testing"

func TestFormatLayout(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{
			name:  "update set list",
			input: "update orders set status = 'X', qty = qty - 1 where id = :id",
			want:  "UPDATE orders\nSET\n  status = 'X',\n  qty = qty - 1\nWHERE\n  id = :id",
		},
		{
			name:  "set statement after an update",
			input: "update t set a = 1; set generator g to 0;",
			want:  "UPDATE t\nSET\n  a = 1;\nSET GENERATOR g TO 0;",
		},
		{
			name:  "between on one line",
			input: "select a from t where a between 1 and 5 and b = 2",
			want:  "SELECT\n  a\nFROM\n  t\nWHERE\n  a BETWEEN 1 AND 5\n  AND b = 2",
		},
		{
			name:  "binary and unary minus",
			input: "delete from orders where created < current_date - 30 and x = -1",
			want:  "DELETE FROM orders\nWHERE\n  created < CURRENT_DATE - 30\n  AND x = -1",
		},
		{
			name:  "function arguments and in lists inline",
			input: "select extract(year from d), coalesce(x, 0), substring(a from 1 for 2) from t where a in (1, 2, 3)",
			want:  "SELECT\n  EXTRACT(YEAR FROM d),\n  COALESCE(x, 0),\n  SUBSTRING(a FROM 1 FOR 2)\nFROM\n  t\nWHERE\n  a IN (1, 2, 3)",
		},
		{
			name:  "case and between inside arguments",
			input: "select sum(case when x = 1 then 1 else 0 end), coalesce(a between 1 and 2, b) from t",
			want:  "SELECT\n  SUM(CASE WHEN x = 1 THEN 1 ELSE 0 END),\n  COALESCE(a BETWEEN 1 AND 2, b)\nFROM\n  t",
		},
		{
			name:  "set term keeps the spelling of term",
			input: "set term ^ ;\nselect 1 from t^\nset Term ; ^",
			want:  "SET term ^ ;\nSELECT\n  1\nFROM\n  t^\nSET Term ; ^",
		},
		{
			name:  "in subquery stays multi-line",
			input: "select a from t where b in (select b from u)",
			want:  "SELECT\n  a\nFROM\n  t\nWHERE\n  b IN (\n    SELECT\n      b\n    FROM\n      u\n  )",
		},
		{
			name:  "union on its own line",
			input: "select a from t union all select b from u union select c from v",
			want:  "SELECT\n  a\nFROM\n  t\nUNION ALL\nSELECT\n  b\nFROM\n  u\nUNION\nSELECT\n  c\nFROM\n  v",
		},
		{
			name:  "insert select",
			input: "insert into t (a, b) select a, b from u",
			want:  "INSERT INTO t(\n  a,\n  b\n)\nSELECT\n  a,\n  b\nFROM\n  u",
		},
		{
			name:  "comments without blank lines or trailing spaces",
			input: "-- header\nselect x -- trailing\nfrom t /* block */ where y = 1",
			want:  "-- header\nSELECT\n  x -- trailing\nFROM\n  t /* block */\nWHERE\n  y = 1",
		},
		{
			name: "procedure body by block depth",
			input: "SET TERM ^ ;\nCREATE PROCEDURE P1 RETURNS (X INTEGER) AS\nBEGIN\n  X = 1;\n  IF (X = 1) THEN\n  BEGIN\n" +
				"    X = CASE WHEN X = 1 THEN 2 ELSE 3 END;\n  END\n  SUSPEND;\nEND^\nSET TERM ; ^",
			want: "SET TERM ^ ;\nCREATE PROCEDURE P1 RETURNS(X INTEGER) AS\nBEGIN\n  X = 1;\n  IF (X = 1) THEN\n  BEGIN\n" +
				"    X = CASE\n      WHEN X = 1 THEN 2\n      ELSE 3\n    END;\n  END\n  SUSPEND;\nEND^\nSET TERM ; ^",
		},
		{
			name:  "multi-character terminator and terminators in strings",
			input: "set term ^^ ;\ncreate procedure p as begin x = ';^^'; end^^\nset term ; ^^",
			want:  "SET term ^^ ;\nCREATE PROCEDURE p AS\nBEGIN\n  x = ';^^';\nEND^^\nSET term ; ^^",
		},
		{
			name:  "comment before set term and before the terminator",
			input: "-- note\nSET TERM ^;\nselect 1 from t -- tail\n^",
			want:  "-- note\nSET TERM ^ ;\nSELECT\n  1\nFROM\n  t -- tail\n^",
		},
		{
			name:  "multi-line literals and comments keep their lines",
			input: "select 'a  \n   b', x /* c  \n   d */ from t",
			want:  "SELECT\n  'a  \n   b',\n  x /* c  \n   d */\nFROM\n  t",
		},
		{
			name:  "trigger body without set term",
			input: "CREATE TRIGGER T1 FOR T ACTIVE BEFORE INSERT AS BEGIN NEW.ID = GEN_ID(G, 1); END",
			want:  "CREATE TRIGGER T1 FOR T ACTIVE BEFORE INSERT AS\nBEGIN\n  NEW.ID = GEN_ID(G, 1);\nEND",
		},
		{
			name:  "rows on its own line",
			input: "select a from t rows 200 to 400",
			want:  "SELECT\n  a\nFROM\n  t\nROWS 200 TO 400",
		},
		{
			name:  "rows after a join and after order by",
			input: "select a from t a left join u b on b.id = a.id order by a rows 1 to 2",
			want:  "SELECT\n  a\nFROM\n  t a\nLEFT JOIN u b\n  ON b.id = a.id\nORDER BY\n  a\nROWS 1 TO 2",
		},
		{
			name:  "rows of fetch stays on its line",
			input: "select a from t order by a offset 10 rows fetch next 5 rows only",
			want:  "SELECT\n  a\nFROM\n  t\nORDER BY\n  a OFFSET 10 ROWS FETCH next 5 ROWS ONLY",
		},
		{
			name:  "comment on its own line between clauses",
			input: "select a from t a\nleft join u b on b.id = a.id\n\n/* Oct 7 2014 */\nleft join v c on c.id = a.id",
			want:  "SELECT\n  a\nFROM\n  t a\nLEFT JOIN u b\n  ON b.id = a.id\n  /* Oct 7 2014 */\nLEFT JOIN v c\n  ON c.id = a.id",
		},
		{
			name:  "comment on its own line in a list",
			input: "select a,\n/* own line */\nb from t",
			want:  "SELECT\n  a,\n  /* own line */\n  b\nFROM\n  t",
		},
		{
			name:  "comment on its own line before a comma",
			input: "select a\n/* before comma */, b from t",
			want:  "SELECT\n  a\n  /* before comma */,\n  b\nFROM\n  t",
		},
		{
			name:  "multi-line comment on its own line at the end",
			input: "select a from t where x <> 'K'\n\n/* Was\n  old */",
			want:  "SELECT\n  a\nFROM\n  t\nWHERE\n  x <> 'K'\n  /* Was\n  old */",
		},
		{
			name:  "parenthesized join condition as a block",
			input: "select x from t a left join u c on (c.p = a.p and c.d = a.n)",
			want:  "SELECT\n  x\nFROM\n  t a\nLEFT JOIN u c\n  ON (\n    c.p = a.p\n    AND c.d = a.n\n  )",
		},
		{
			name:  "single parenthesized condition inline",
			input: "select x from t a left join u c on (c.p = a.p) where (a between 1 and 2)",
			want:  "SELECT\n  x\nFROM\n  t a\nLEFT JOIN u c\n  ON (c.p = a.p)\nWHERE\n  (a BETWEEN 1 AND 2)",
		},
		{
			name:  "nested parenthesized conditions",
			input: "select x from t where (a = 1 and (b = 2 or c = 3)) or d = 4",
			want:  "SELECT\n  x\nFROM\n  t\nWHERE\n  (\n    a = 1\n    AND (\n      b = 2\n      OR c = 3\n    )\n  )\n  OR d = 4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatInterBase(t, tt.input); got != tt.want {
				t.Fatalf("formatted =\n%s\nwant\n%s\n(%q)", got, tt.want, got)
			}
		})
	}
}
