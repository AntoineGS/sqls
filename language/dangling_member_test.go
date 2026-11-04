package language_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

// A qualifier typed before its column ("s." with nothing after the period)
// parses as a member identifier without a child; no position-based request
// may panic on it, whichever driver serves the document.
func TestDanglingMemberIdentifierDoesNotPanic(t *testing.T) {
	texts := []string{
		"select s. from city s",
		"select s.",
		"select * from city s where s.",
		"select s. , s.id from city s",
		"select .id from city s",
	}
	requests := map[string]func(string) language.Request{
		"interbase": func(text string) language.Request {
			return interBaseCompletionRequest(interBaseBaseCompletionCache(), text, dialect.SQLVariantInterBase3)
		},
		"mysql8": func(text string) language.Request {
			return otherDriverCompletionRequest(interBaseBaseCompletionCache(), dialect.DatabaseDriverMySQL8, text)
		},
		"duckdb": func(text string) language.Request {
			return otherDriverCompletionRequest(interBaseBaseCompletionCache(), dialect.DatabaseDriverDuckDB, text)
		},
	}
	svc := language.NewService()
	ctx := context.Background()
	for driver, request := range requests {
		for _, text := range texts {
			req := request(text)
			for col := 0; col <= len(text); col++ {
				pos := language.Position{UTF16Column: col}
				calls := map[string]func(){
					"Hover":         func() { _, _ = svc.Hover(ctx, req, pos) },
					"Definition":    func() { _, _ = svc.Definition(ctx, req, pos) },
					"References":    func() { _, _ = svc.References(ctx, req, pos, true) },
					"Rename":        func() { _, _ = svc.Rename(ctx, req, pos, "X") },
					"SignatureHelp": func() { _, _ = svc.SignatureHelp(ctx, req, pos) },
					"Complete":      func() { _, _ = svc.Complete(ctx, req, pos) },
				}
				for name, call := range calls {
					if err := recoverPanic(call); err != nil {
						t.Errorf("%s %s(%q) at %d: %v", driver, name, text, col, err)
					}
				}
			}
		}
	}
}

func recoverPanic(call func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	call()
	return nil
}

// Hovering the qualifier of a dangling "s." describes its relation, exactly as
// it does for the qualifier of a complete "s.ID".
func TestHoverDanglingQualifierDescribesItsRelation(t *testing.T) {
	for _, text := range []string{"SELECT s. FROM CITY s", "SELECT s.ID FROM CITY s"} {
		res, err := language.NewService().Hover(context.Background(), hoverRequest(hoverCache(), text), language.Position{UTF16Column: 7})
		if err != nil {
			t.Fatalf("Hover(%q): %v", text, err)
		}
		if !strings.HasPrefix(res.Value.Markdown, "# `CITY` table") {
			t.Fatalf("Hover(%q) = %q, want the CITY table", text, res.Value.Markdown)
		}
	}
}
