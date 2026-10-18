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
			want:  "SET TERM ^^ ;\nCREATE PROCEDURE p AS\nBEGIN\n  x = ';^^';\nEND^^\nSET TERM ; ^^",
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatInterBase(t, tt.input); got != tt.want {
				t.Fatalf("formatted =\n%s\nwant\n%s\n(%q)", got, tt.want, got)
			}
		})
	}
}
