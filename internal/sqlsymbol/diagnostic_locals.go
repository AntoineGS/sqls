package sqlsymbol

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sqls-server/sqls/token"
)

// codeUnknownVariable marks a bare procedural identifier, or an INTO/
// RETURNING_VALUES target, whose role provably must be a local variable or
// parameter but that does not match any declaration in scope.
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

// procedureUnknownVariableFindings scans every name token owned by a
// procedure (a.procedureAt >= 0) and flags two provable variable-role
// positions the existing binder (resolve.go) does not already resolve to a
// declared Symbol: a bare identifier in procedural (non-SQL) context --
// reached only through an assignment lvalue ("name =") or a read already
// inside an IF/WHILE/FOR/CASE condition, per buildContexts' own state
// machine -- and a SELECT ... INTO / EXECUTE ... RETURNING_VALUES target,
// which is always a local/parameter by InterBase's own grammar.
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
		resolution := a.Resolve(item.Span.Start)
		unknown := (ctx.kind == contextProcedure && resolution.Role == Other) ||
			(ctx.outputTarget && resolution.Role == Column)
		if !unknown {
			continue
		}
		name, ok := nameFromLexeme(a.Text, item)
		if !ok {
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
// list, so their content never produces a finding.
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
//	  BEGIN ... END
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

// unknownVariableFindings conservatively scans the trigger body for the same
// two provable variable-role shapes as procedureUnknownVariableFindings: a
// bare assignment lvalue ("name ="), and a bare read inside an IF/WHILE
// condition's parentheses. NEW/OLD and any name qualified by "." (including
// the column half of NEW.col/OLD.col) are always skipped: this file does not
// enforce column existence or event-appropriateness for NEW/OLD, only that
// they are never mistaken for an unbound local.
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

	i := trg.bodyStart + 1
	for i < trg.bodyEnd {
		item := items[i]
		switch {
		case isNameToken(item) && i+1 < trg.bodyEnd && items[i+1].Token.Kind == token.Eq:
			report(i)
			i++
		case (isWord(item, "IF") || isWord(item, "WHILE")) && i+1 < trg.bodyEnd && items[i+1].Token.Kind == token.LParen:
			depth := 0
			j := i + 1
			for ; j < trg.bodyEnd; j++ {
				switch items[j].Token.Kind {
				case token.LParen:
					depth++
				case token.RParen:
					depth--
				}
				if depth == 0 {
					break
				}
			}
			for k := i + 2; k < j; k++ {
				report(k)
			}
			i = j + 1
		default:
			i++
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
