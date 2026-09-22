package parseutil

import (
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/token"
)

func walkerAt(t *testing.T, text string, col int) *NodeWalker {
	t.Helper()
	parsed, err := parser.ParseWithDriver(text, dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("ParseWithDriver:", err)
	}
	return NewNodeWalker(parsed, token.Pos{Line: 0, Col: col})
}

func TestEnclosingCall(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		col        int
		wantOK     bool
		wantCallee string
		wantInside bool
	}{
		{
			name:       "inside an empty argument list",
			text:       "execute procedure myproc(",
			col:        25,
			wantOK:     true,
			wantCallee: "myproc",
			wantInside: true,
		},
		{
			name:       "inside a populated argument list",
			text:       "execute procedure myproc(1, 2)",
			col:        28,
			wantOK:     true,
			wantCallee: "myproc",
			wantInside: true,
		},
		{
			name:       "selectable procedure call",
			text:       "select * from myproc(",
			col:        21,
			wantOK:     true,
			wantCallee: "myproc",
			wantInside: true,
		},
		{
			name:       "built-in function call",
			text:       "select gen_id(",
			col:        14,
			wantOK:     true,
			wantCallee: "gen_id",
			wantInside: true,
		},
		{
			// On the callee name itself, not between the parentheses.
			// Signature help and generator completion both need this
			// distinction: neither fires while the name is still being typed.
			name:       "on the callee name",
			text:       "select gen_id(1, 2)",
			col:        10,
			wantOK:     true,
			wantCallee: "gen_id",
			wantInside: false,
		},
		{
			name:   "not in a call at all",
			text:   "select * from city",
			col:    16,
			wantOK: false,
		},
		{
			name:   "parenthesis that is not a call",
			text:   "select (1 + 2",
			col:    12,
			wantOK: false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EnclosingCall(walkerAt(t, tt.text, tt.col))
			if ok != tt.wantOK {
				t.Fatalf("EnclosingCall ok = %v, want %v (call=%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.Callee != tt.wantCallee {
				t.Errorf("Callee = %q, want %q", got.Callee, tt.wantCallee)
			}
			if got.Inside != tt.wantInside {
				t.Errorf("Inside = %v, want %v", got.Inside, tt.wantInside)
			}
		})
	}
}

func TestCallInfoActiveParameter(t *testing.T) {
	const text = "execute procedure myproc(123, 45)"
	cases := []struct {
		col  int
		want int
	}{
		{col: 25, want: 0},
		{col: 28, want: 0},
		{col: 29, want: 1},
		{col: 31, want: 1},
	}

	for _, tt := range cases {
		pos := token.Pos{Line: 0, Col: tt.col}
		call, ok := EnclosingCall(walkerAt(t, text, tt.col))
		if !ok {
			t.Fatalf("col %d: no enclosing call", tt.col)
		}
		if got := call.ActiveParameter(pos); got != tt.want {
			t.Errorf("col %d: ActiveParameter = %d, want %d", tt.col, got, tt.want)
		}
	}
}

func TestCallInfoActiveParameterWithNoArguments(t *testing.T) {
	// An empty argument list has no ast.IdentifierList at all, so GetIndex is
	// unreachable. The answer is 0, never -1: the cursor is on the first
	// parameter, and a negative index would render as "no active parameter"
	// in the editor.
	call, ok := EnclosingCall(walkerAt(t, "execute procedure myproc(", 25))
	if !ok {
		t.Fatal("no enclosing call")
	}
	if got := call.ActiveParameter(token.Pos{Line: 0, Col: 25}); got != 0 {
		t.Errorf("ActiveParameter = %d, want 0 for an empty argument list", got)
	}
}
