package queryparams

import "testing"

func TestExecutableSelectsOnlyRemovesStandalonePSQLOutput(t *testing.T) {
	tests := []struct{ name, source, want string }{
		{
			name:   "case insensitive and multiple outputs",
			source: "  for\nSELECT x, y FROM T WHERE note = 'INTO :notAnOutput' InTo :first, :second Do;",
			want:   "  SELECT x, y FROM T WHERE note = 'INTO :notAnOutput';",
		},
		{
			name:   "standalone FOR SELECT without output variables",
			source: "FOR SELECT x FROM T;",
			want:   "SELECT x FROM T;",
		},
		{
			name:   "batch and comments",
			source: "-- into :ignored\nSELECT x FROM T into :output;\nFOR SELECT y FROM U INTO :next DO;",
			want:   "-- into :ignored\nSELECT x FROM T;\nSELECT y FROM U;",
		},
		{
			name:   "into inside a quoted identifier",
			source: `SELECT "INTO :notOutput" FROM T INTO :output;`,
			want:   `SELECT "INTO :notOutput" FROM T;`,
		},
		{
			name:   "procedure body is not independently executable",
			source: "CREATE PROCEDURE P AS BEGIN X = 1; FOR SELECT x FROM T INTO :output DO BEGIN SUSPEND; END; END",
			want:   "CREATE PROCEDURE P AS BEGIN X = 1; FOR SELECT x FROM T INTO :output DO BEGIN SUSPEND; END; END",
		},
		{
			name:   "execute block cannot expose its FOR SELECT fragment",
			source: "EXECUTE BLOCK AS BEGIN X = 1; FOR SELECT x FROM T INTO :output DO",
			want:   "EXECUTE BLOCK AS BEGIN X = 1; FOR SELECT x FROM T INTO :output DO",
		},
		{
			name: "other INTO and FOR syntax stays",
			source: "INSERT INTO T(x) VALUES (1); SELECT x FROM T FOR UPDATE; " +
				"SELECT x FROM (SELECT y FROM U INTO :inner) AS sub;",
			want: "INSERT INTO T(x) VALUES (1); SELECT x FROM T FOR UPDATE; " +
				"SELECT x FROM (SELECT y FROM U INTO :inner) AS sub;",
		},
		{
			name:   "a FOR loop body is not stripped",
			source: "FOR SELECT x FROM T INTO :output DO BEGIN SUSPEND; END",
			want:   "FOR SELECT x FROM T INTO :output DO BEGIN SUSPEND; END",
		},
		{
			name:   "not an output variable",
			source: "SELECT x FROM T INTO output_table;",
			want:   "SELECT x FROM T INTO output_table;",
		},
		{
			name:   "malformed output list is not guessed away",
			source: "SELECT x FROM T INTO :first, DO;",
			want:   "SELECT x FROM T INTO :first, DO;",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExecutableSelects(tt.source); got != tt.want {
				t.Errorf("ExecutableSelects(%q) = %q, want %q", tt.source, got, tt.want)
			}
		})
	}
}
