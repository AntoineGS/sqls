package language

import (
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/parser/parseutil"
)

// genIDFunctionName is the only call in which a generator name is required.
// Offering every generator in every expression position would bury column
// candidates.
const genIDFunctionName = "GEN_ID"

// procedureCandidates offers every procedure, for the EXECUTE PROCEDURE
// position. A procedure with no output parameters is still executable, so it
// is offered here even though it cannot appear in a FROM clause.
func (c *completionEngine) procedureCandidates() []CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []CompletionItem{}
	for _, name := range c.cache.SortedProcedures() {
		desc, ok := c.cache.Procedure(name)
		if !ok {
			continue
		}
		candidates = append(candidates, procedureCandidate(desc, "procedure"))
	}
	return candidates
}

// selectableProcedureCandidates offers only procedures with at least one
// output parameter: an InterBase selectable procedure is legal wherever a
// relation is, and one without output is not selectable.
func (c *completionEngine) selectableProcedureCandidates() []CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []CompletionItem{}
	for _, name := range c.cache.SortedProcedures() {
		desc, ok := c.cache.Procedure(name)
		if !ok || len(desc.OutputParameters) == 0 {
			continue
		}
		candidates = append(candidates, procedureCandidate(desc, "selectable procedure"))
	}
	return candidates
}

func procedureCandidate(desc *catalog.ProcedureDesc, detail string) CompletionItem {
	return CompletionItem{
		Label:         desc.Name,
		Kind:          KindMethod,
		Detail:        detail,
		Documentation: catalog.ProcedureDoc(desc),
	}
}

// viewCandidates is called only where completionTypeView is set and
// completionTypeTable is not. Views stay in SchemaTables, so in every position
// that offers tables the existing table candidate already represents the view
// and gains only the "view" detail; emitting here as well would offer every
// view twice.
//
// A non-completionParentTypeNone parent yields nothing: those branches are the
// member-identifier positions, and a view name is not a member of a table.
func (c *completionEngine) viewCandidates(parent *completionParent) []CompletionItem {
	if !c.catalogEnabled() || parent.Type != completionParentTypeNone {
		return nil
	}
	candidates := []CompletionItem{}
	for _, name := range c.cache.SortedViews() {
		desc, ok := c.cache.View(name)
		if !ok {
			continue
		}
		candidates = append(candidates, viewCandidate(desc))
	}
	return candidates
}

func viewCandidate(desc *catalog.ViewDesc) CompletionItem {
	return CompletionItem{
		Label:         desc.Name,
		Kind:          KindClass,
		Detail:        "view",
		Documentation: catalog.ViewDoc(desc),
	}
}

func (c *completionEngine) generatorCandidates() []CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []CompletionItem{}
	for _, name := range c.cache.SortedGenerators() {
		desc, ok := c.cache.Generator(name)
		if !ok {
			continue
		}
		candidates = append(candidates, CompletionItem{
			Label:         desc.Name,
			Kind:          KindValue,
			Detail:        "generator",
			Documentation: catalog.GeneratorDoc(desc),
		})
	}
	return candidates
}

// externalFunctionCandidates offers UDFs beside the built-in functions. A UDF
// whose argument types the catalog cannot render is still offered: the name is
// what the user needs.
func (c *completionEngine) externalFunctionCandidates() []CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []CompletionItem{}
	for _, name := range c.cache.SortedFunctions() {
		desc, ok := c.cache.Function(name)
		if !ok {
			continue
		}
		candidates = append(candidates, CompletionItem{
			Label:         desc.Name,
			Kind:          KindFunction,
			Detail:        "external function",
			Documentation: catalog.FunctionDoc(desc),
		})
	}
	return candidates
}

// procedureColumnCandidates turns a selectable procedure's output parameters
// into column candidates. Input parameters are deliberately absent: DSQL has
// no named parameters, so an input parameter name is never valid statement
// text.
func (c *completionEngine) procedureColumnCandidates(tableName string) []CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	desc, ok := c.cache.Procedure(tableName)
	if !ok {
		return nil
	}
	candidates := []CompletionItem{}
	for _, param := range desc.OutputParameters {
		candidates = append(candidates, CompletionItem{
			Label:         param.Name,
			Kind:          KindField,
			Detail:        columnDetail(desc.Name),
			Documentation: catalog.ParameterDoc(param),
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
