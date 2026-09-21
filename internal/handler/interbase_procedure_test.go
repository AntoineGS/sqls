package handler

import "testing"

func TestInterBaseProcedureName(t *testing.T) {
	for _, tt := range []struct {
		name  string
		query string
		want  string
	}{
		{name: "call with parenthesised arguments", query: "EXECUTE PROCEDURE MYPROC(1, 2)", want: "MYPROC"},
		{name: "call with no parenthesis", query: "EXECUTE PROCEDURE MYPROC 1 2", want: "MYPROC"},
		{name: "no arguments", query: "EXECUTE PROCEDURE MYPROC", want: "MYPROC"},
		{name: "trailing semicolon", query: "EXECUTE PROCEDURE MYPROC;", want: "MYPROC"},
		{name: "lower case keywords and name", query: "execute procedure myproc(1)", want: "myproc"},
		{name: "quoted name", query: `EXECUTE PROCEDURE "MyProc"(1)`, want: "MyProc"},
		{name: "dollar in name", query: "EXECUTE PROCEDURE MY$PROC", want: "MY$PROC"},
		{name: "extra whitespace", query: "EXECUTE\tPROCEDURE\n  MYPROC", want: "MYPROC"},
		{name: "not a procedure call", query: "SELECT * FROM MYPROC", want: ""},
		{name: "execute without procedure", query: "EXECUTE STMT", want: ""},
		{name: "execute procedure with no name", query: "EXECUTE PROCEDURE", want: ""},
		{name: "empty", query: "", want: ""},
		// Known limitation, recorded rather than worked around: a Dialect-3
		// quoted name containing whitespace does not survive field splitting.
		// It degrades to "", which routes to Exec — today's behaviour — rather
		// than to a wrong decision.
		{name: "quoted name with a space", query: `EXECUTE PROCEDURE "My Proc"(1)`, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := interBaseProcedureName(tt.query); got != tt.want {
				t.Errorf("interBaseProcedureName(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}
