package language

import (
	"context"
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// ValidateDiagnosticPolicy validates rule codes and levels against SQLs's
// single diagnostic registry.
func ValidateDiagnosticPolicy(policy DiagnosticPolicy) error {
	if err := (sqlsymbol.DiagnosticOptions{Rules: policy.Rules}).Validate(); err != nil {
		return fmt.Errorf("language: invalid diagnostic policy: %w", err)
	}
	return nil
}

// Diagnostics returns static diagnostics for req.Document, interpreted
// against req.Context. It only analyzes InterBase-dialect documents;
// other dialects, and documents whose dialect is not yet resolved, yield
// an empty (never nil) diagnostics list. Malformed SQL that the analyzer
// cannot parse also yields an empty list rather than an error: in-progress
// edits must not surface a diagnostics failure.
func (s *Service) Diagnostics(ctx context.Context, req Request) (Result[[]Diagnostic], error) {
	if err := ValidateDiagnosticPolicy(req.Policy); err != nil {
		return Result[[]Diagnostic]{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result[[]Diagnostic]{}, ErrCanceled
	}
	stamp := stampFor(req)
	diagnostics := make([]Diagnostic, 0)
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase || !req.Context.DialectResolved {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}
	analysis, err := sqlsymbol.AnalyzeDiagnostics(req.Document.Text, req.Context.Variant)
	if err != nil {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}
	// Text-only rules remain active regardless of catalog readiness. The
	// snapshot adapter independently gates each semantic namespace/category.
	var catalogView sqlsymbol.Catalog = snapshotSemanticCatalog{snapshot: req.Context.Snapshot}
	found := analysis.DiagnosticsWithOptions(catalogView, sqlsymbol.DiagnosticOptions{Rules: req.Policy.Rules})
	sort.SliceStable(found, func(i, j int) bool {
		left, right := found[i], found[j]
		if left.Span.Start != right.Span.Start {
			return left.Span.Start < right.Span.Start
		}
		if left.Span.End != right.Span.End {
			return left.Span.End < right.Span.End
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Message != right.Message {
			return left.Message < right.Message
		}
		return left.Severity < right.Severity
	})
	for _, finding := range found {
		rangeValue, ok := spanRange(req.Document.Text, finding.Span)
		if !ok {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Range:    rangeValue,
			Severity: finding.Severity,
			Code:     finding.Code,
			Source:   "sqls",
			Message:  finding.Message,
		})
	}
	return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
}
