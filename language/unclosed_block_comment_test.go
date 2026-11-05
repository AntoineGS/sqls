package language_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func TestCompletionContinuesBeforeUnclosedBlockComment(t *testing.T) {
	const text = "select c. from city c /* unfinished"
	items := completeAt(t, interBaseCompletionRequest(interBaseBaseCompletionCache(), text, dialect.SQLVariantInterBase3), len("select c."))
	if !completionLabels(items)["ID"] {
		t.Fatalf("completion labels = %v, want CITY.ID", completionLabels(items))
	}
}

func TestCompletionInsideUnclosedBlockCommentReturnsNoCandidates(t *testing.T) {
	const text = "select c. from city c /* unfinished"
	req := interBaseCompletionRequest(interBaseBaseCompletionCache(), text, dialect.SQLVariantInterBase3)
	result, err := language.NewService().Complete(context.Background(), req, language.Position{UTF16Column: len(text)})
	if err != nil {
		t.Fatalf("Complete() inside comment: %v", err)
	}
	if len(result.Value.Items) != 0 {
		t.Fatalf("completion labels inside comment = %v, want none", completionLabels(result.Value.Items))
	}
}

func TestHoverContinuesBeforeUnclosedBlockComment(t *testing.T) {
	const text = "SELECT ID FROM CITY /* unfinished"
	result, err := language.NewService().Hover(context.Background(), hoverRequest(hoverCache(), text), language.Position{UTF16Column: 8})
	if err != nil {
		t.Fatalf("Hover() before comment: %v", err)
	}
	if !strings.Contains(result.Value.Markdown, "ID") {
		t.Fatalf("hover markdown = %q, want the ID column", result.Value.Markdown)
	}
}

func TestHoverInsideUnclosedBlockCommentReturnsNoHover(t *testing.T) {
	const text = "SELECT ID FROM CITY /* unfinished"
	_, err := language.NewService().Hover(context.Background(), hoverRequest(hoverCache(), text), language.Position{UTF16Column: len(text)})
	if err != language.ErrNoHover {
		t.Fatalf("Hover() inside comment error = %v, want ErrNoHover", err)
	}
}

func TestSignatureHelpContinuesBeforeUnclosedBlockComment(t *testing.T) {
	const text = "execute procedure myproc( /* unfinished"
	position := strings.Index(text, "/*")
	result, err := language.NewService().SignatureHelp(context.Background(), signatureRequest(signatureProcedureCache(), text), language.Position{UTF16Column: position})
	if err != nil {
		t.Fatalf("SignatureHelp() before comment: %v", err)
	}
	if len(result.Value.Signatures) != 1 || result.Value.Signatures[0].Label != "MYPROC (IN_CODE, IN_AMOUNT)" {
		t.Fatalf("signature help = %+v, want MYPROC signature", result.Value)
	}
}

func TestSignatureHelpInsideUnclosedBlockCommentReturnsEmpty(t *testing.T) {
	const text = "execute procedure myproc( /* unfinished"
	result, err := language.NewService().SignatureHelp(context.Background(), signatureRequest(signatureProcedureCache(), text), language.Position{UTF16Column: len(text)})
	if err != nil {
		t.Fatalf("SignatureHelp() inside comment: %v", err)
	}
	if len(result.Value.Signatures) != 0 {
		t.Fatalf("signature help inside comment = %+v, want none", result.Value)
	}
}

func TestDiagnosticsContinueBeforeUnclosedBlockComment(t *testing.T) {
	request := language.Request{
		Document: language.Document{URI: "memory:unclosed-comment-diagnostics", Text: "SELECT ID FROM T WHERE ID = NULL /* unfinished"},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	result, err := language.NewService().Diagnostics(context.Background(), request)
	if err != nil {
		t.Fatalf("Diagnostics(): %v", err)
	}
	for _, diagnostic := range result.Value {
		if diagnostic.Code == "interbase-null-comparison" {
			return
		}
	}
	t.Fatalf("diagnostics = %+v, want the null-comparison finding before the comment", result.Value)
}

func TestCompletionStillReportsLexicalErrorsBeforeUnclosedComment(t *testing.T) {
	const text = "select ! /* unfinished"
	req := interBaseCompletionRequest(interBaseBaseCompletionCache(), text, dialect.SQLVariantInterBase3)
	if _, err := language.NewService().Complete(context.Background(), req, language.Position{UTF16Column: len(text)}); err == nil {
		t.Fatal("Complete() accepted an illegal token before the unclosed comment")
	}
}
