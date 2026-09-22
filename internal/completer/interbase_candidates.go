package completer

import (
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser/parseutil"
)

// genIDFunctionName is the only call in which a generator name is required.
// Offering every generator in every expression position would bury column
// candidates.
const genIDFunctionName = "GEN_ID"

// catalogEnabled gates every candidate generator in this file. HasCatalog() is
// false both on a non-InterBase connection and in the window before the
// worker's secondary catalog pass lands, and in both cases completion must
// degrade silently to today's behaviour.
func (c *Completer) catalogEnabled() bool {
	return c.Driver == dialect.DatabaseDriverInterBase && c.DBCache.HasCatalog()
}

// ProcedureCandidates offers every procedure, for the EXECUTE PROCEDURE
// position. A procedure with no output parameters is still executable, so it
// is offered here even though it cannot appear in a FROM clause.
func (c *Completer) ProcedureCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedProcedures() {
		desc, ok := c.DBCache.Procedure(name)
		if !ok {
			continue
		}
		candidates = append(candidates, procedureCandidate(desc, "procedure"))
	}
	return candidates
}

// SelectableProcedureCandidates offers only procedures with at least one
// output parameter: an InterBase selectable procedure is legal wherever a
// relation is, and one without output is not selectable.
func (c *Completer) SelectableProcedureCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedProcedures() {
		desc, ok := c.DBCache.Procedure(name)
		if !ok || len(desc.OutputParameters) == 0 {
			continue
		}
		candidates = append(candidates, procedureCandidate(desc, "selectable procedure"))
	}
	return candidates
}

func procedureCandidate(desc *database.ProcedureDesc, detail string) lsp.CompletionItem {
	return lsp.CompletionItem{
		Label:  desc.Name,
		Kind:   lsp.MethodCompletion,
		Detail: detail,
		Documentation: &lsp.MarkupContent{
			Kind:  lsp.Markdown,
			Value: database.ProcedureDoc(desc),
		},
	}
}

// ViewCandidates is called only where CompletionTypeView is set and
// CompletionTypeTable is not. Views stay in SchemaTables, so in every position
// that offers tables the existing table candidate already represents the view
// and gains only the "view" detail; emitting here as well would offer every
// view twice.
//
// A non-ParentTypeNone parent yields nothing: those branches are the
// member-identifier positions, and a view name is not a member of a table.
func (c *Completer) ViewCandidates(parent *completionParent) []lsp.CompletionItem {
	if !c.catalogEnabled() || parent.Type != ParentTypeNone {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedViews() {
		desc, ok := c.DBCache.View(name)
		if !ok {
			continue
		}
		candidates = append(candidates, viewCandidate(desc))
	}
	return candidates
}

func viewCandidate(desc *database.ViewDesc) lsp.CompletionItem {
	return lsp.CompletionItem{
		Label:  desc.Name,
		Kind:   lsp.ClassCompletion,
		Detail: "view",
		Documentation: &lsp.MarkupContent{
			Kind:  lsp.Markdown,
			Value: database.ViewDoc(desc),
		},
	}
}

func (c *Completer) GeneratorCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedGenerators() {
		desc, ok := c.DBCache.Generator(name)
		if !ok {
			continue
		}
		candidates = append(candidates, lsp.CompletionItem{
			Label:  desc.Name,
			Kind:   lsp.ValueCompletion,
			Detail: "generator",
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.GeneratorDoc(desc),
			},
		})
	}
	return candidates
}

// ExternalFunctionCandidates offers UDFs beside the built-in functions. A UDF
// whose argument types the catalog cannot render is still offered: the name is
// what the user needs.
func (c *Completer) ExternalFunctionCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedFunctions() {
		desc, ok := c.DBCache.Function(name)
		if !ok {
			continue
		}
		candidates = append(candidates, lsp.CompletionItem{
			Label:  desc.Name,
			Kind:   lsp.FunctionCompletion,
			Detail: "external function",
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.FunctionDoc(desc),
			},
		})
	}
	return candidates
}

// procedureColumnCandidates turns a selectable procedure's output parameters
// into column candidates. Input parameters are deliberately absent: DSQL has
// no named parameters, so an input parameter name is never valid statement
// text.
func (c *Completer) procedureColumnCandidates(tableName string) []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	desc, ok := c.DBCache.Procedure(tableName)
	if !ok {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, param := range desc.OutputParameters {
		candidates = append(candidates, lsp.CompletionItem{
			Label:  param.Name,
			Kind:   lsp.FieldCompletion,
			Detail: columnDetail(desc.Name),
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.ParameterDoc(param),
			},
		})
	}
	return candidates
}

// insideGenIDCall reports whether the cursor is in the argument list of a
// GEN_ID call. Being on the callee name is not enough: the user typing
// "gen_i" wants the function, not a generator.
func insideGenIDCall(nw *parseutil.NodeWalker) bool {
	call, ok := parseutil.EnclosingCall(nw)
	if !ok || !call.Inside {
		return false
	}
	return strings.EqualFold(call.Callee, genIDFunctionName)
}
