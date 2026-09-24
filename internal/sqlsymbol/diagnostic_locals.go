package sqlsymbol

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sqls-server/sqls/token"
)

// codeUnknownVariable marks a position whose procedural-variable role is
// syntactically proven -- a genuine assignment lvalue, or an INTO/
// RETURNING_VALUES target -- but that does not match any declaration in
// scope. General expression-position reads (an arbitrary IF/WHILE
// condition, the right-hand side of an assignment, a function argument) are
// deliberately never scanned: unlike an assignment target or an INTO list,
// nothing about those positions proves a bare name there is a variable
// reference rather than a keyword, a context value, a type name, a
// generator/domain/exception name, or some other legal non-variable
// identifier. See the "Fix round 1" section of task-3-report.md for the
// false positives that motivated this narrower scope.
const codeUnknownVariable = "interbase-unknown-variable"

// codeDuplicateDeclaration marks a local variable or parameter declaration
// that reuses a name already declared earlier in the same procedure or
// trigger. It is reported at each conflicting declaration after the first.
const codeDuplicateDeclaration = "interbase-duplicate-declaration"

// localFindings reports unknown procedural-variable references and
// conflicting local/parameter declarations. CREATE PROCEDURE bodies reuse
// the existing local-symbol binder (procedure.go, resolve.go); CREATE
// TRIGGER bodies use this file's own additive recognizer, since navigation
// does not yet model trigger scope.
func (m *diagnosticModel) localFindings() []Finding {
	a := m.analysis
	if a == nil || len(a.lexemes) == 0 {
		return nil
	}
	var findings []Finding
	findings = append(findings, a.procedureDuplicateFindings()...)
	findings = append(findings, a.procedureUnknownVariableFindings(m)...)

	items := significantLexemes(a.lexemes)
	for _, trg := range discoverTriggers(a.Text, items) {
		findings = append(findings, trg.duplicateFindings()...)
		findings = append(findings, trg.unknownVariableFindings(a.Text, items)...)
	}

	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Span.Start < findings[j].Span.Start })
	return findings
}

// --- CREATE PROCEDURE: duplicate declarations ---

func (a *Analysis) procedureDuplicateFindings() []Finding {
	var findings []Finding
	for pi := range a.procedures {
		for _, group := range a.procedures[pi].Symbols {
			findings = append(findings, duplicateFindingsFor(group, "procedure")...)
		}
	}
	return findings
}

func duplicateFindingsFor(group []*Symbol, scopeWord string) []Finding {
	if len(group) < 2 {
		return nil
	}
	for _, sym := range group {
		if sym.RenameBlocked == "malformed procedure declaration" {
			// The whole procedure's declarations could not be parsed
			// reliably; do not draw a duplicate conclusion from them.
			return nil
		}
	}
	ordered := append([]*Symbol(nil), group...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Declaration.Start < ordered[j].Declaration.Start })
	var findings []Finding
	for _, sym := range ordered[1:] {
		findings = append(findings, Finding{
			Span:     sym.Declaration,
			Code:     codeDuplicateDeclaration,
			Message:  fmt.Sprintf("%s is already declared in this %s", sym.Name.Text, scopeWord),
			Severity: 1,
		})
	}
	return findings
}

// --- CREATE PROCEDURE: unknown variable references ---

// procedureUnknownVariableFindings flags exactly two provable variable-role
// positions: a genuine assignment lvalue, and a SELECT ... INTO / EXECUTE
// ... RETURNING_VALUES target. Both are recognized by isWriteOccurrence
// (resolve.go), the same helper the existing binder already uses to
// classify a bound symbol's occurrence as a Read or a Write -- reused here
// rather than reimplemented, so this rule's notion of "provably a variable
// position" never drifts from the binder's own. Every other bare
// identifier in procedural context (an IF/WHILE condition, a function
// argument, the right-hand side of an assignment) is left unscanned: see
// the codeUnknownVariable doc comment for why.
func (a *Analysis) procedureUnknownVariableFindings(m *diagnosticModel) []Finding {
	items := significantLexemes(a.lexemes)
	var findings []Finding
	for i, item := range items {
		if !isNameToken(item) {
			continue
		}
		procIndex := a.procedureAt[i]
		if procIndex < 0 {
			continue
		}
		if withinAny(a.malformedDeclarations, item.Span) {
			continue
		}
		if m != nil && m.Unsupported(i) {
			continue
		}
		ctx := a.contexts[i]
		if !isWriteOccurrence(items, i, ctx) {
			continue
		}
		resolution := a.Resolve(item.Span.Start)
		if resolution.Role != Other && resolution.Role != Column {
			continue
		}
		name, ok := nameFromLexeme(a.Text, item)
		if !ok {
			continue
		}
		if a.malformedDeclarationNames[procIndex][name.Key()] {
			// This exact name already has its own, more specific
			// malformed-declaration problem; reporting it as an unknown
			// variable too would just be a confusing duplicate finding
			// for the same root cause.
			continue
		}
		findings = append(findings, unknownVariableFinding(item.Span, name, procedureCandidateNames(a, procIndex)))
	}
	return findings
}

func procedureCandidateNames(a *Analysis, procIndex int) []string {
	if procIndex < 0 || procIndex >= len(a.procedures) {
		return nil
	}
	names := make([]string, 0, len(a.procedures[procIndex].Symbols))
	for _, group := range a.procedures[procIndex].Symbols {
		if len(group) > 0 {
			names = append(names, group[0].Name.Text)
		}
	}
	return names
}

func withinAny(spans []Span, s Span) bool {
	for _, r := range spans {
		if s.Start >= r.Start && s.Start < r.End {
			return true
		}
	}
	return false
}

func unknownVariableFinding(span Span, name Name, candidates []string) Finding {
	message := fmt.Sprintf("%s is not a declared local variable or parameter", name.Text)
	if suggestion, ok := nearestName(name.Text, candidates); ok {
		message = fmt.Sprintf("%s; did you mean %s?", message, suggestion)
	}
	return Finding{Span: span, Code: codeUnknownVariable, Message: message, Severity: 1}
}

// --- CREATE TRIGGER: additive, diagnostics-only recognition ---

// triggerSymbol is a trigger's own DECLARE VARIABLE local. Triggers do not
// participate in navigation/rename yet, so this is a separate, minimal type
// rather than a reuse of Symbol.
type triggerSymbol struct {
	name        Name
	declaration Span
}

// triggerModel is one recognized CREATE TRIGGER statement: a supported
// subset of InterBase's trigger grammar (see parseTrigger). Triggers whose
// header or body do not match this subset are never added to the discovered
// list, so their content never produces a finding. A trigger whose own
// DECLARE VARIABLE section is malformed is entirely excluded this same way
// (parseTrigger returns ok=false), which also satisfies "withhold dependent
// errors after a malformed declaration" for triggers without needing
// procedure.go's per-name suppression map: there is no partial trigger body
// to selectively report on.
type triggerModel struct {
	name      Name
	relation  Name
	bodyStart int // item index of the outer BEGIN
	bodyEnd   int // item index of the matching END
	locals    map[string][]*triggerSymbol
}

func discoverTriggers(text string, items []lexeme) []triggerModel {
	var triggers []triggerModel
	for i := 0; i < len(items); i++ {
		if !isWord(items[i], "CREATE") || i+1 >= len(items) || !isWord(items[i+1], "TRIGGER") {
			continue
		}
		if trg, ok := parseTrigger(text, items, i); ok {
			triggers = append(triggers, trg)
		}
	}
	return triggers
}

// parseTrigger recognizes:
//
//	CREATE TRIGGER name FOR relation
//	  [ACTIVE|INACTIVE]
//	  (BEFORE|AFTER) (INSERT|UPDATE|DELETE) (OR (INSERT|UPDATE|DELETE))*
//	  [POSITION number]
//	AS
//	  (DECLARE VARIABLE name type;)*
//	BEGIN ... END
//
// Any deviation -- a schema-qualified relation, INSTEAD OF, a malformed
// declaration, an unbalanced body -- reports ok=false and the whole trigger
// is withheld from findings, never guessed at.
func parseTrigger(text string, items []lexeme, start int) (triggerModel, bool) {
	i := start + 2 // past CREATE TRIGGER
	if i >= len(items) || !isNameToken(items[i]) {
		return triggerModel{}, false
	}
	name, ok := nameFromLexeme(text, items[i])
	if !ok {
		return triggerModel{}, false
	}
	i++
	if i >= len(items) || !isWord(items[i], "FOR") {
		return triggerModel{}, false
	}
	i++
	if i >= len(items) || !isNameToken(items[i]) {
		return triggerModel{}, false
	}
	relation, ok := nameFromLexeme(text, items[i])
	if !ok {
		return triggerModel{}, false
	}
	i++
	if i < len(items) && items[i].Token.Kind == token.Period {
		return triggerModel{}, false // schema-qualified relation: unsupported
	}
	if i < len(items) && (isWord(items[i], "ACTIVE") || isWord(items[i], "INACTIVE")) {
		i++
	}
	if i >= len(items) || !(isWord(items[i], "BEFORE") || isWord(items[i], "AFTER")) {
		return triggerModel{}, false
	}
	i++
	if i >= len(items) || !isTriggerEventWord(items[i]) {
		return triggerModel{}, false
	}
	i++
	for i+1 < len(items) && isWord(items[i], "OR") && isTriggerEventWord(items[i+1]) {
		i += 2
	}
	if i < len(items) && isWord(items[i], "POSITION") {
		i++
		if i >= len(items) || items[i].Token.Kind != token.Number {
			return triggerModel{}, false
		}
		i++
	}
	if i >= len(items) || !isWord(items[i], "AS") {
		return triggerModel{}, false
	}
	i++

	trg := triggerModel{name: name, relation: relation, locals: make(map[string][]*triggerSymbol)}
	for i < len(items) && isWord(items[i], "DECLARE") {
		if i+2 >= len(items) || !isWord(items[i+1], "VARIABLE") {
			return triggerModel{}, false
		}
		declName, ok := nameFromLexeme(text, items[i+2])
		if !ok || i+3 >= len(items) || !declarationTypeStart(items[i+3]) {
			return triggerModel{}, false
		}
		declEnd := i + 3
		for declEnd < len(items) && items[declEnd].Token.Kind != token.Semicolon {
			if isWord(items[declEnd], "BEGIN") {
				return triggerModel{}, false
			}
			declEnd++
		}
		if declEnd >= len(items) {
			return triggerModel{}, false
		}
		sym := &triggerSymbol{name: declName, declaration: items[i+2].Span}
		trg.locals[declName.Key()] = append(trg.locals[declName.Key()], sym)
		i = declEnd + 1
	}
	if i >= len(items) || !isWord(items[i], "BEGIN") {
		return triggerModel{}, false
	}
	trg.bodyStart = i

	frames := []bodyFrame{beginFrame}
	i++
	for i < len(items) {
		switch {
		case isWord(items[i], "BEGIN"):
			frames = append(frames, beginFrame)
		case isWord(items[i], "CASE"):
			frames = append(frames, caseFrame)
		case isWord(items[i], "END"):
			frames = frames[:len(frames)-1]
			if len(frames) == 0 {
				trg.bodyEnd = i
				return trg, true
			}
		}
		i++
	}
	return triggerModel{}, false
}

func isTriggerEventWord(item lexeme) bool {
	return isWord(item, "INSERT") || isWord(item, "UPDATE") || isWord(item, "DELETE")
}

func (trg *triggerModel) duplicateFindings() []Finding {
	var findings []Finding
	for _, group := range trg.locals {
		findings = append(findings, duplicateFindingsFor(triggerSymbolsAsSymbols(group), "trigger")...)
	}
	return findings
}

// triggerSymbolsAsSymbols adapts trigger locals to the shared duplicate-
// finding helper without giving triggerSymbol the full Symbol shape
// (RenameBlocked, Reads/Writes, ...) that navigation/rename would need.
func triggerSymbolsAsSymbols(group []*triggerSymbol) []*Symbol {
	out := make([]*Symbol, len(group))
	for i, sym := range group {
		out[i] = &Symbol{Name: sym.name, Declaration: sym.declaration}
	}
	return out
}

func (trg *triggerModel) symbolFor(name Name) (*triggerSymbol, bool) {
	group := trg.locals[name.Key()]
	if len(group) != 1 {
		return nil, len(group) > 1
	}
	return group[0], false
}

func (trg *triggerModel) candidateNames() []string {
	names := make([]string, 0, len(trg.locals))
	for _, group := range trg.locals {
		if len(group) > 0 {
			names = append(names, group[0].name.Text)
		}
	}
	return names
}

// triggerBodyContext classifies every item position within [bodyStart,
// bodyEnd) as SQL or procedural, and marks SQL-region INTO/RETURNING_VALUES
// target positions, mirroring buildContexts' own contextSQL/outputTarget
// tracking in resolve.go (scoped down to the single concern this file
// needs: never treating a SQL column name or predicate as a candidate
// variable, and correctly finding INTO targets). A SELECT/UPDATE/INSERT/
// DELETE keyword opens a SQL region that closes at the next top-level ";"
// or, for a FOR SELECT ... DO cursor loop, at "DO" -- matching resolve.go's
// own "kind, active = contextProcedure, false" transition at FOR's DO.
// MERGE/WITH open an unsupported region (also excluded, never scanned)
// closing at the next ";". This is intentionally simpler than
// buildContexts: it does not need Local/Column/Alias role classification,
// only the SQL/procedural split (inSQL) plus, separately, which of those
// SQL-or-unsupported positions are specifically unsupported (the
// unsupported field) -- a distinction this file's own rule does not need
// but a caller modeling trigger-embedded queries elsewhere does, to avoid
// misreading a bare SELECT/UPDATE/INSERT/DELETE keyword inside a MERGE's
// WHEN clause or a WITH's CTE body as an independent query start.
type triggerBodyContext struct {
	inSQL        []bool
	outputTarget []bool

	// unsupported marks a position whose kind is specifically
	// "unsupported" (a MERGE/WITH region), a strict subset of inSQL (which
	// is true for both "sql" and "unsupported" kinds, since this file's own
	// unknownVariableFindings never needs to tell them apart -- both are
	// equally "never a variable candidate" to that rule). A caller that
	// does need the distinction (for example, one deciding whether a bare
	// keyword genuinely starts a new top-level SQL statement, which must
	// never be true inside MERGE/WITH content) should check inSQL &&
	// !unsupported for "genuinely sql", not inSQL alone.
	unsupported []bool
}

func newTriggerBodyContext(items []lexeme, bodyStart, bodyEnd int) triggerBodyContext {
	n := bodyEnd - bodyStart
	ctx := triggerBodyContext{inSQL: make([]bool, n), outputTarget: make([]bool, n), unsupported: make([]bool, n)}
	kind := "procedural"
	head := ""
	depth := 0
	outputDepth := -1
	outputActive := false
	outputExpect := false
	for idx := bodyStart; idx < bodyEnd; idx++ {
		item := items[idx]
		i := idx - bodyStart
		if item.Token.Kind == token.Semicolon {
			kind, head = "procedural", ""
			outputDepth, outputActive, outputExpect = -1, false, false
		}
		switch kind {
		case "procedural":
			switch {
			case isWord(item, "SELECT"), isWord(item, "UPDATE"), isWord(item, "INSERT"), isWord(item, "DELETE"):
				kind = "sql"
				if word, ok := item.Token.Value.(*token.SQLWord); ok {
					head = strings.ToUpper(word.Keyword)
				}
			case isWord(item, "MERGE"), isWord(item, "WITH"):
				kind = "unsupported"
			}
		case "sql":
			if isWord(item, "DO") {
				kind, head = "procedural", ""
				outputDepth, outputActive, outputExpect = -1, false, false
			}
			if isWord(item, "INTO") && head != "INSERT" {
				outputDepth, outputActive, outputExpect = depth, true, true
			}
			if outputActive && depth <= outputDepth && (isWord(item, "FROM") || isWord(item, "WHERE") || isWord(item, "RETURNING")) {
				outputActive, outputExpect = false, false
			}
			if outputActive && item.Token.Kind == token.Comma && depth == outputDepth {
				outputExpect = true
			}
			if outputActive && isNameToken(item) && depth == outputDepth && outputExpect {
				ctx.outputTarget[i] = true
				outputExpect = false
			}
		case "unsupported":
			// closed only by the top-level ";" handled above
		}
		ctx.inSQL[i] = kind == "sql" || kind == "unsupported"
		ctx.unsupported[i] = kind == "unsupported"
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
	}
	return ctx
}

func isTriggerAssignmentBoundary(prev lexeme) bool {
	if prev.Token.Kind == token.Semicolon {
		return true
	}
	return isWord(prev, "BEGIN") || isWord(prev, "THEN") || isWord(prev, "ELSE") || isWord(prev, "DO")
}

// unknownVariableFindings flags the same two provable variable-role shapes
// as the procedure-side rule -- a genuine assignment lvalue, and an INTO/
// RETURNING_VALUES target -- scoped to the trigger body's own procedural
// regions via triggerBodyContext. Positions inside an embedded SQL
// statement (a column name, a WHERE predicate, an UPDATE ... SET target)
// are never scanned: those are Task 4's concern, not an unbound local.
// NEW/OLD, any name qualified by ".", and any name immediately followed by
// "(" are always skipped, so NEW.col/OLD.col and a function/procedure call
// name are never mistaken for a local.
func (trg *triggerModel) unknownVariableFindings(text string, items []lexeme) []Finding {
	var findings []Finding
	report := func(idx int) {
		item := items[idx]
		if !isNameToken(item) {
			return
		}
		if idx+1 < len(items) && items[idx+1].Token.Kind == token.Period {
			return
		}
		if idx > 0 && items[idx-1].Token.Kind == token.Period {
			return
		}
		if idx+1 < len(items) && items[idx+1].Token.Kind == token.LParen {
			return
		}
		if isWord(item, "NEW") || isWord(item, "OLD") {
			return
		}
		name, ok := nameFromLexeme(text, item)
		if !ok {
			return
		}
		sym, duplicate := trg.symbolFor(name)
		if duplicate || sym != nil {
			return
		}
		findings = append(findings, unknownVariableFinding(item.Span, name, trg.candidateNames()))
	}

	ctx := newTriggerBodyContext(items, trg.bodyStart, trg.bodyEnd)
	for idx := trg.bodyStart + 1; idx < trg.bodyEnd; idx++ {
		i := idx - trg.bodyStart
		item := items[idx]
		switch {
		case ctx.outputTarget[i]:
			report(idx)
		case ctx.inSQL[i]:
			// SQL column names and predicates: never a variable candidate.
		case isNameToken(item) && idx+1 < trg.bodyEnd && items[idx+1].Token.Kind == token.Eq &&
			isTriggerAssignmentBoundary(items[idx-1]):
			report(idx)
		}
	}
	return findings
}

// --- nearest-name suggestion ---

// nearestName returns the unique declared candidate closest to name by
// Levenshtein distance, within a length-scaled threshold. It returns
// ok=false when no candidate is close enough or the closest distance ties
// between two or more candidates: a suggestion is only offered when it is
// unambiguous.
func nearestName(name string, candidates []string) (string, bool) {
	upper := strings.ToUpper(name)
	threshold := nearestThreshold(len(upper))
	best, bestDist, tie := "", 0, false
	found := false
	for _, candidate := range candidates {
		if strings.EqualFold(candidate, name) {
			continue
		}
		dist := levenshtein(upper, strings.ToUpper(candidate))
		if dist > threshold {
			continue
		}
		switch {
		case !found || dist < bestDist:
			best, bestDist, found, tie = candidate, dist, true, false
		case dist == bestDist:
			tie = true
		}
	}
	if !found || tie {
		return "", false
	}
	return best, true
}

func nearestThreshold(length int) int {
	switch {
	case length <= 4:
		return 1
	case length <= 8:
		return 2
	default:
		return length / 4
	}
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			deletion := prev[j] + 1
			insertion := curr[j-1] + 1
			substitution := prev[j-1] + cost
			m := deletion
			if insertion < m {
				m = insertion
			}
			if substitution < m {
				m = substitution
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
