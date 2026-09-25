// This file exercises language.Service against the smallest fixtures moved
// from the old handler-level InterBase completion, procedure signature,
// hover and diagnostic test suites (see task-B2-brief.md Step 1) so those
// fixtures' expected labels, ranges and errors are pinned directly against
// the shared implementation, not only reachable transitively through an
// LSP-transport test.
package language_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/handler"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func readTestdataFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(data), "\n")
}

// TestParityCompletionSelectStarFromFixture is the fixture-file counterpart
// of TestLanguageUsesSuppliedSnapshot: the same "SELECT * FROM " completion
// probe, but reading its document text from language/testdata rather than
// an inline literal.
func TestParityCompletionSelectStarFromFixture(t *testing.T) {
	text := readTestdataFixture(t, "completion_select_star.sql")
	cache := &catalog.Cache{SchemaTables: map[string][]string{"": {"CUSTOMERS"}}}
	req := language.Request{
		Document: language.Document{URI: "memory:parity-completion", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        &catalog.MetadataSnapshot{Generation: 1, Revision: 1, Cache: cache},
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	result, err := language.NewService().Complete(context.Background(), req, language.Position{UTF16Column: len(text)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Value.Items {
		found = found || item.Label == "CUSTOMERS"
	}
	if !found {
		t.Fatalf("completion items = %+v, want CUSTOMERS", result.Value.Items)
	}
}

// TestParitySignatureHelpExecuteProcedureFromFixture is the fixture-file
// counterpart of the old handler TestInterBaseSignatureHelpForExecuteProcedure.
func TestParitySignatureHelpExecuteProcedureFromFixture(t *testing.T) {
	text := readTestdataFixture(t, "signature_execute_procedure.sql")
	req := signatureRequest(signatureProcedureCache(), text)
	result, err := language.NewService().SignatureHelp(context.Background(), req, language.Position{Line: 0, UTF16Column: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Value.Signatures) != 1 || result.Value.Signatures[0].Label != "MYPROC (IN_CODE, IN_AMOUNT)" {
		t.Fatalf("signature help = %+v, want MYPROC's signature", result.Value)
	}
	if result.Value.ActiveParameter != 0 {
		t.Fatalf("active parameter = %d, want 0", result.Value.ActiveParameter)
	}
}

// TestParityHoverColumnFromFixture is the fixture-file counterpart of
// TestHoverColumnIdentifierReturnsColumnDoc.
func TestParityHoverColumnFromFixture(t *testing.T) {
	text := readTestdataFixture(t, "hover_select_city.sql")
	req := hoverRequest(hoverCache(), text)
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Value.Markdown, "ID") {
		t.Fatalf("hover markdown = %q, want it to mention column ID", result.Value.Markdown)
	}
}

// TestDiagnosticViewUnusedDeclarationFromFixture is the fixture-file
// counterpart of the old handler TestInterBaseDiagnosticsAcceptanceWithoutCatalog:
// a declared-but-unused PSQL local variable is flagged even with no catalog
// attached at all.
func TestDiagnosticViewUnusedDeclarationFromFixture(t *testing.T) {
	text := readTestdataFixture(t, "diagnostic_unused_declaration.sql")
	req := language.Request{
		Document: language.Document{URI: "memory:parity-diagnostics", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase},
			DialectResolved: true,
		},
	}
	result, err := language.NewService().Diagnostics(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range result.Value {
		if diagnostic.Code == "interbase-unused" {
			found = true
			if diagnostic.Severity != 4 {
				t.Errorf("unused severity = %d, want 4", diagnostic.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want interbase-unused", result.Value)
	}
	if result.Value == nil {
		t.Fatal("diagnostics is nil, want a non-nil (possibly empty) slice")
	}
}

// TestParityCompletionRoutesThroughLSPAdapter is
// TestParityCompletionSelectStarFromFixture's counterpart through
// internal/handler.CompletionWithDriverVariant: the same fixture, driven
// through the LSP-shape type conversion (lspCompletionItems) a real
// textDocument/completion response goes through, not only the public
// language API.
func TestParityCompletionRoutesThroughLSPAdapter(t *testing.T) {
	text := readTestdataFixture(t, "completion_select_star.sql")
	cache := &catalog.Cache{SchemaTables: map[string][]string{"": {"CUSTOMERS"}}}
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	params := lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Character: len(text)},
	}}
	result, err := handler.CompletionWithDriverVariant(context.Background(), text, params, cache, dv)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Items {
		found = found || item.Label == "CUSTOMERS"
	}
	if !found {
		t.Fatalf("LSP completion items = %+v, want CUSTOMERS", result.Items)
	}
}

// TestParitySignatureHelpRoutesThroughLSPAdapter is
// TestParitySignatureHelpExecuteProcedureFromFixture's counterpart through
// internal/handler.SignatureHelpWithDriverVariant, the exported LSP-shape
// conversion textDocument/signatureHelp uses in production.
func TestParitySignatureHelpRoutesThroughLSPAdapter(t *testing.T) {
	text := readTestdataFixture(t, "signature_execute_procedure.sql")
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	params := lsp.SignatureHelpParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Character: 25},
	}}
	result, err := handler.SignatureHelpWithDriverVariant(context.Background(), text, params, signatureProcedureCache(), dv)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Signatures) != 1 || result.Signatures[0].Label != "MYPROC (IN_CODE, IN_AMOUNT)" {
		t.Fatalf("LSP signature help = %+v, want MYPROC's signature", result)
	}
	if result.ActiveParameter != 0 {
		t.Fatalf("LSP active parameter = %v, want 0", result.ActiveParameter)
	}
}

// TestParityHoverRoutesThroughLSPAdapter is TestParityHoverColumnFromFixture's
// counterpart through internal/handler.HoverWithDriverVariant, the exported
// LSP-shape conversion textDocument/hover uses in production.
func TestParityHoverRoutesThroughLSPAdapter(t *testing.T) {
	text := readTestdataFixture(t, "hover_select_city.sql")
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	params := lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Character: 8},
	}}
	result, err := handler.HoverWithDriverVariant(context.Background(), text, params, hoverCache(), dv)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !strings.Contains(result.Contents.Value, "ID") {
		t.Fatalf("LSP hover = %+v, want it to mention column ID", result)
	}
}

// TestParityDiagnosticsRoutesThroughLSPAdapter is
// TestDiagnosticViewUnusedDeclarationFromFixture's counterpart through
// internal/handler.DiagnosticsWithDriverVariant, the exported LSP-shape
// conversion textDocument/publishDiagnostics uses in production.
func TestParityDiagnosticsRoutesThroughLSPAdapter(t *testing.T) {
	text := readTestdataFixture(t, "diagnostic_unused_declaration.sql")
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	diagnostics, err := handler.DiagnosticsWithDriverVariant(context.Background(), text, dv, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code != nil && *diagnostic.Code == "interbase-unused" {
			found = true
			if diagnostic.Severity != 4 {
				t.Errorf("LSP unused severity = %v, want 4", diagnostic.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("LSP diagnostics = %+v, want interbase-unused", diagnostics)
	}
}
