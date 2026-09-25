package sqlsymbol

import (
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

// codeUnknownRelation marks a name in a relation position (FROM/JOIN/UPDATE
// target/INSERT INTO target/DELETE FROM target) that does not match any
// known table, view, or procedure, in a fully known namespace.
const codeUnknownRelation = "interbase-unknown-relation"

// codeUnknownColumn marks a qualified or unqualified column reference that
// does not match any column of any relation it could plausibly belong to.
const codeUnknownColumn = "interbase-unknown-column"

// codeUnknownQualifier marks a qualifier (alias.column) that does not match
// any relation alias or table name in the applicable, fully known scope.
const codeUnknownQualifier = "interbase-unknown-qualifier"

// codeAmbiguousColumn marks an unqualified column reference matched by more
// than one relation in the innermost, fully known scope level.
const codeAmbiguousColumn = "interbase-ambiguous-column"

// nameFindings reports unknown relations, unknown/ambiguous columns, and
// unknown qualifiers across every modeled SQL position: standalone and
// embedded statements, CTEs, derived tables, selectable procedures in FROM,
// correlated subqueries (via RelationScope's scope chain), and each UNION
// arm independently. It requires c to implement SemanticCatalog (see
// diagnosticModel.semantic); when the caller's Catalog does not, no findings
// are produced at all here, rather than falling back to weaker Columns()-only
// behavior -- a plain Catalog cannot distinguish "not yet loaded" from
// "proven absent", and every rule below depends on that distinction to avoid
// speculative findings.
func (m *diagnosticModel) nameFindings() []Finding {
	if m == nil || m.semantic == nil || m.analysis == nil {
		return nil
	}
	var findings []Finding
	findings = append(findings, m.unknownRelationFindings()...)
	findings = append(findings, m.columnFindings(planClauseExclusions(m.items))...)
	findings = append(findings, m.triggerRowColumnFindings()...)
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Span.Start < findings[j].Span.Start })
	return findings
}

// syntaxOnlyKeywords supplements the dialect's own token.SQLWord.Kind
// classification (isReservedWordToken's primary source) with InterBase
// clause syntax the lexer does not yet classify as a keyword at all:
// FIRST/SKIP (SELECT FIRST n SKIP m), NULLS/LAST (ORDER BY ... NULLS
// FIRST/LAST), NEXT (NEXT VALUE FOR generator), and STARTING (STARTING
// WITH ...). These are kept local to this file rather than added to the
// dialect package's keyword tables, since those tables also drive
// completion suggestions and rename's reserved-word validity -- concerns
// this task does not want to affect.
var syntaxOnlyKeywords = map[string]bool{
	"FIRST": true, "SKIP": true, "NULLS": true, "LAST": true,
	"NEXT": true, "STARTING": true,
}

// isReservedWordToken reports whether item is a token the dialect's own
// keyword table already recognizes as some kind of SQL keyword (Kind !=
// dialect.Unmatched), or one of syntaxOnlyKeywords above -- the generic
// form of isWord()'s single-word check: "is this token ANY keyword the
// lexer already knows about", not "is this token exactly one specific
// word". This covers clause syntax (DISTINCT, BETWEEN, ROWS, TO, PLAN,
// NATURAL, CONTAINING, COLLATE, CHARACTER, ...) and context values
// (CURRENT_DATE, CURRENT_USER, USER, ...) without this file hand-
// maintaining its own duplicate word list for each of them. A quoted word
// is never treated as a keyword, matching isNameToken's own rule: quoting
// always means "this is a genuine identifier".
func isReservedWordToken(item lexeme) bool {
	if item.Token == nil || item.Token.Kind != token.SQLKeyword {
		return false
	}
	word, ok := item.Token.Value.(*token.SQLWord)
	if !ok || word.QuoteStyle != 0 {
		return false
	}
	return word.Kind != dialect.Unmatched || syntaxOnlyKeywords[word.Keyword]
}

// isNonColumnIdentifierPosition reports whether item i is a genuine
// identifier used in a position that names something other than a table
// column or relation: an InterBase generator (sequence) name -- the sole
// argument of GEN_ID(...), or the name following NEXT VALUE FOR -- or a
// character set or collation name following CHARACTER SET / COLLATE.
// SemanticCatalog does not model any of these namespaces (RelationInfo/
// ProcedureInfo/DomainInfo cover relations, procedures, and domains only),
// so rather than guessing, every position recognized here is withheld
// from column checking entirely.
func isNonColumnIdentifierPosition(items []lexeme, i int) bool {
	if i >= 2 && items[i-1].Token.Kind == token.LParen && isWord(items[i-2], "GEN_ID") {
		return true
	}
	if i >= 3 && isWord(items[i-1], "FOR") && isWord(items[i-2], "VALUE") && isWord(items[i-3], "NEXT") {
		return true
	}
	if i >= 2 && isWord(items[i-1], "SET") && isWord(items[i-2], "CHARACTER") {
		return true
	}
	if i >= 1 && isWord(items[i-1], "COLLATE") {
		return true
	}
	return false
}

// planClauseExclusions marks every item position inside a PLAN (...) or
// PLAN JOIN (...) clause's parentheses. PLAN is InterBase's query-plan
// optimizer hint: the identifiers inside it name relations/aliases/indexes
// in that mini-language's own terms (for example PLAN (T NATURAL) or PLAN
// JOIN (A NATURAL, B INDEX IX1)), not column expressions -- a relation
// alias appearing there, such as T above, would otherwise be checked (and
// wrongly flagged) as a bare column candidate. The whole clause is
// excluded wholesale rather than modeled, consistent with this task's
// "narrow to what's provable" bias.
func planClauseExclusions(items []lexeme) map[int]bool {
	excluded := make(map[int]bool)
	for i := 0; i < len(items); i++ {
		if !isWord(items[i], "PLAN") {
			continue
		}
		j := i + 1
		if j < len(items) && isWord(items[j], "JOIN") {
			j++
		}
		if j >= len(items) || items[j].Token.Kind != token.LParen {
			continue
		}
		depth := 0
		for k := j; k < len(items); k++ {
			switch items[k].Token.Kind {
			case token.LParen:
				depth++
			case token.RParen:
				depth--
			}
			excluded[k] = true
			if depth == 0 {
				break
			}
		}
	}
	return excluded
}

// --- unknown relation ---

// unknownRelationFindings checks every relation named in a query's own
// modeled FROM/JOIN/UPDATE-target/INSERT-target/DELETE-target list --
// RelationCandidates(qi), the same query-boundary-aware scan RelationScope
// itself is built from (sql.go's queryRelations/relationAt via
// queryRelationDetails) -- against SemanticCatalog.RelationInfo. This
// deliberately does not scan for "any name immediately after a FROM
// keyword" the way a.contexts[i].relation (resolve.go's markSQLPositions)
// does: that marker cannot distinguish a query's own FROM clause from FROM
// used as function-call syntax, such as EXTRACT(YEAR FROM col) or TRIM(...
// FROM col), and would otherwise check a function argument as if it were a
// table name. A name that resolves to a WITH/CTE binding in scope is a
// model-level identity, never a catalog lookup, so it is excluded before
// consulting the catalog; an unaliased FROM-clause procedure call has no
// RelationRef.Name of its own (relationAt's callable form deliberately
// drops it, the same as a derived table), so its real name is recovered
// from procedureNameFor before the catalog lookup, rather than skipped as
// if it had nothing to check.
func (m *diagnosticModel) unknownRelationFindings() []Finding {
	var findings []Finding
	for qi, q := range m.queries {
		if q.malformedProjection {
			continue
		}
		for _, detail := range m.RelationCandidates(qi) {
			if m.Unsupported(detail.start) {
				continue
			}
			name := detail.ref.Name
			if name.Key() == "" {
				procName, ok := m.procedureNameFor[relationSourceKey{owner: qi, source: detail.start}]
				if !ok {
					continue // a derived table: nothing to check by name
				}
				name = procName
			}
			if m.DDLInvalidated(detail.start, name) {
				// Named by an earlier CREATE/ALTER/DROP in this same
				// script: the live catalog's answer (present or absent)
				// predates that statement and is unsafe to trust from
				// here on.
				continue
			}
			if _, isCTE := m.cteFor(qi, name.Key()); isCTE {
				continue
			}
			if _, knowledge := m.semantic.RelationInfo(name); knowledge == Missing {
				findings = append(findings, Finding{
					Span:     m.items[detail.start].Span,
					Code:     codeUnknownRelation,
					Message:  fmt.Sprintf("%s is not a known table, view, or procedure", name.Text),
					Severity: 1,
				})
			}
		}
	}
	return findings
}

// --- unknown/ambiguous column, unknown qualifier ---

// columnFindings walks every name token belonging to a modeled SQL query
// once (any position with a non-negative m.queryAt, the same width this
// model's other rules use -- this deliberately does not gate on
// a.contexts[i].kind == contextSQL, since the shared binder (resolve.go)
// classifies a whole recognized WITH statement or CREATE TRIGGER body as
// contextUnsupported for its own unrelated reasons, the same caveat
// m.Unsupported's own doc comment describes for WITH; m.Unsupported itself
// correctly narrows that back down to true unsupported regions), classifying
// each as a relation/alias declaration (skipped: not a column reference), a
// qualified column reference (its qualifier resolved first, job 3, then its
// column checked against that one relation, job 2), or a candidate
// unqualified column reference (job 2/4). NEW/OLD trigger row references are
// excluded here and handled uniformly by triggerRowColumnFindings instead,
// since NEW/OLD are per-row aliases for the trigger's FOR-relation, not a
// FROM-clause relation RelationScope can resolve.
func (m *diagnosticModel) columnFindings(excludedPositions map[int]bool) []Finding {
	a := m.analysis
	var findings []Finding
	for i, item := range m.items {
		if !isNameToken(item) {
			continue
		}
		qi := -1
		if i < len(m.queryAt) {
			qi = m.queryAt[i]
		}
		if qi < 0 {
			continue
		}
		if qi < len(m.queries) && m.queries[qi].malformedProjection {
			// This query's own projection list did not parse cleanly (a
			// genuinely empty comma-separated segment, e.g. a trailing
			// comma right before FROM): the whole statement must not be
			// trusted for speculative missing-name errors.
			continue
		}
		if m.Unsupported(i) {
			continue
		}
		if i < len(a.contexts) && (a.contexts[i].relation || a.contexts[i].alias) {
			continue
		}
		if m.relationDeclPositions[i] {
			continue
		}
		if excludedPositions[i] {
			continue // inside a PLAN (...) clause: not a column position at all
		}
		if isReservedWordToken(item) {
			continue // SQL keyword/clause syntax, never a column candidate
		}
		if isNonColumnIdentifierPosition(m.items, i) {
			continue // a generator, character set, or collation name
		}
		if i > 0 && isWord(m.items[i-1], "AS") {
			continue // a column or relation alias declaration, never a reference
		}
		if i > 0 && m.items[i-1].Token.Kind == token.Colon {
			continue // a local-variable reference, never a column
		}
		precededByPeriod := i > 0 && m.items[i-1].Token.Kind == token.Period
		followedByPeriod := i+1 < len(m.items) && m.items[i+1].Token.Kind == token.Period
		if precededByPeriod {
			findings = append(findings, m.qualifiedColumnFindings(i, qi)...)
			continue
		}
		if followedByPeriod {
			// The qualifier half of a qualified reference: validated
			// together with its column above, at the name-after-period
			// position.
			continue
		}
		if isCallable(m.items, i) || isProcedureCallName(m.items, i) {
			continue
		}
		name, ok := nameFromLexeme(a.Text, item)
		if !ok {
			continue
		}
		findings = append(findings, m.unqualifiedColumnFindings(i, qi, name)...)
	}
	return findings
}

// qualifiedColumnFindings resolves an "alias.column" or "table.column"
// reference at item position i (the column name; i-1 is the period, i-2 the
// qualifier). It searches RelationScope's whole chain, innermost first, so
// a correlated subquery's qualifier can resolve to an enclosing query's
// relation -- but stops at the first scope level offering any match, so a
// qualifier is never silently rebound past a closer, shadowing relation.
func (m *diagnosticModel) qualifiedColumnFindings(i, qi int) []Finding {
	a := m.analysis
	qualifier := nameBeforePeriod(a.Text, m.items, i-1)
	if qualifier == nil {
		return nil
	}
	if isTriggerRowAlias(*qualifier) {
		return nil // NEW/OLD: handled by triggerRowColumnFindings instead.
	}
	name, ok := nameFromLexeme(a.Text, m.items[i])
	if !ok {
		return nil
	}
	chain := m.RelationScope(i)
	qiChain := m.queryChain(i)
	ref, found, ambiguous := m.resolveQualifier(chain, qiChain, *qualifier)
	if ambiguous {
		// More than one relation in the same scope level shares this
		// alias/name (for example a duplicate alias): the qualifier cannot
		// be safely resolved to one relation, so neither an unknown-column
		// nor an unknown-qualifier claim can be proven. See
		// task-4-report.md for this judgment call.
		return nil
	}
	if !found {
		if hasAnyRelation(chain) {
			return []Finding{{
				Span:     m.items[i-2].Span,
				Code:     codeUnknownQualifier,
				Message:  fmt.Sprintf("%s does not match any relation alias or table in scope", qualifier.Text),
				Severity: 1,
			}}
		}
		return nil
	}
	cols, ok := m.RelationOutput(i, ref)
	if !ok {
		return nil // that relation's columns are not provably complete
	}
	for _, col := range cols {
		if name.MatchesCatalogName(col.Name) {
			return nil
		}
	}
	return []Finding{{
		Span:     m.items[i].Span,
		Code:     codeUnknownColumn,
		Message:  fmt.Sprintf("%s is not a column of %s", name.Text, qualifier.Text),
		Severity: 1,
	}}
}

// unqualifiedColumnFindings checks a candidate bare column name against the
// innermost RelationScope level (job 2/4): more than one relation there
// offering the name is ambiguous (job 4), unless the query joins with
// USING/NATURAL, whose merged shared column must never be flagged ambiguous
// (job 5; see queryHasMergingJoin). A name absent from every innermost
// relation is checked against each outer scope level in turn before being
// reported unknown, so a legitimate unqualified correlated reference is
// never flagged; any relation along the way with unprovable columns
// withholds the finding entirely, in both directions, per job 4's
// direction.
func (m *diagnosticModel) unqualifiedColumnFindings(i, qi int, name Name) []Finding {
	if m.aliasSuppressesColumnCheck(qi, i, name) {
		return nil
	}
	chain := m.RelationScope(i)
	if len(chain) == 0 || len(chain[0]) == 0 {
		return nil
	}
	matches, allKnown := m.columnMatchesInLevel(i, chain[0], name)
	if !allKnown {
		return nil
	}
	if matches > 1 {
		if m.queryHasMergingJoin(qi) {
			return nil
		}
		return []Finding{{
			Span:     m.items[i].Span,
			Code:     codeAmbiguousColumn,
			Message:  fmt.Sprintf("%s matches more than one relation in scope", name.Text),
			Severity: 1,
		}}
	}
	if matches == 1 {
		return nil
	}
	for _, level := range chain[1:] {
		outerMatches, outerKnown := m.columnMatchesInLevel(i, level, name)
		if !outerKnown {
			return nil
		}
		if outerMatches > 0 {
			return nil // resolves via correlation to an enclosing query
		}
	}
	if m.matchesDeclaredLocal(i, name) {
		// A bare, unqualified SQL-context name that happens to share a
		// declared local variable/parameter's spelling: withhold, per this
		// plan's deliberate conservative bias (see matchesDeclaredLocal).
		return nil
	}
	return []Finding{{
		Span:     m.items[i].Span,
		Code:     codeUnknownColumn,
		Message:  fmt.Sprintf("%s is not a column of any relation in scope", name.Text),
		Severity: 1,
	}}
}

// matchesDeclaredLocal reports whether name matches a declared local
// variable or parameter in the enclosing procedure or trigger scope at
// item position i, reusing the existing local-symbol machinery (symbolFor
// for procedures, triggerModel.symbolFor for triggers) rather than
// reimplementing declaration lookup. Real InterBase requires a ':' prefix
// to actually reference a local in SQL context, so this rule does not
// attempt to prove which reading (column vs local) is intended -- only
// that a plausible non-error reading exists, and withholds accordingly.
func (m *diagnosticModel) matchesDeclaredLocal(i int, name Name) bool {
	a := m.analysis
	if i < len(a.procedureAt) {
		if procIndex := a.procedureAt[i]; procIndex >= 0 {
			if symbol, _ := symbolFor(a, procIndex, name); symbol != nil {
				return true
			}
		}
	}
	for _, trg := range discoverTriggers(a.Text, m.items) {
		if i < trg.bodyStart || i >= trg.bodyEnd {
			continue
		}
		if sym, _ := trg.symbolFor(name); sym != nil {
			return true
		}
	}
	return false
}

// columnMatchesInLevel counts how many relations in level expose a column
// named name, using RelationOutput at position i for each. allKnown is
// false as soon as any relation's output cannot be proven complete, so a
// caller never draws an absence or ambiguity conclusion from a partial scan.
func (m *diagnosticModel) columnMatchesInLevel(i int, level []RelationRef, name Name) (matches int, allKnown bool) {
	allKnown = true
	for _, ref := range level {
		cols, ok := m.RelationOutput(i, ref)
		if !ok {
			allKnown = false
			continue
		}
		for _, col := range cols {
			if name.MatchesCatalogName(col.Name) {
				matches++
			}
		}
	}
	return matches, allKnown
}

// resolveQualifier searches chain (innermost first) for the one relation
// qualifier names, matching RelationRef.Alias first and RelationRef.Name
// otherwise (relationMatchesQualifier), plus one InterBase-specific case
// neither of those covers: an unaliased FROM-clause procedure call, whose
// RelationRef has neither a Name nor an Alias of its own (relationAt's
// callable form deliberately drops the literal name, the same as a
// derived table, since a FROM-clause callable's RelationRef identity is
// alias-only) but is still implicitly qualifiable by its own procedure
// name in InterBase, the same as an unaliased table is implicitly
// qualifiable by its own table name -- recovered here from
// procedureNameFor. qiChain must be queryChain(i), RelationScope(i)'s own
// query-index chain in the same order, so each level's procedureNameFor
// lookup is keyed by the right owning query. It stops at the first scope
// level offering any match: ambiguous is true when that level has more
// than one, found is true when it has exactly one.
func (m *diagnosticModel) resolveQualifier(chain [][]RelationRef, qiChain []int, qualifier Name) (ref RelationRef, found, ambiguous bool) {
	for level, relations := range chain {
		qi := -1
		if level < len(qiChain) {
			qi = qiChain[level]
		}
		matches := 0
		var candidate RelationRef
		for ri, r := range relations {
			if qi >= 0 && ri < len(m.relationPositions[qi]) {
				r.source, r.sourceKnown = m.relationPositions[qi][ri].start, true
			}
			if relationMatchesQualifier(r, qualifier) {
				matches++
				candidate = r
				continue
			}
			if r.Name.Key() == "" && r.Alias == nil && qi >= 0 {
				if procName, ok := m.procedureNameFor[m.relationSourceKey(qi, ri)]; ok && procName.Key() == qualifier.Key() {
					matches++
					candidate = r
				}
			}
		}
		if matches == 1 {
			return candidate, true, false
		}
		if matches > 1 {
			return RelationRef{}, false, true
		}
	}
	return RelationRef{}, false, false
}

func hasAnyRelation(chain [][]RelationRef) bool {
	for _, level := range chain {
		if len(level) > 0 {
			return true
		}
	}
	return false
}

// queryHasMergingJoin reports whether qi's own query text contains a JOIN
// ... USING(...) or NATURAL JOIN. queryRelations (sql.go) does not track
// which columns such a join merges/deduplicates -- it models relation
// identity, not column shape -- so this task's safe fallback (job 5) is to
// suppress ambiguous-column reporting for the whole query rather than model
// USING/NATURAL column merging itself. This can only withhold a true
// ambiguous-column finding, never fabricate one, and does not affect
// unknown-column detection (a USING/NATURAL-merged column still genuinely
// exists on both relations, so its existence check is unaffected).
func (m *diagnosticModel) queryHasMergingJoin(qi int) bool {
	if qi < 0 || qi >= len(m.queries) {
		return false
	}
	q := m.queries[qi]
	for idx := q.start; idx < q.end; idx++ {
		if isWord(m.items[idx], "NATURAL") || isWord(m.items[idx], "USING") {
			return true
		}
	}
	return false
}

// aliasSuppressesColumnCheck implements this task's InterBase select-list
// alias-visibility assumption, applied only to a SELECT query's own GROUP
// BY, HAVING, and ORDER BY clauses: WHERE is excluded, since every SQL
// dialect's logical query processing order evaluates WHERE before the
// SELECT list's aliases exist (high confidence, not merely a conservative
// default). GROUP BY/HAVING/ORDER BY referencing a SELECT-list alias is a
// common relaxation across SQL dialects, including Firebird/InterBase, but
// this task is not fully certain of InterBase's precise support in each of
// those three clauses; per the task brief's conservative-default guidance,
// a name in one of those clauses that is not provably absent from the
// query's own output is never reported unknown/ambiguous, whether it
// matches an alias or the output shape itself cannot be proven. See
// task-4-report.md for the full reasoning.
func (m *diagnosticModel) aliasSuppressesColumnCheck(qi, i int, name Name) bool {
	if qi < 0 || qi >= len(m.queries) {
		return false
	}
	q := m.queries[qi]
	if q.kind != "SELECT" {
		return false
	}
	if i < q.targetEnd {
		return false // within the SELECT list itself: no forward-alias assumption
	}
	clause := m.clauseAt(qi, i)
	switch clause {
	case "GROUP", "HAVING", "ORDER":
	default:
		return false
	}
	output := q.output
	if clause == "ORDER" {
		// A trailing ORDER BY conventionally applies to a UNION's whole
		// merged result, not just the last arm it lexically trails, so an
		// alias declared in an earlier arm (SELECT ID AS K FROM T UNION
		// SELECT ID FROM U ORDER BY K) must be checked against the
		// union's own merged output (UnionOutput, using the first arm's
		// column names per its own doc comment), not this arm's own
		// output alone.
		if unionOutput, ok := m.UnionOutput(i); ok {
			output = unionOutput
		} else if m.unionOf[qi] >= 0 {
			// Part of a union whose merged shape isn't provable (an arm's
			// own output is unknown, or arm column counts disagree): the
			// trailing ORDER BY could still be referencing another arm's
			// alias this arm's own output cannot see, so withhold rather
			// than checking only this arm's own shape.
			return true
		}
	}
	if !output.CountKnown {
		return true // cannot rule out a matching SELECT-list alias: withhold
	}
	for _, col := range output.Columns {
		if col.NameKnown && col.Name.Key() == name.Key() {
			return true
		}
	}
	return false
}

// clauseAt reports which clause of qi's own query contains item position i,
// by scanning qi's own query from the end of its target list (the shared
// diagnosticModel boundary computeSelectOutput already records) up to i,
// tracking top-level clause keywords the same way singletonClauses does.
// Only called for SELECT queries with a valid targetEnd; see
// aliasSuppressesColumnCheck.
func (m *diagnosticModel) clauseAt(qi, i int) string {
	q := m.queries[qi]
	if i < q.targetEnd || i >= q.end {
		return ""
	}
	depth := q.baseDepth
	clause := "FROM"
	for idx := q.targetEnd; idx < i; idx++ {
		switch m.items[idx].Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
		if depth != q.baseDepth {
			continue
		}
		switch {
		case isWord(m.items[idx], "WHERE"):
			clause = "WHERE"
		case isWord(m.items[idx], "GROUP"):
			clause = "GROUP"
		case isWord(m.items[idx], "HAVING"):
			clause = "HAVING"
		case isWord(m.items[idx], "ORDER"):
			clause = "ORDER"
		case isWord(m.items[idx], "UNION"), isWord(m.items[idx], "RETURNING"),
			isWord(m.items[idx], "SET"), isWord(m.items[idx], "VALUES"):
			clause = "OTHER"
		}
	}
	return clause
}

// --- trigger NEW/OLD row column existence ---

// isTriggerRowAlias reports whether name is the unquoted NEW or OLD trigger
// row alias. A quoted "NEW"/"OLD" identifier names a genuinely different
// (case-sensitive) object, never the trigger row alias.
func isTriggerRowAlias(name Name) bool {
	if name.Quoted {
		return false
	}
	key := name.Key()
	return key == "NEW" || key == "OLD"
}

// triggerRowColumnFindings checks every NEW.<col>/OLD.<col> reference in
// each discovered trigger body against that trigger's own FOR-relation
// columns. Task 3 (diagnostic_locals.go) discovers and stores the
// FOR-relation Name but does not check it; this is that deferred check. It
// scans independently of columnFindings' SQL/procedural split, since
// NEW/OLD are per-row aliases valid in both a trigger's procedural
// statements (an assignment target, an IF condition) and its embedded SQL
// (an INSERT ... VALUES(NEW.col)), not a FROM-clause relation RelationScope
// can resolve.
func (m *diagnosticModel) triggerRowColumnFindings() []Finding {
	a := m.analysis
	var findings []Finding
	for _, trg := range discoverTriggers(a.Text, m.items) {
		ctx := newTriggerBodyContext(m.items, trg.bodyStart, trg.bodyEnd)
		for idx := trg.bodyStart; idx < trg.bodyEnd; idx++ {
			if !isWord(m.items[idx], "NEW") && !isWord(m.items[idx], "OLD") {
				continue
			}
			if idx+2 >= trg.bodyEnd || m.items[idx+1].Token.Kind != token.Period || !isNameToken(m.items[idx+2]) {
				continue
			}
			// m.Unsupported now correctly exempts a recognized trigger
			// body's own span (recognizedTriggerBodies, populated by
			// buildTriggerQueries) from the shared binder's whole-
			// statement contextUnsupported classification, the same way
			// it already exempted a recognized WITH statement's body.
			if m.Unsupported(idx) || ctx.unsupported[idx-trg.bodyStart] {
				continue
			}
			if m.DDLInvalidated(idx, trg.relation) {
				continue
			}
			colName, ok := nameFromLexeme(a.Text, m.items[idx+2])
			if !ok {
				continue
			}
			fact, knowledge := m.semantic.RelationInfo(trg.relation)
			if knowledge != Present || !fact.ColumnsKnown {
				continue
			}
			found := false
			for _, col := range fact.Columns {
				if colName.MatchesCatalogName(col.Name) {
					found = true
					break
				}
			}
			if !found {
				findings = append(findings, Finding{
					Span:     m.items[idx+2].Span,
					Code:     codeUnknownColumn,
					Message:  fmt.Sprintf("%s is not a column of %s", colName.Text, trg.relation.Text),
					Severity: 1,
				})
			}
		}
	}
	return findings
}
