package sqlsymbol

import (
	"sort"
	"strings"

	"github.com/sqls-server/sqls/token"
)

// diagnosticModel is the semantic-scope index built once per diagnostic run.
// Rule methods (arity, unknown-name, type, and flow checks) consume it
// instead of rescanning the document themselves. It extends the navigation
// binder's relation tracking (sql.go, resolve.go) with concepts navigation
// does not need: WITH/CTE names, per-query output (projected column) shape,
// UNION shape merging, DDL-invalidated identity, and unsupported-region
// membership. Every position parameter below is an index into items, the
// document's significant (whitespace/comment-stripped) lexeme stream -- the
// same indexing width.go, assignments.go, and singleton.go already use.
type diagnosticModel struct {
	analysis *Analysis
	catalog  Catalog
	semantic SemanticCatalog // nil when catalog does not implement SemanticCatalog
	items    []lexeme

	statements []modelStatement

	queries      []modelQuery
	queryAt      []int // per item index: owning query index in queries, or -1
	queryByStart map[int]int

	derivedFor       map[relationSourceKey]int  // owning query + alias -> the derived table's own inner query index
	procedureNameFor map[relationSourceKey]Name // owning query + alias -> the FROM-clause callable's procedure name
	cteColumns       map[int][]Name             // CTE query index -> its explicit WITH-column-list names, in order

	// relationDeclPositions marks every item index that is part of a
	// relation reference's own name, call-arguments, or alias within a
	// FROM clause -- never a column value reference in that relation's
	// owning query. a.contexts' relation/alias flags (resolve.go's
	// markSQLPositions) only cover a single name token directly after
	// FROM/JOIN/UPDATE/INSERT INTO/DELETE FROM; they do not see a
	// comma-continued relation in a multi-table FROM list, nor a derived
	// table's or FROM-clause callable's alias (whose relation "name" is a
	// parenthesized construct, not a name token markSQLPositions can
	// anchor to). This is a second, independent source of the same
	// concept, built directly from the already-parsed RelationRef list.
	relationDeclPositions map[int]bool

	// relationPositions maps a query index to its own q.relations, paired
	// 1:1 with the item index where each relation reference's own text
	// begins (queryRelationDetails' scan) -- the actual, query-boundary-
	// aware candidate list unknown-relation checking uses, rather than a
	// naive "name immediately after any FROM keyword" scan that cannot
	// distinguish a query's own FROM clause from FROM used as function-
	// call syntax (for example EXTRACT(YEAR FROM col) or TRIM(... FROM
	// col)).
	relationPositions map[int][]relationDetail

	unionOf     []int   // per query index: the union-chain group it belongs to, or -1
	unionGroups [][]int // group id -> ordered arm query indices

	recognizedWith []itemRange // WITH statements successfully modeled (no nested SELECT in any CTE body or the final query)
	malformed      []itemRange // statement regions statement recovery had to skip over

	// recognizedTriggerBodies is each discovered trigger's own [bodyStart,
	// bodyEnd) span (a trigger the shared binder's whole-statement
	// contextUnsupported classification would otherwise hide from
	// Unsupported-gated rules, the same reason recognizedWith exists for
	// WITH statements above). See Unsupported's own doc comment.
	recognizedTriggerBodies []itemRange

	ddl []modelDDL // CREATE/ALTER/DROP object identities, in document order
}

// itemRange is a half-open range of item indices, distinct from the
// byte-offset Span used elsewhere in this package.
type itemRange struct {
	start, end int
}

// modelStatement is one candidate top-level statement's item-index span.
// Start/End are item indices (not byte offsets); End is the terminating ";"
// index, or len(items) for a final statement with no terminator. Malformed
// statements are still reported (so callers can see where recovery
// happened) but their internal shape was not modeled.
type modelStatement struct {
	Start, End int
	Malformed  bool
}

// modelOutputColumn is one column of a query's projected output. NameKnown
// is false for an unaliased expression: the column exists (it still counts
// toward the output's shape) but its name cannot be claimed.
type modelOutputColumn struct {
	Name      Name
	NameKnown bool
}

// modelOutput is a query's projected output shape. CountKnown is false for
// an unprovable projection (for example SELECT * over zero, multiple, or
// unknown-column relations); callers must not draw conclusions about column
// count or identity when CountKnown is false.
type modelOutput struct {
	Columns    []modelOutputColumn
	CountKnown bool
}

type modelQuery struct {
	start, end     int
	baseDepth      int
	parent         int // index into queries, or -1
	kind           string
	relations      []RelationRef
	output         modelOutput
	targetStart    int // item-index bounds of this query's own projection (target) list
	targetEnd      int
	cteName        string // non-empty (Name.Key()) when this query is a CTE body
	statementIndex int

	// malformedProjection is true when this query's own projection list
	// contains a genuinely empty comma-separated segment (for example a
	// trailing comma right before FROM, or two consecutive commas) --
	// computeProjection's own signal that the SELECT list itself did not
	// parse cleanly, distinct from the many other reasons a query's output
	// can be legitimately CountKnown=false (an unprovable star projection
	// over zero, multiple, or unknown-column relations). Name-checking
	// rules withhold every finding for such a query: a statement whose own
	// projection list is this broken should not be trusted for anything.
	malformedProjection bool
}

type modelDDL struct {
	key string // Name.Key() of the object CREATE/ALTER/DROP named
	end int    // item index one past the DDL statement's own content
}

type relationSourceKey struct {
	owner int // owning query index
	alias string
}

// diagnosticModel builds the semantic-scope model for this analysis. c
// supplies relation/procedure metadata; when c also implements
// SemanticCatalog, RelationOutput can use completeness-aware facts (known
// procedure outputs, proven-absent objects) instead of only Catalog.Columns.
func (a *Analysis) diagnosticModel(c Catalog) *diagnosticModel {
	m := &diagnosticModel{analysis: a, catalog: c}
	if semantic, ok := c.(SemanticCatalog); ok {
		m.semantic = semantic
	}
	if a == nil || len(a.lexemes) == 0 {
		return m
	}
	items := significantLexemes(a.lexemes)
	m.items = items
	if len(items) == 0 {
		return m
	}
	depths, matching := sqlDepths(items)
	boundaryAfter := boundaryAfterItems(items, scriptDelimiterOffsets(a.Text))
	m.statements = splitStatements(items, boundaryAfter)

	raw := discoverSQLQueries(items, a.contexts, depths)
	for i := range raw {
		raw[i].relations, raw[i].target, raw[i].hasTarget = queryRelations(a.Text, items, raw[i], depths, matching)
	}
	for _, q := range raw {
		m.queries = append(m.queries, modelQuery{
			start: q.start, end: q.end, parent: q.parent, kind: q.kind,
			baseDepth: q.baseDepth, relations: q.relations,
		})
	}

	for stmtIndex, stmt := range m.statements {
		if stmt.Malformed || stmt.Start >= stmt.End || !isWord(items[stmt.Start], "WITH") {
			continue
		}
		if m.buildWithStatement(a.Text, items, depths, matching, stmt) {
			m.recognizedWith = append(m.recognizedWith, itemRange{stmt.Start, stmt.End})
		}
		_ = stmtIndex
	}

	for qi := range m.queries {
		m.queries[qi].statementIndex = statementIndexAt(m.statements, m.queries[qi].start)
	}

	m.queryByStart = make(map[int]int, len(m.queries))
	for qi, q := range m.queries {
		m.queryByStart[q.start] = qi
	}

	m.buildQueryAt(len(items))
	m.connectRelationSources(a.Text, items, depths, matching)
	m.computeRelationPositions(a.Text, items, depths, matching)
	// DDL detection must run before computeOutputs: expandStar resolves
	// through resolveRelationOutput, which checks DDLInvalidated while
	// computing a query's own output shape, not only when a caller later
	// asks for it.
	m.ddl = detectDDL(a.Text, items, m.statements)
	m.computeOutputs(a.Text)
	// buildTriggerQueries runs after the ordinary top-level pass above has
	// already built m.queryAt/m.queryByStart/m.statements/m.ddl, since a
	// trigger-embedded query's own relation checks (DDLInvalidated, CTE
	// shadowing) depend on that state already being in place; it updates
	// those same structures directly for its own additions rather than
	// triggering a second top-level pass.
	m.buildTriggerQueries(a.Text, items, depths, matching)
	m.detectUnionGroups(items, depths)

	for _, stmt := range m.statements {
		if stmt.Malformed {
			m.malformed = append(m.malformed, itemRange{stmt.Start, stmt.End})
		}
	}
	return m
}

// Statements returns each candidate top-level statement's item-index span in
// document order. A Malformed statement's internal shape was not modeled,
// but later statements are still discovered and modeled independently.
func (m *diagnosticModel) Statements() []modelStatement {
	return append([]modelStatement(nil), m.statements...)
}

// RelationScope reports the chain of relations visible at item position i,
// innermost query first: the query directly enclosing i, then each
// correlating/enclosing query outward. It includes WITH/CTE names,
// derived-table aliases, and correlated-subquery scopes that the navigation
// binder's a.sqlScopes does not model.
//
// Caveat: a derived table's own body currently also has the outer query's
// scope appended past its innermost level, because the existing binder's
// parent-walk cannot distinguish "derived table body" from "correlated
// subquery" nesting. This is safe-direction-only -- it can only add an
// extra candidate relation and so suppress a finding, never fabricate a
// false "Present" resolution -- but the innermost level should not be
// assumed exhaustive proof of what a derived table's body can see. A future
// task may want to tag each scope level with its nesting kind to narrow this.
func (m *diagnosticModel) RelationScope(i int) [][]RelationRef {
	if i < 0 || i >= len(m.queryAt) {
		return nil
	}
	var chain [][]RelationRef
	for qi := m.queryAt[i]; qi >= 0; qi = m.queries[qi].parent {
		chain = append(chain, m.queries[qi].relations)
	}
	return chain
}

// TargetOutput reports the projected output shape of the query whose own
// projection (target) list contains item position i; ok is false when i is
// not part of a modeled query's target list.
func (m *diagnosticModel) TargetOutput(i int) (modelOutput, bool) {
	if i < 0 || i >= len(m.queryAt) {
		return modelOutput{}, false
	}
	qi := m.queryAt[i]
	if qi < 0 {
		return modelOutput{}, false
	}
	q := m.queries[qi]
	if i < q.targetStart || i >= q.targetEnd {
		return modelOutput{}, false
	}
	return q.output, true
}

// RelationOutput resolves the column list ref exposes at item position i: a
// real table/view's catalog columns, a known procedure's declared outputs
// (for a FROM-clause callable), a derived table's own projected output, or a
// CTE's computed output. known is false when that list cannot be proven
// complete, and callers must not treat that as evidence of absence.
func (m *diagnosticModel) RelationOutput(i int, ref RelationRef) ([]ColumnFact, bool) {
	if i < 0 || i >= len(m.queryAt) {
		return nil, false
	}
	for qi := m.queryAt[i]; qi >= 0; qi = m.queries[qi].parent {
		for _, candidate := range m.queries[qi].relations {
			if relationRefEqual(candidate, ref) {
				return m.resolveRelationOutput(i, qi, ref)
			}
		}
	}
	return nil, false
}

// UnionOutput reports the merged output shape for the UNION chain containing
// the query at item position i, using the first arm's column names. ok is
// false when i is not part of a modeled UNION chain, or when any arm's own
// output count is unknown or does not match the others.
func (m *diagnosticModel) UnionOutput(i int) (modelOutput, bool) {
	if i < 0 || i >= len(m.queryAt) {
		return modelOutput{}, false
	}
	qi := m.queryAt[i]
	if qi < 0 || m.unionOf[qi] < 0 {
		return modelOutput{}, false
	}
	arms := m.unionGroups[m.unionOf[qi]]
	if len(arms) == 0 {
		return modelOutput{}, false
	}
	first := m.queries[arms[0]].output
	if !first.CountKnown {
		return modelOutput{}, false
	}
	for _, arm := range arms[1:] {
		out := m.queries[arm].output
		if !out.CountKnown || len(out.Columns) != len(first.Columns) {
			return modelOutput{}, false
		}
	}
	return first, true
}

// DDLInvalidated reports whether name was named by an earlier CREATE/ALTER/
// DROP statement that has already fully ended before item position i in this
// analysis, making catalog-derived claims about it unsafe to trust from that
// point on. Positions within the DDL statement's own extent (its own object
// name, column list, or body) report false: invalidation begins with the
// next statement, not partway through the statement that causes it.
func (m *diagnosticModel) DDLInvalidated(i int, name Name) bool {
	key := name.Key()
	for _, d := range m.ddl {
		if d.key == key && i >= d.end {
			return true
		}
	}
	return false
}

// Unsupported reports whether item position i lies in a region the model
// deliberately did not analyze: MERGE, dynamic SQL, a recursive or
// otherwise-unrecognized WITH, or a malformed statement skipped by recovery.
// A recognized non-recursive WITH's own body and final query report false,
// even though the navigation binder still classifies that whole statement as
// unsupported for its own (unrelated) purposes -- and likewise a recognized
// CREATE TRIGGER body (parseTrigger validated its shape): resolve.go's
// buildContexts has no CREATE TRIGGER recognition at all, so it classifies
// a whole trigger statement contextUnsupported end to end, unrelated to
// whether this model actually understands its embedded SQL.
func (m *diagnosticModel) Unsupported(i int) bool {
	if i < 0 || i >= len(m.items) {
		return false
	}
	for _, r := range m.malformed {
		if i >= r.start && i < r.end {
			return true
		}
	}
	if m.analysis != nil && i < len(m.analysis.contexts) && m.analysis.contexts[i].kind == contextUnsupported {
		for _, r := range m.recognizedWith {
			if i >= r.start && i < r.end {
				return false
			}
		}
		for _, r := range m.recognizedTriggerBodies {
			if i >= r.start && i < r.end {
				return false
			}
		}
		return true
	}
	return false
}

// resolveRelationOutput resolves ref's column list as seen from item
// position at (used only to evaluate DDLInvalidated against that position;
// owner identifies the query that ref belongs to). CTE and derived-table
// outputs come from the model's own computed body shape and are unaffected
// by catalog DDL; real-table and procedure lookups check DDLInvalidated
// first and report unknown rather than trusting stale catalog data.
func (m *diagnosticModel) resolveRelationOutput(at, owner int, ref RelationRef) ([]ColumnFact, bool) {
	// A FROM-clause callable procedure and a derived table share the same
	// RelationRef shape (empty Name, alias-only): relationAt's callable
	// branch mirrors its LParen/derived-table branch. Check both keyed
	// lookups before falling back to a catalog name lookup.
	key := relationSourceKey{owner: owner, alias: aliasKeyOf(ref.Alias)}
	if inner, ok := m.derivedFor[key]; ok {
		return m.queryOutputColumns(inner)
	}
	if procName, ok := m.procedureNameFor[key]; ok {
		if m.DDLInvalidated(at, procName) {
			return nil, false
		}
		if m.semantic != nil {
			if fact, knowledge := m.semantic.ProcedureInfo(procName); knowledge == Present && fact.OutputsKnown {
				return fact.Outputs, true
			}
		}
		return nil, false
	}
	if ref.Name.Key() == "" {
		return nil, false
	}
	if inner, ok := m.cteFor(owner, ref.Name.Key()); ok {
		return m.queryOutputColumns(inner)
	}
	if m.DDLInvalidated(at, ref.Name) {
		return nil, false
	}
	if m.semantic != nil {
		if fact, knowledge := m.semantic.RelationInfo(ref.Name); knowledge == Present && fact.ColumnsKnown {
			return fact.Columns, true
		}
		if fact, knowledge := m.semantic.ProcedureInfo(ref.Name); knowledge == Present && fact.OutputsKnown {
			return fact.Outputs, true
		}
		return nil, false
	}
	if m.catalog != nil {
		if cols, ok := m.catalog.Columns(ref.Name); ok {
			out := make([]ColumnFact, len(cols))
			for i, c := range cols {
				out[i] = ColumnFact{Name: c.Name, Type: c.Type}
			}
			return out, true
		}
	}
	return nil, false
}

func (m *diagnosticModel) queryOutputColumns(qi int) ([]ColumnFact, bool) {
	if qi < 0 || qi >= len(m.queries) || !m.queries[qi].output.CountKnown {
		return nil, false
	}
	out := make([]ColumnFact, len(m.queries[qi].output.Columns))
	for i, col := range m.queries[qi].output.Columns {
		if !col.NameKnown {
			return nil, false
		}
		out[i] = ColumnFact{Name: col.Name.Key()}
	}
	return out, true
}

func (m *diagnosticModel) cteFor(owner int, nameKey string) (int, bool) {
	if owner < 0 || owner >= len(m.queries) {
		return -1, false
	}
	statementIndex := m.queries[owner].statementIndex
	for qi, q := range m.queries {
		if q.cteName == nameKey && q.statementIndex == statementIndex {
			return qi, true
		}
	}
	return -1, false
}

func relationRefEqual(a, b RelationRef) bool {
	if a.Name.Key() != b.Name.Key() {
		return false
	}
	return aliasKeyOf(a.Alias) == aliasKeyOf(b.Alias)
}

func aliasKeyOf(alias *Name) string {
	if alias == nil {
		return ""
	}
	return alias.Key()
}

func (m *diagnosticModel) buildQueryAt(n int) {
	m.queryAt = make([]int, n)
	for i := range m.queryAt {
		m.queryAt[i] = -1
	}
	for qi, q := range m.queries {
		if q.start < 0 || q.end > n || q.start >= q.end {
			continue
		}
		for i := q.start; i < q.end; i++ {
			if m.queryAt[i] < 0 || (m.queries[m.queryAt[i]].end-m.queries[m.queryAt[i]].start) > (q.end-q.start) {
				m.queryAt[i] = qi
			}
		}
	}
}

func statementIndexAt(statements []modelStatement, pos int) int {
	for i, s := range statements {
		if pos >= s.Start && pos < s.End {
			return i
		}
	}
	return -1
}

// boundaryAfterItems marks, for each item index, whether a SET TERM custom
// script delimiter falls immediately after that item -- the item-index
// equivalent of a Semicolon lexeme, for the one boundary kind lex() does not
// represent as a lexeme at all. items and offsets must both be in ascending
// document order (as significantLexemes and scriptDelimiterOffsets produce).
func boundaryAfterItems(items []lexeme, offsets []int) []bool {
	marks := make([]bool, len(items))
	item := 0
	for _, offset := range offsets {
		last := -1
		for item < len(items) && items[item].Span.Start < offset {
			last = item
			item++
		}
		if last >= 0 {
			marks[last] = true
		}
	}
	return marks
}

// splitStatements delimits each candidate statement using the same
// depth-tracking recovery boundary completeStatementEnd uses (width/singleton
// diagnostics reuse that function directly; this model additionally treats a
// SET TERM boundary in boundaryAfter as an equally valid terminator, since
// lex() does not represent that boundary as a lexeme), falling back to the
// document end for a final statement with no explicit terminator. A
// statement that never reaches a balanced state is reported Malformed and
// recovery resumes at the next top-level ";" or SET TERM boundary, so a
// later safely delimited statement is never hidden.
func splitStatements(items []lexeme, boundaryAfter []bool) []modelStatement {
	var statements []modelStatement
	i := 0
	for i < len(items) {
		start := i
		end, consumeSemicolon, ok := statementEnd(items, start, boundaryAfter)
		if ok {
			statements = append(statements, modelStatement{Start: start, End: end})
			i = end
			if consumeSemicolon {
				i++
			}
			continue
		}
		contentEnd, nextStart := recoveryBoundary(items, start, boundaryAfter)
		statements = append(statements, modelStatement{Start: start, End: contentEnd, Malformed: true})
		i = nextStart
	}
	return statements
}

// recoveryBoundary finds the next safe statement boundary at or after start,
// whether an ordinary ";" token or a SET TERM boundary, for malformed
// statement recovery. contentEnd excludes a terminating ";" token; nextStart
// is where the next statement begins.
func recoveryBoundary(items []lexeme, start int, boundaryAfter []bool) (contentEnd, nextStart int) {
	for i := start; i < len(items); i++ {
		if items[i].Token.Kind == token.Semicolon {
			return i, i + 1
		}
		if i < len(boundaryAfter) && boundaryAfter[i] {
			return i + 1, i + 1
		}
	}
	return len(items), len(items)
}

func statementEnd(items []lexeme, start int, boundaryAfter []bool) (end int, consumeSemicolon bool, ok bool) {
	depth := 0
	for i := start; i < len(items); i++ {
		switch items[i].Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth < 0 {
				return i, false, false
			}
		case token.Semicolon:
			if depth != 0 {
				return i, false, false
			}
			return i, true, true
		}
		if depth == 0 && i < len(boundaryAfter) && boundaryAfter[i] {
			return i + 1, false, true
		}
	}
	if depth != 0 {
		return len(items), false, false
	}
	return len(items), false, true
}

type withParse struct {
	ctes       []cteDef
	finalStart int
}

type cteDef struct {
	name               Name
	bodyStart, bodyEnd int
	columns            []Name // explicit WITH <name> (<col>,...) list, nil when absent
}

// parseWithStatement recognizes WITH <name> [(<col>,...)] AS (<select>)
// [, <name2> AS (<select2>)]* <select-using-those-names>, over a single
// statement's own item range. Any nested SELECT (a subquery, EXISTS, IN, or
// a UNION arm -- UNION's second arm is itself introduced by a SELECT
// keyword) anywhere inside a CTE body or the final SELECT, beyond that
// body/query's own leading SELECT, is left unrecognized rather than
// partially modeled: per-arm UNION output shapes, correlated/nested scopes,
// and recursive self-reference detection inside a WITH are not yet
// implemented, and silently modeling only part of the statement would hide
// real content from RelationScope while still reporting it supported. The
// whole WITH statement is rejected in that case; callers must fall back to
// Unsupported, which then reports true for its entire span.
func parseWithStatement(text string, items []lexeme, start, end int, matching map[int]int) (withParse, bool) {
	if start >= end || !isWord(items[start], "WITH") {
		return withParse{}, false
	}
	i := start + 1
	seen := make(map[string]bool)
	var ctes []cteDef
	for {
		if i >= end || !isNameToken(items[i]) {
			return withParse{}, false
		}
		name, ok := nameFromLexeme(text, items[i])
		if !ok || seen[name.Key()] {
			return withParse{}, false
		}
		i++
		var columns []Name
		if i < end && items[i].Token.Kind == token.LParen {
			close, ok := matching[i]
			if !ok || close >= end {
				return withParse{}, false
			}
			columns, ok = columnListNames(text, items[i+1:close])
			if !ok {
				return withParse{}, false
			}
			i = close + 1
		}
		if i >= end || !isWord(items[i], "AS") {
			return withParse{}, false
		}
		i++
		if i >= end || items[i].Token.Kind != token.LParen {
			return withParse{}, false
		}
		close, ok := matching[i]
		if !ok || close >= end {
			return withParse{}, false
		}
		bodyStart := i + 1
		if bodyStart >= close || !isWord(items[bodyStart], "SELECT") {
			return withParse{}, false
		}
		if containsNestedSelect(items[bodyStart+1 : close]) {
			return withParse{}, false
		}
		seen[name.Key()] = true
		ctes = append(ctes, cteDef{name: name, bodyStart: bodyStart, bodyEnd: close, columns: columns})
		i = close + 1
		if i < end && items[i].Token.Kind == token.Comma {
			i++
			continue
		}
		break
	}
	if i >= end || !isWord(items[i], "SELECT") {
		return withParse{}, false
	}
	if containsNestedSelect(items[i+1 : end]) {
		return withParse{}, false
	}
	return withParse{ctes: ctes, finalStart: i}, true
}

// containsNestedSelect reports whether items contains a SELECT keyword
// anywhere, at any nesting depth. Callers scope items to exclude a query's
// own leading SELECT, so any match here indicates a subquery or UNION arm.
func containsNestedSelect(items []lexeme) bool {
	for _, item := range items {
		if isWord(item, "SELECT") {
			return true
		}
	}
	return false
}

// columnListNames parses a parenthesized comma-separated identifier list
// (a CTE's explicit output-column-name list) into ordered Names. ok is false
// for anything other than plain identifiers separated by commas.
func columnListNames(text string, items []lexeme) ([]Name, bool) {
	parts, ok := splitTopLevel(items, token.Comma)
	if !ok {
		return nil, false
	}
	names := make([]Name, len(parts))
	for i, part := range parts {
		if len(part) != 1 || !isNameToken(part[0]) {
			return nil, false
		}
		name, ok := nameFromLexeme(text, part[0])
		if !ok {
			return nil, false
		}
		names[i] = name
	}
	return names, true
}

func (m *diagnosticModel) buildWithStatement(text string, items []lexeme, depths []int, matching map[int]int, stmt modelStatement) bool {
	parsed, ok := parseWithStatement(text, items, stmt.Start, stmt.End, matching)
	if !ok {
		return false
	}
	for _, cte := range parsed.ctes {
		synthetic := sqlQuery{start: cte.bodyStart, end: cte.bodyEnd, baseDepth: depths[cte.bodyStart], kind: "SELECT", parent: -1}
		relations, _, _ := queryRelations(text, items, synthetic, depths, matching)
		m.queries = append(m.queries, modelQuery{
			start: cte.bodyStart, end: cte.bodyEnd, parent: -1, kind: "SELECT",
			baseDepth: synthetic.baseDepth, relations: relations, cteName: cte.name.Key(),
		})
		if len(cte.columns) > 0 {
			if m.cteColumns == nil {
				m.cteColumns = make(map[int][]Name)
			}
			m.cteColumns[len(m.queries)-1] = cte.columns
		}
	}
	synthetic := sqlQuery{start: parsed.finalStart, end: stmt.End, baseDepth: depths[parsed.finalStart], kind: "SELECT", parent: -1}
	relations, _, _ := queryRelations(text, items, synthetic, depths, matching)
	m.queries = append(m.queries, modelQuery{
		start: parsed.finalStart, end: stmt.End, parent: -1, kind: "SELECT",
		baseDepth: synthetic.baseDepth, relations: relations,
	})
	return true
}

// queryRelationDetails mirrors queryRelations' own target-detection and
// FROM/JOIN scan for every query kind, additionally recording the item
// index each relation reference began at. queryRelations itself does not
// expose this position; the model needs it for several purposes: connecting
// a derived table's alias to its own inner query, a FROM-clause procedure
// call's alias to its catalog identity, and giving unknown-relation
// checking a query-boundary-aware candidate position for every one of
// q.relations (including an UPDATE/INSERT/DELETE target), rather than a
// naive "name immediately after any FROM keyword" scan that cannot
// distinguish a query's own FROM clause from FROM used as function-call
// syntax (for example EXTRACT(YEAR FROM col) or TRIM(... FROM col)).
func queryRelationDetails(text string, items []lexeme, query sqlQuery, depths []int, matching map[int]int) []relationDetail {
	if query.start >= query.end {
		return nil
	}
	seen := make(map[int]bool)
	var details []relationDetail
	addAt := func(index int, callable bool) int {
		ref, next, ok := relationAt(text, items, index, query.end, depths, matching, callable)
		if !ok || seen[index] {
			return index
		}
		seen[index] = true
		details = append(details, relationDetail{ref: ref, start: index, end: next})
		return next - 1
	}

	switch query.kind {
	case "UPDATE":
		if index := nextName(items, query.start+1, query.end); index >= 0 {
			addAt(index, false)
		}
	case "INSERT":
		if index := wordAfter(items, query.start+1, query.end, "INTO"); index >= 0 {
			addAt(index, false)
		}
	case "DELETE":
		if index := wordAfter(items, query.start+1, query.end, "FROM"); index >= 0 {
			addAt(index, false)
		}
	}

	inFrom := false
	for i := query.start + 1; i < query.end; i++ {
		if depths[i] != query.baseDepth {
			continue
		}
		if close, ok := matching[i]; ok {
			i = close
			continue
		}
		switch {
		case isWord(items[i], "FROM"), isWord(items[i], "USING"):
			inFrom = true
			if isWord(items[i], "FROM") && query.kind == "DELETE" {
				inFrom = false
			}
		case isWord(items[i], "JOIN"):
			inFrom = true
			i = addAt(i+1, true)
		case inFrom && items[i].Token.Kind == token.Comma:
			i = addAt(i+1, true)
		case inFrom && isQueryClause(items[i]):
			inFrom = false
		}
		if isWord(items[i], "FROM") && query.kind == "SELECT" {
			i = addAt(i+1, true)
		}
	}
	return details
}

type relationDetail struct {
	ref   RelationRef
	start int
	end   int // item index one past this relation reference's own text (name/call-args/derived-body and optional alias)
}

func (m *diagnosticModel) connectRelationSources(text string, items []lexeme, depths []int, matching map[int]int) {
	m.derivedFor = make(map[relationSourceKey]int)
	m.procedureNameFor = make(map[relationSourceKey]Name)
	for qi, q := range m.queries {
		if q.kind != "SELECT" {
			continue
		}
		synthetic := sqlQuery{start: q.start, end: q.end, baseDepth: q.baseDepth, kind: q.kind}
		details := queryRelationDetails(text, items, synthetic, depths, matching)
		if len(details) != len(q.relations) {
			continue
		}
		for k, ref := range q.relations {
			if ref.Name.Key() != "" {
				continue
			}
			source := details[k].start
			key := relationSourceKey{owner: qi, alias: aliasKeyOf(ref.Alias)}
			if items[source].Token.Kind == token.LParen {
				if inner, ok := m.queryByStart[source+1]; ok && m.queries[inner].end == matching[source] {
					m.derivedFor[key] = inner
				}
				continue
			}
			if plain, _, ok := relationAt(text, items, source, q.end, depths, matching, false); ok && plain.Name.Key() != "" {
				m.procedureNameFor[key] = plain.Name
			}
		}
	}
}

// computeRelationPositions builds relationDeclPositions and
// relationPositions together from one per-query relation detail scan
// (queryRelationDetails, now general across every query kind, not just
// SELECT). relationDeclPositions covers every relation (not only the
// alias-only derived-table/callable ones connectRelationSources itself
// keys): a derived table's own body belongs to a separate, independently
// modeled inner query and is deliberately not swept in here, only its
// alias token is a declaration from the outer query's point of view; a
// plain table, an UPDATE/INSERT/DELETE target, or a FROM-clause callable's
// whole [start, end) span (name, and call-arguments if any, and alias if
// any) is swept, since none of it is ever a column value reference in the
// owning query's own scope -- including call arguments, whose column-
// existence validation this task does not attempt. relationPositions keeps
// the same per-query detail list itself, for RelationCandidates.
func (m *diagnosticModel) computeRelationPositions(text string, items []lexeme, depths []int, matching map[int]int) {
	m.relationDeclPositions = make(map[int]bool)
	m.relationPositions = make(map[int][]relationDetail)
	for qi, q := range m.queries {
		synthetic := sqlQuery{start: q.start, end: q.end, baseDepth: q.baseDepth, kind: q.kind}
		details := queryRelationDetails(text, items, synthetic, depths, matching)
		if len(details) != len(q.relations) {
			continue
		}
		m.relationPositions[qi] = details
		for k, ref := range q.relations {
			source, end := details[k].start, details[k].end
			if source < 0 || end > len(items) || source >= end {
				continue
			}
			if items[source].Token.Kind == token.LParen {
				if ref.Alias != nil {
					m.relationDeclPositions[end-1] = true
				}
				continue
			}
			for p := source; p < end; p++ {
				m.relationDeclPositions[p] = true
			}
		}
	}
}

// RelationCandidates returns query qi's own modeled relations (the same
// list RelationScope's innermost level exposes for qi when i falls inside
// qi), paired with each relation's own item-index position in the source.
func (m *diagnosticModel) RelationCandidates(qi int) []relationDetail {
	return append([]relationDetail(nil), m.relationPositions[qi]...)
}

// queryChain returns the same query-index walk RelationScope(i) uses to
// build its own chain (m.queryAt[i], then each query's own parent,
// outward), so a caller that also needs RelationScope's per-level owning
// query index -- not just its relations -- can keep both aligned by
// position without RelationScope itself needing to expose query indices.
func (m *diagnosticModel) queryChain(i int) []int {
	if i < 0 || i >= len(m.queryAt) {
		return nil
	}
	var chain []int
	for qi := m.queryAt[i]; qi >= 0; qi = m.queries[qi].parent {
		chain = append(chain, qi)
	}
	return chain
}

// buildTriggerQueries additively models each trigger body's own embedded
// SQL statement (SELECT/UPDATE/INSERT/DELETE) as a query, the same way
// procedure bodies' embedded SQL is already modeled by the ordinary
// discoverSQLQueries pass above. CREATE TRIGGER bodies get no such
// treatment from the shared binder: resolve.go's buildContexts has no
// CREATE TRIGGER recognition at all (unlike CREATE PROCEDURE), so a whole
// trigger statement's span is classified contextUnsupported end to end,
// and discoverSQLQueries (which requires contextSQL/contextExecute) never
// discovers anything inside one. This reuses Task 3's own
// newTriggerBodyContext/discoverTriggers (diagnostic_locals.go) -- built
// for a narrower purpose (finding procedural vs SQL regions for local-
// variable detection) but equally able to delimit each embedded SQL
// statement's own [start, end) span here -- over a trigger body already
// known to have a syntactically valid, supported shape (parseTrigger only
// ever returns ok=true for that recognized subset).
func (m *diagnosticModel) buildTriggerQueries(text string, items []lexeme, depths []int, matching map[int]int) {
	for _, trg := range discoverTriggers(text, items) {
		m.recognizedTriggerBodies = append(m.recognizedTriggerBodies, itemRange{trg.bodyStart, trg.bodyEnd})
		ctx := newTriggerBodyContext(items, trg.bodyStart, trg.bodyEnd)
		start := -1
		for idx := trg.bodyStart + 1; idx < trg.bodyEnd; idx++ {
			i := idx - trg.bodyStart
			if start < 0 && ctx.inSQL[i] && isQueryStart(items[idx]) {
				start = idx
			}
			if start >= 0 && !ctx.inSQL[i] {
				m.addTriggerQuery(text, items, depths, matching, start, idx)
				start = -1
			}
		}
		if start >= 0 {
			m.addTriggerQuery(text, items, depths, matching, start, trg.bodyEnd)
		}
	}
}

// addTriggerQuery models one trigger-embedded SQL statement's [start, end)
// span exactly as a standalone top-level statement would be: the same
// queryRelations scan, no parent (a trigger body is not a lexically
// enclosing query RelationScope should correlate through). It is appended
// after buildQueryAt/connectRelationSources/computeRelationPositions/
// computeOutputs have already run over the ordinary top-level queries, so
// it updates m.queryAt/m.queryByStart/m.relationPositions/
// m.relationDeclPositions directly here rather than relying on a second
// pass over those functions, and computes its own output shape directly
// when it is a SELECT so alias-visibility checking works the same as any
// other SELECT.
func (m *diagnosticModel) addTriggerQuery(text string, items []lexeme, depths []int, matching map[int]int, start, end int) {
	if start >= end {
		return
	}
	q := sqlQuery{start: start, end: end, baseDepth: depths[start], parent: -1, kind: itemWord(items[start])}
	relations, _, _ := queryRelations(text, items, q, depths, matching)
	qi := len(m.queries)
	m.queries = append(m.queries, modelQuery{
		start: start, end: end, parent: -1, kind: q.kind,
		baseDepth: q.baseDepth, relations: relations,
		statementIndex: statementIndexAt(m.statements, start),
	})
	for i := start; i < end && i < len(m.queryAt); i++ {
		if m.queryAt[i] < 0 {
			m.queryAt[i] = qi
		}
	}
	m.queryByStart[start] = qi

	details := queryRelationDetails(text, items, q, depths, matching)
	if len(details) == len(relations) {
		m.relationPositions[qi] = details
		for k, ref := range relations {
			source, end := details[k].start, details[k].end
			if source < 0 || end > len(items) || source >= end {
				continue
			}
			if items[source].Token.Kind == token.LParen {
				if ref.Alias != nil {
					m.relationDeclPositions[end-1] = true
				}
				continue
			}
			for p := source; p < end; p++ {
				m.relationDeclPositions[p] = true
			}
		}
	}

	if q.kind == "SELECT" {
		m.computeSelectOutput(text, qi)
	}
}

// computeOutputs computes each query's own projected output shape, in
// m.queries order. That order matters: a CTE's explicit column-list rename
// is applied immediately after computing that CTE's own output, before any
// later query (which can only reference earlier-declared CTEs or the CTE
// itself, per standard WITH scoping) has its own output computed -- so a
// star-expansion referencing that CTE always sees the renamed shape.
func (m *diagnosticModel) computeOutputs(text string) {
	for qi := range m.queries {
		if m.queries[qi].kind != "SELECT" {
			continue
		}
		m.computeSelectOutput(text, qi)
		if columns, ok := m.cteColumns[qi]; ok {
			m.applyCTEColumnList(qi, columns)
		}
	}
}

// applyCTEColumnList overrides qi's computed output column names with an
// explicit WITH-declared column list, positionally. If the list's count does
// not match the body's own proven output count, the CTE's output becomes
// entirely unknown rather than guessing a partial mapping.
func (m *diagnosticModel) applyCTEColumnList(qi int, columns []Name) {
	q := &m.queries[qi]
	if !q.output.CountKnown || len(q.output.Columns) != len(columns) {
		q.output = modelOutput{}
		return
	}
	renamed := make([]modelOutputColumn, len(columns))
	for i, name := range columns {
		renamed[i] = modelOutputColumn{Name: name, NameKnown: true}
	}
	q.output = modelOutput{Columns: renamed, CountKnown: true}
}

func (m *diagnosticModel) computeSelectOutput(text string, qi int) {
	q := &m.queries[qi]
	body := m.items[q.start+1 : q.end]
	fromRelative := topLevelWordIndex(body, "FROM")
	intoRelative := topLevelWordIndex(body, "INTO")
	columnEnd := len(body)
	if fromRelative >= 0 {
		columnEnd = fromRelative
	}
	if intoRelative >= 0 && intoRelative < columnEnd {
		columnEnd = intoRelative
	}
	q.targetStart = q.start + 1
	q.targetEnd = q.start + 1 + columnEnd
	q.output = m.computeProjection(text, qi, body[:columnEnd], q.relations)
}

func (m *diagnosticModel) computeProjection(text string, owner int, items []lexeme, relations []RelationRef) modelOutput {
	items = trimSetQuantifier(items)
	if len(items) == 0 {
		return modelOutput{}
	}
	parts, ok := splitTopLevel(items, token.Comma)
	if !ok {
		// splitTopLevel fails this same way for a genuinely empty
		// comma-separated segment (a trailing comma right before FROM, a
		// leading comma, or two consecutive commas) as it does for
		// unbalanced parentheses within the projection list -- either way,
		// the projection list itself did not parse cleanly.
		m.queries[owner].malformedProjection = true
		return modelOutput{}
	}
	var columns []modelOutputColumn
	for _, part := range parts {
		if len(part) == 0 {
			m.queries[owner].malformedProjection = true
			return modelOutput{}
		}
		if isStarProjection(part) {
			expanded, ok := m.expandStar(text, owner, part, relations)
			if !ok {
				return modelOutput{}
			}
			columns = append(columns, expanded...)
			continue
		}
		columns = append(columns, singleProjectionColumn(text, part))
	}
	return modelOutput{Columns: columns, CountKnown: true}
}

func trimSetQuantifier(items []lexeme) []lexeme {
	if len(items) > 0 && (isWord(items[0], "DISTINCT") || isWord(items[0], "ALL")) {
		return items[1:]
	}
	return items
}

func isStarProjection(part []lexeme) bool {
	if len(part) == 1 && part[0].Token.Kind == token.Mult {
		return true
	}
	return len(part) == 3 && isNameToken(part[0]) && part[1].Token.Kind == token.Period && part[2].Token.Kind == token.Mult
}

// expandStar resolves a SELECT * or qualified alias.* projection through the
// same relation-output path as RelationOutput (CTE and derived-table shapes,
// procedure outputs, then real catalog columns, each checked against
// DDLInvalidated) rather than reading the catalog directly, so star
// expansion cannot mistake a real table for a same-named CTE that shadows it
// in this scope, and cannot bake in a catalog shape a DDL statement earlier
// in this same document has already made stale.
func (m *diagnosticModel) expandStar(text string, owner int, part []lexeme, relations []RelationRef) ([]modelOutputColumn, bool) {
	var qualifier *Name
	if len(part) == 3 {
		name, ok := nameFromLexeme(text, part[0])
		if !ok {
			return nil, false
		}
		qualifier = &name
	}
	var target *RelationRef
	if qualifier == nil {
		if len(relations) != 1 {
			return nil, false
		}
		target = &relations[0]
	} else {
		matches := 0
		for i := range relations {
			if relationMatchesQualifier(relations[i], *qualifier) {
				matches++
				target = &relations[i]
			}
		}
		if matches != 1 {
			return nil, false
		}
	}
	if target == nil {
		return nil, false
	}
	at := m.queries[owner].start
	cols, ok := m.resolveRelationOutput(at, owner, *target)
	if !ok {
		return nil, false
	}
	out := make([]modelOutputColumn, len(cols))
	for i, col := range cols {
		out[i] = modelOutputColumn{Name: Name{Text: col.Name, Quoted: true}, NameKnown: true}
	}
	return out, true
}

func singleProjectionColumn(text string, part []lexeme) modelOutputColumn {
	if alias := topLevelWordIndex(part, "AS"); alias >= 0 && alias == len(part)-2 && isNameToken(part[len(part)-1]) {
		name, ok := nameFromLexeme(text, part[len(part)-1])
		return modelOutputColumn{Name: name, NameKnown: ok}
	}
	if len(part) == 1 && isNameToken(part[0]) {
		name, ok := nameFromLexeme(text, part[0])
		return modelOutputColumn{Name: name, NameKnown: ok}
	}
	if len(part) == 3 && isNameToken(part[0]) && part[1].Token.Kind == token.Period && isNameToken(part[2]) {
		name, ok := nameFromLexeme(text, part[2])
		return modelOutputColumn{Name: name, NameKnown: ok}
	}
	return modelOutputColumn{}
}

func (m *diagnosticModel) detectUnionGroups(items []lexeme, depths []int) {
	m.unionOf = make([]int, len(m.queries))
	for i := range m.unionOf {
		m.unionOf[i] = -1
	}
	byParent := make(map[int][]int)
	for qi, q := range m.queries {
		if q.kind != "SELECT" {
			continue
		}
		byParent[q.parent] = append(byParent[q.parent], qi)
	}
	for _, siblings := range byParent {
		sort.Slice(siblings, func(a, b int) bool { return m.queries[siblings[a]].start < m.queries[siblings[b]].start })
		var current []int
		for k := 0; k < len(siblings); k++ {
			if k > 0 {
				prev := m.queries[siblings[k-1]]
				next := m.queries[siblings[k]]
				// discoverSQLQueries closes a sibling query exactly at the
				// next sibling's start (closeQueriesAtDepth), so prev.end
				// equals next.start; the UNION/UNION ALL keyword between
				// the arms is inside prev's own range, at its tail. Require
				// the UNION token itself to be at prev's own baseDepth, not
				// merely somewhere in the byte range, so a nested UNION
				// inside one sibling's own subquery (for example within an
				// unrelated EXISTS(...)) can never be mistaken for the
				// separator between two unrelated top-level siblings.
				if prev.statementIndex != next.statementIndex || !unionBetween(items, depths, prev.baseDepth, prev.start, next.start) {
					m.flushUnionGroup(current)
					current = nil
				}
			}
			current = append(current, siblings[k])
		}
		m.flushUnionGroup(current)
	}
}

func (m *diagnosticModel) flushUnionGroup(arms []int) {
	if len(arms) < 2 {
		return
	}
	group := len(m.unionGroups)
	m.unionGroups = append(m.unionGroups, arms)
	for _, qi := range arms {
		m.unionOf[qi] = group
	}
}

func unionBetween(items []lexeme, depths []int, baseDepth, from, to int) bool {
	for i := from; i < to && i < len(items); i++ {
		if items[i].Token.Kind == token.Semicolon {
			return false
		}
		if depths[i] == baseDepth && isWord(items[i], "UNION") {
			return true
		}
	}
	return false
}

var ddlObjectKinds = map[string]bool{
	"TABLE": true, "VIEW": true, "PROCEDURE": true, "DOMAIN": true,
	"TRIGGER": true, "INDEX": true, "GENERATOR": true, "SEQUENCE": true,
	"EXCEPTION": true, "ROLE": true, "FUNCTION": true, "PACKAGE": true,
}

// detectDDL records the object name each CREATE/ALTER/DROP statement names,
// keyed the same way Name.Key() works, so later rule tasks can check
// DDLInvalidated before trusting SemanticCatalog for that name.
func detectDDL(text string, items []lexeme, statements []modelStatement) []modelDDL {
	var ddls []modelDDL
	for _, stmt := range statements {
		if stmt.Malformed || stmt.Start >= stmt.End {
			continue
		}
		if name, ok := ddlInvalidatedName(text, items, stmt.Start, stmt.End); ok {
			ddls = append(ddls, modelDDL{key: name.Key(), end: stmt.End})
		}
	}
	return ddls
}

func ddlInvalidatedName(text string, items []lexeme, start, end int) (Name, bool) {
	if !isWord(items[start], "CREATE") && !isWord(items[start], "ALTER") && !isWord(items[start], "DROP") {
		return Name{}, false
	}
	i := start + 1
	if isWord(items[start], "CREATE") && i+1 < end && isWord(items[i], "OR") && isWord(items[i+1], "ALTER") {
		i += 2
	}
	kindAt := -1
	for i < end && i-start <= 5 {
		if isDDLObjectKind(items[i]) {
			kindAt = i
			break
		}
		i++
	}
	if kindAt < 0 {
		return Name{}, false
	}
	i = kindAt + 1
	if itemWord(items[kindAt]) == "INDEX" {
		// CREATE [UNIQUE] [ASC|DESC] INDEX name ON table: the invalidated
		// identity later statements actually query against is the table,
		// not the index name.
		if i < end && isNameToken(items[i]) {
			i++
		}
		if i < end && isWord(items[i], "ON") {
			i++
		}
	}
	if i >= end || !isNameToken(items[i]) {
		return Name{}, false
	}
	return nameFromLexeme(text, items[i])
}

func isDDLObjectKind(item lexeme) bool {
	if item.Token == nil || item.Token.Kind != token.SQLKeyword {
		return false
	}
	word, ok := item.Token.Value.(*token.SQLWord)
	return ok && word.QuoteStyle == 0 && ddlObjectKinds[strings.ToUpper(word.Keyword)]
}
