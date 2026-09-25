package sqlsymbol

import (
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/token"
)

const (
	codeReadBeforeAssignment = "interbase-read-before-assignment"
	codeOutputNotAssigned    = "interbase-output-not-assigned"
	codeDeadStore            = "interbase-dead-store"
	codeUnreachable          = "interbase-unreachable"

	flowMaxGraphNodes = 5000
	flowMaxWork       = 100000
	flowMaxPaths      = 128
)

type flowNodeKind uint8

const (
	flowBlock flowNodeKind = iota
	flowAssignment
	flowMaybeAssignment
	flowIf
	flowLoop
	flowSuspend
	flowExit
	flowUnknown
	flowNoop
)

type flowNode struct {
	kind       flowNodeKind
	start, end int // half-open significant-lexeme indexes
	condition  itemRange
	children   []*flowNode
	otherwise  []*flowNode
}

type flowParser struct {
	items []lexeme
	i     int
	end   int
	steps int
	work  int
	bad   bool
}

func (p *flowParser) charge() {
	p.work++
	if p.work > flowMaxWork {
		p.bad = true
	}
}

// flowFindings reports only conclusions supported by the small procedural
// grammar below. Unsupported statements invalidate facts they may affect;
// excessive graph/work/path growth abandons that procedure's conclusions.
func (m *diagnosticModel) flowFindings(options DiagnosticOptions) []Finding {
	if m == nil || m.analysis == nil || !options.anyOn(codeReadBeforeAssignment, codeOutputNotAssigned, codeDeadStore, codeUnreachable) {
		return nil
	}
	a := m.analysis
	if len(m.items) == 0 || len(m.items) > flowMaxWork {
		return nil
	}
	var findings []Finding
	graphUsed, workUsed := 0, 0
	for pi := range a.procedures {
		p := &a.procedures[pi]
		if p.Span.End <= p.Span.Start {
			continue
		}
		start, end := flowProcedureItems(m.items, p.Span)
		if start < 0 || end <= start {
			continue
		}
		body := start
		for body < end && !isWord(m.items[body], "BEGIN") {
			body++
		}
		if body >= end {
			continue
		}
		parser := flowParser{items: m.items, i: body, end: end}
		root := parser.parseStatement()
		graphUsed += parser.steps
		workUsed += parser.work
		if graphUsed > flowMaxGraphNodes || workUsed > flowMaxWork {
			break
		}
		if parser.bad || root == nil || flowCountNodes(root, flowMaxGraphNodes+1) > flowMaxGraphNodes {
			continue
		}
		procFindings, ok, procWork := m.analyzeFlowScope(flowProcedureSymbols(*p), root, options, flowMaxWork-workUsed)
		workUsed += procWork
		if workUsed > flowMaxWork {
			break
		}
		if ok {
			findings = append(findings, procFindings...)
		} else if workUsed >= flowMaxWork {
			break
		}
	}
	for _, trigger := range discoverTriggers(a.Text, m.items) {
		if graphUsed >= flowMaxGraphNodes || workUsed >= flowMaxWork {
			break
		}
		triggerSymbols, supported := flowTriggerSymbols(a.Text, m.items, trigger)
		if !supported || trigger.bodyStart < 0 || trigger.bodyEnd < trigger.bodyStart || trigger.bodyEnd >= len(m.items) {
			continue
		}
		parser := flowParser{items: m.items, i: trigger.bodyStart, end: trigger.bodyEnd + 1}
		root := parser.parseStatement()
		graphUsed += parser.steps
		workUsed += parser.work
		if graphUsed > flowMaxGraphNodes || workUsed > flowMaxWork {
			break
		}
		if parser.bad || root == nil || flowCountNodes(root, flowMaxGraphNodes+1) > flowMaxGraphNodes {
			continue
		}
		triggerFindings, ok, triggerWork := m.analyzeFlowScope(triggerSymbols, root, options, flowMaxWork-workUsed)
		workUsed += triggerWork
		if workUsed > flowMaxWork {
			break
		}
		if ok {
			findings = append(findings, triggerFindings...)
		} else if workUsed >= flowMaxWork {
			break
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Span.Start == findings[j].Span.Start {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Span.Start < findings[j].Span.Start
	})
	return findings
}

func flowProcedureItems(items []lexeme, span Span) (int, int) {
	start := sort.Search(len(items), func(i int) bool { return items[i].Span.Start >= span.Start })
	end := sort.Search(len(items), func(i int) bool { return items[i].Span.Start >= span.End })
	if start >= len(items) || end <= start {
		return -1, -1
	}
	return start, end
}

func (p *flowParser) parseStatement() *flowNode {
	p.steps++
	if p.steps > flowMaxGraphNodes || p.i >= p.end {
		p.bad = true
		return nil
	}
	start := p.i
	item := p.items[p.i]
	switch {
	case isWord(item, "BEGIN"):
		p.i++
		n := &flowNode{kind: flowBlock, start: start}
		for p.i < p.end && !isWord(p.items[p.i], "END") {
			before := p.i
			child := p.parseStatement()
			if child != nil {
				n.children = append(n.children, child)
			}
			if p.i <= before {
				p.bad = true
				break
			}
		}
		if p.i >= p.end || !isWord(p.items[p.i], "END") {
			p.bad = true
			return n
		}
		p.i++
		if p.i < p.end && p.items[p.i].Token.Kind == token.Semicolon {
			p.i++
		}
		n.end = p.i
		return n
	case isWord(item, "IF"):
		p.i++
		condStart := p.i
		thenAt := p.scanKeyword("THEN")
		if thenAt < 0 {
			p.bad = true
			return nil
		}
		if thenAt == condStart || !flowExpressionComplete(p.items[condStart:thenAt]) {
			p.bad = true
			return nil
		}
		n := &flowNode{kind: flowIf, start: start, condition: itemRange{condStart, thenAt}}
		p.i = thenAt + 1
		child := p.parseStatement()
		if child == nil {
			p.bad = true
			return n
		}
		n.children = []*flowNode{child}
		if p.i < p.end && isWord(p.items[p.i], "ELSE") {
			p.i++
			elseChild := p.parseStatement()
			if elseChild == nil {
				p.bad = true
				return n
			}
			n.otherwise = []*flowNode{elseChild}
		}
		n.end = p.i
		return n
	case isWord(item, "WHILE"):
		p.i++
		condStart := p.i
		doAt := p.scanKeyword("DO")
		if doAt < 0 {
			p.bad = true
			return nil
		}
		if doAt == condStart || !flowExpressionComplete(p.items[condStart:doAt]) {
			p.bad = true
			return nil
		}
		n := &flowNode{kind: flowLoop, start: start, condition: itemRange{condStart, doAt}}
		p.i = doAt + 1
		child := p.parseStatement()
		if child == nil {
			p.bad = true
			return n
		}
		n.children = []*flowNode{child}
		n.end = p.i
		return n
	case isWord(item, "FOR"):
		p.i++
		condStart := p.i
		doAt := p.scanKeyword("DO")
		if doAt < 0 {
			p.bad = true
			return nil
		}
		if doAt == condStart || !flowSelectComplete(p.items[condStart:doAt]) {
			p.bad = true
			return nil
		}
		n := &flowNode{kind: flowLoop, start: start, condition: itemRange{condStart, doAt}}
		p.i = doAt + 1
		child := p.parseStatement()
		if child == nil {
			p.bad = true
			return n
		}
		if condStart >= doAt || !isWord(p.items[condStart], "SELECT") {
			return &flowNode{kind: flowUnknown, start: start, end: p.i, children: []*flowNode{child}}
		}
		n.children = []*flowNode{child}
		n.end = p.i
		return n
	case isWord(item, "WHEN"):
		// Handler conditions/exceptions can change which statements are
		// reached. Parse its body for structure, but represent the entire
		// handler as an unknown-effect edge unless handler semantics are
		// explicitly modeled.
		p.i++
		doAt := p.scanKeyword("DO")
		if doAt < 0 {
			p.bad = true
			return nil
		}
		p.i = doAt + 1
		body := p.parseStatement()
		if body == nil {
			p.bad = true
			return nil
		}
		return &flowNode{kind: flowUnknown, start: start, end: p.i, children: []*flowNode{body}}
	case isWord(item, "SUSPEND"):
		p.i++
		if !p.consumeSemicolon() {
			return &flowNode{kind: flowUnknown, start: start, end: p.i}
		}
		return &flowNode{kind: flowSuspend, start: start, end: p.i}
	case isWord(item, "EXIT"):
		p.i++
		if !p.consumeSemicolon() {
			return &flowNode{kind: flowUnknown, start: start, end: p.i}
		}
		return &flowNode{kind: flowExit, start: start, end: p.i}
	case item.Token.Kind == token.Semicolon:
		p.i++
		return &flowNode{kind: flowNoop, start: start, end: p.i}
	default:
		if isWord(item, "CASE") {
			return p.parseUnsupportedCase()
		}
		return p.parseSimple()
	}
}

func (p *flowParser) parseUnsupportedCase() *flowNode {
	start := p.i
	caseDepth, beginDepth := 0, 0
	for p.i < p.end {
		p.charge()
		if p.bad {
			return nil
		}
		item := p.items[p.i]
		switch {
		case isWord(item, "CASE"):
			caseDepth++
		case isWord(item, "BEGIN"):
			beginDepth++
		case isWord(item, "END"):
			if beginDepth > 0 {
				beginDepth--
			} else if caseDepth > 0 {
				caseDepth--
				if caseDepth == 0 {
					p.i++
					p.consumeSemicolon()
					return &flowNode{kind: flowUnknown, start: start, end: p.i}
				}
			}
		}
		p.i++
	}
	p.bad = true
	return nil
}

func (p *flowParser) scanKeyword(word string) int {
	depth, caseDepth := 0, 0
	for i := p.i; i < p.end; i++ {
		p.charge()
		if p.bad {
			return -1
		}
		item := p.items[i]
		if depth == 0 && caseDepth == 0 && isWord(item, word) {
			return i
		}
		if depth == 0 {
			switch {
			case isWord(item, "CASE"):
				caseDepth++
			case isWord(item, "END") && caseDepth > 0:
				caseDepth--
			}
		}
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		case token.Semicolon:
			if depth == 0 {
				return -1
			}
		}
	}
	return -1
}

func (p *flowParser) parseSimple() *flowNode {
	start := p.i
	depth := 0
	caseDepth := 0
	for p.i < p.end {
		p.charge()
		if p.bad {
			return &flowNode{kind: flowUnknown, start: start, end: p.i}
		}
		item := p.items[p.i]
		if depth == 0 && p.i > start && isWord(item, "END") && caseDepth == 0 {
			break
		}
		if item.Token.Kind == token.Semicolon && depth == 0 {
			p.i++
			break
		}
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
		if depth == 0 {
			switch {
			case isWord(item, "CASE"):
				caseDepth++
			case isWord(item, "END") && caseDepth > 0:
				caseDepth--
			}
		}
		p.i++
	}
	if p.i == start {
		p.bad = true
		return nil
	}
	if depth != 0 {
		p.bad = true
		return nil
	}
	n := &flowNode{start: start, end: p.i}
	first := p.items[start]
	terminated := p.i > start && p.items[p.i-1].Token.Kind == token.Semicolon
	switch {
	case isWord(first, "SELECT"):
		if !terminated || !flowSelectComplete(p.items[start:p.i]) {
			n.kind = flowUnknown
		} else if flowHasWord(p.items[start:p.i], "INTO") {
			n.kind = flowMaybeAssignment
		} else {
			n.kind = flowUnknown
		}
	case isWord(first, "MERGE"), isWord(first, "EXECUTE"), isWord(first, "POST_EVENT"), isWord(first, "INSERT"), isWord(first, "UPDATE"), isWord(first, "DELETE"), isWord(first, "EXCEPTION"):
		n.kind = flowUnknown
	case p.i-start >= 2 && isNameToken(first) && p.items[start+1].Token.Kind == token.Eq:
		if terminated && flowAssignmentComplete(p.items[start:p.i]) && !flowHasWord(p.items[start:p.i], "ELSE") {
			n.kind = flowAssignment
		} else {
			n.kind = flowUnknown
		}
	default:
		n.kind = flowUnknown
	}
	return n
}

func flowAssignmentComplete(items []lexeme) bool {
	end := len(items)
	if end == 0 || items[end-1].Token.Kind != token.Semicolon {
		return false
	}
	end--
	return end >= 3 && items[1].Token.Kind == token.Eq && flowExpressionComplete(items[2:end])
}

func flowSelectComplete(items []lexeme) bool {
	end := len(items)
	if end > 0 && items[end-1].Token.Kind == token.Semicolon {
		end--
	}
	if end < 2 || !balancedExpression(items[:end]) {
		return false
	}
	return !flowIncompleteTail(items[end-1])
}

func flowExpressionComplete(items []lexeme) bool {
	if len(items) == 0 || !balancedExpression(items) {
		return false
	}
	return !flowIncompleteTail(items[len(items)-1])
}

func flowIncompleteTail(item lexeme) bool {
	switch item.Token.Kind {
	case token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq,
		token.Plus, token.Minus, token.Mult, token.Div, token.Caret, token.Mod,
		token.Comma, token.Period, token.Colon, token.DoubleColon:
		return true
	}
	for _, keyword := range []string{"AND", "OR", "NOT", "IS", "LIKE", "IN", "BETWEEN", "FROM", "WHERE", "INTO", "JOIN", "ON", "SET", "VALUES", "SELECT", "AS", "THEN", "DO"} {
		if isWord(item, keyword) {
			return true
		}
	}
	return false
}

func (p *flowParser) consumeSemicolon() bool {
	if p.i < p.end && p.items[p.i].Token.Kind == token.Semicolon {
		p.i++
		return true
	}
	return false
}

func flowHasWord(items []lexeme, word string) bool {
	for _, item := range items {
		if isWord(item, word) {
			return true
		}
	}
	return false
}

func flowCountNodes(n *flowNode, cap int) int {
	if n == nil || cap <= 0 {
		return 0
	}
	count := 1
	for _, child := range n.children {
		count += flowCountNodes(child, cap-count)
	}
	for _, child := range n.otherwise {
		count += flowCountNodes(child, cap-count)
	}
	return count
}

type flowValueState uint8

const (
	flowNoExplicitAssignment flowValueState = iota
	flowExplicitlyAssigned
	flowUnknownValue
)

type flowState struct {
	values     map[*Symbol]flowValueState
	nulls      map[*Symbol]flowNullState
	terminated bool
}

type flowNullState uint8

const (
	flowNullUnknown flowNullState = iota
	flowKnownNull
)

type flowRun struct {
	model     *diagnosticModel
	symbols   []*Symbol
	options   DiagnosticOptions
	outputs   []*Symbol
	findings  map[string]Finding
	work      int
	workLimit int
	failed    bool
}

func flowProcedureSymbols(p procedure) []*Symbol {
	var symbols []*Symbol
	for _, group := range p.Symbols {
		symbols = append(symbols, group...)
	}
	return symbols
}

// flowTriggerSymbols adapts the existing conservative trigger recognizer to
// the shared flow engine. Only unique local names in procedural positions
// are bound; SQL clauses and qualified NEW/OLD/relation names are excluded.
func flowTriggerSymbols(text string, items []lexeme, trg triggerModel) ([]*Symbol, bool) {
	ctx := newTriggerBodyContext(items, trg.bodyStart, trg.bodyEnd)
	var symbols []*Symbol
	byName := make(map[string]*Symbol, len(trg.locals))
	for key, group := range trg.locals {
		if len(group) != 1 {
			return nil, false
		}
		local := group[0]
		symbol := &Symbol{Name: local.name, Kind: Variable, Declaration: local.declaration,
			Scope: Span{Start: items[trg.bodyStart].Span.Start, End: items[trg.bodyEnd].Span.End}}
		symbols = append(symbols, symbol)
		byName[key] = symbol
	}
	for i := trg.bodyStart + 1; i < trg.bodyEnd; i++ {
		if i-trg.bodyStart >= len(ctx.inSQL) || !isNameToken(items[i]) {
			continue
		}
		outputTarget := ctx.outputTarget[i-trg.bodyStart]
		hostBind := i > trg.bodyStart && items[i-1].Token.Kind == token.Colon
		if ctx.inSQL[i-trg.bodyStart] && !outputTarget && !hostBind {
			continue
		}
		if i > trg.bodyStart && items[i-1].Token.Kind == token.Period || i+1 < trg.bodyEnd && items[i+1].Token.Kind == token.Period {
			continue
		}
		name, ok := nameFromLexeme(text, items[i])
		if !ok {
			continue
		}
		symbol := byName[name.Key()]
		if symbol == nil {
			continue
		}
		span := items[i].Span
		symbol.Uses = append(symbol.Uses, span)
		if i+1 < trg.bodyEnd && items[i+1].Token.Kind == token.Eq || outputTarget {
			symbol.Writes = append(symbol.Writes, span)
		} else {
			symbol.Reads = append(symbol.Reads, span)
		}
	}
	return symbols, true
}

func (m *diagnosticModel) analyzeFlowScope(symbols []*Symbol, root *flowNode, options DiagnosticOptions, workLimit int) ([]Finding, bool, int) {
	r := &flowRun{model: m, symbols: symbols, options: options, findings: make(map[string]Finding), workLimit: workLimit}
	for _, symbol := range symbols {
		if symbol.Kind == OutputParameter {
			r.outputs = append(r.outputs, symbol)
		}
	}
	state := flowState{values: make(map[*Symbol]flowValueState), nulls: make(map[*Symbol]flowNullState)}
	for _, symbol := range symbols {
		initial, nullState := flowDeclarationInitial(m.items, symbol)
		if symbol.Kind == InputParameter {
			initial, nullState = flowExplicitlyAssigned, flowNullUnknown
		}
		state.values[symbol] = initial
		state.nulls[symbol] = nullState
	}
	r.findDeadStores(root)
	states := r.execute(root, []flowState{state})
	if r.failed {
		return nil, false, r.work
	}
	for _, st := range states {
		if !st.terminated {
			r.observeOutputs(st, "procedure exit")
		}
	}
	if r.failed {
		return nil, false, r.work
	}
	var findings []Finding
	for _, finding := range r.findings {
		findings = append(findings, finding)
	}
	return findings, true, r.work
}

// findDeadStores scans only reachable, supported basic-block paths. Branches
// are analyzed independently; loops retain zero-iteration reachability and
// do not carry store proofs across back-edges. Unknown effects clear proofs.
func (r *flowRun) findDeadStores(root *flowNode) {
	if !r.options.anyOn(codeDeadStore) || root == nil {
		return
	}
	var scan func(*flowNode, bool) bool
	scan = func(block *flowNode, reachable bool) bool {
		if block.kind != flowBlock {
			return reachable
		}
		pending := make(map[*Symbol]int)
		clear := func() { clear(pending) }
		for _, node := range block.children {
			if !reachable {
				break
			}
			r.charge()
			if r.failed {
				return false
			}
			if r.hasUnsupportedSyntax(node) {
				// Retain possible control continuation, but do not prove an
				// overwrite relationship across an unsupported effect.
				clear()
				continue
			}
			if (node.kind == flowIf || node.kind == flowLoop) && r.expressionHasCall(node.condition.start, node.condition.end) {
				// A called function may mutate locals or have other effects; the
				// branch/body therefore cannot establish an overwrite proof.
				clear()
				continue
			}
			switch node.kind {
			case flowAssignment:
				if r.expressionHasCall(node.start, node.end) {
					clear()
					continue
				}
				// Reads are processed before the write in an assignment.
				for _, symbol := range r.symbols {
					for _, read := range symbol.Reads {
						r.charge()
						if r.failed {
							return false
						}
						idx := flowIndexAt(r.model.items, read.Start)
						if idx >= node.start && idx < node.end {
							delete(pending, symbol)
						}
					}
				}
				for _, symbol := range r.symbols {
					for _, write := range symbol.Writes {
						r.charge()
						if r.failed {
							return false
						}
						idx := flowIndexAt(r.model.items, write.Start)
						if idx < node.start || idx >= node.end {
							continue
						}
						if previous, ok := pending[symbol]; ok {
							r.add(codeDeadStore, previous, fmt.Sprintf("assignment to %s is overwritten before it is read or observed", symbol.Name.Text), 4)
						}
						pending[symbol] = idx
					}
				}
			case flowSuspend:
				for _, output := range r.outputs {
					delete(pending, output)
				}
			case flowBlock:
				clear()
				reachable = scan(node, reachable)
				clear()
			case flowNoop:
			case flowIf:
				clear()
				thenContinues := scan(&flowNode{kind: flowBlock, children: node.children}, true)
				elseContinues := true // the implicit false edge when there is no ELSE
				if len(node.otherwise) > 0 {
					elseContinues = scan(&flowNode{kind: flowBlock, children: node.otherwise}, true)
				}
				reachable = thenContinues || elseContinues
				clear()
			case flowLoop:
				clear()
				// Stores within one loop-body pass may be proven dead, but no
				// pending store crosses a back-edge or zero-iteration path.
				for _, child := range node.children {
					if child.kind == flowBlock {
						scan(child, true)
					} else {
						scan(&flowNode{kind: flowBlock, children: []*flowNode{child}}, true)
					}
				}
				clear() // include the loop's possible zero-iteration path
			case flowExit:
				clear()
				reachable = false
			case flowUnknown, flowMaybeAssignment:
				clear()
			}
		}
		return reachable
	}
	scan(root, true)
}

// flowDeclarationInitial distinguishes the guaranteed engine NULL default
// from explicit assignment. Recognized initializer forms only mark the
// symbol assigned when they have a complete, balanced expression; the exact
// expression value is tracked separately and currently remains unknown.
func flowDeclarationInitial(items []lexeme, symbol *Symbol) (flowValueState, flowNullState) {
	if symbol.Kind != Variable {
		return flowNoExplicitAssignment, flowKnownNull
	}
	declaration := flowIndexAt(items, symbol.Declaration.Start)
	if declaration < 0 {
		return flowUnknownValue, flowNullUnknown
	}
	depth := 0
	for i := declaration + 1; i < len(items); i++ {
		item := items[i]
		if depth == 0 && item.Token.Kind == token.Semicolon {
			return flowNoExplicitAssignment, flowKnownNull
		}
		if depth == 0 && (item.Token.Kind == token.Eq || isWord(item, "DEFAULT")) {
			start := i + 1
			end := start
			for end < len(items) && items[end].Token.Kind != token.Semicolon {
				end++
			}
			if start >= end || !flowExpressionComplete(items[start:end]) {
				return flowUnknownValue, flowNullUnknown
			}
			if end == start+1 {
				value := items[start]
				if isWord(value, "NULL") {
					return flowExplicitlyAssigned, flowKnownNull
				}
				if value.Token.Kind == token.Number || value.Token.Kind == token.SingleQuotedString || value.Token.Kind == token.NationalStringLiteral || isWord(value, "TRUE") || isWord(value, "FALSE") || isWord(value, "USER") {
					return flowExplicitlyAssigned, flowNullUnknown
				}
			}
			// The initializer is present, but this expression's value/effects
			// are not modeled. Do not guess at either fact.
			return flowUnknownValue, flowNullUnknown
		}
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth < 0 {
				return flowUnknownValue, flowNullUnknown
			}
		}
	}
	return flowUnknownValue, flowNullUnknown
}

func (r *flowRun) execute(n *flowNode, states []flowState) []flowState {
	if r.failed || n == nil {
		return states
	}
	r.work++
	if r.work > r.workLimit || len(states) > flowMaxPaths {
		r.failed = true
		return nil
	}
	if r.hasUnsupportedSyntax(n) {
		for i := range states {
			if !states[i].terminated {
				r.invalidateState(&states[i])
			}
		}
		return states
	}
	switch n.kind {
	case flowBlock:
		for _, child := range n.children {
			allTerminated := len(states) > 0
			for _, st := range states {
				if !st.terminated {
					allTerminated = false
					break
				}
			}
			if allTerminated && r.options.anyOn(codeUnreachable) {
				r.add(codeUnreachable, child.start, fmt.Sprintf("statement after EXIT is unreachable"), 4)
			}
			states = r.execute(child, states)
			if r.failed {
				return nil
			}
		}
		return states
	case flowIf:
		for i := range states {
			st := &states[i]
			if st.terminated {
				continue
			}
			r.checkReads(n.condition.start, n.condition.end, *st)
			if r.expressionHasCall(n.condition.start, n.condition.end) {
				r.invalidateState(st)
			}
		}
		var out []flowState
		for _, st := range states {
			if st.terminated {
				out = append(out, st)
				continue
			}
			thenStates := r.executeSequence(n.children, []flowState{cloneFlowState(st)})
			var elseStates []flowState
			if len(n.otherwise) == 0 {
				elseStates = []flowState{cloneFlowState(st)}
			} else {
				elseStates = r.executeSequence(n.otherwise, []flowState{cloneFlowState(st)})
			}
			out = append(out, thenStates...)
			out = append(out, elseStates...)
		}
		return r.limitStates(out)
	case flowLoop:
		for i := range states {
			if states[i].terminated {
				continue
			}
			r.checkReads(n.condition.start, n.condition.end, states[i])
			if r.expressionHasCall(n.condition.start, n.condition.end) {
				r.invalidateState(&states[i])
			}
		}
		// The entry states are the mandatory zero-iteration exits. Explore
		// back-edges to a fixed point over the finite assignment-status lattice;
		// a bounded failure abandons the entire procedure's flow conclusions.
		out := make([]flowState, 0, len(states))
		frontier := make([]flowState, 0, len(states))
		for _, st := range states {
			out = r.appendState(out, cloneFlowState(st))
			if !st.terminated {
				frontier = r.appendState(frontier, cloneFlowState(st))
			}
		}
		visited := append([]flowState(nil), out...)
		for iteration := 0; len(frontier) > 0; iteration++ {
			if iteration >= 32 {
				r.failed = true
				return nil
			}
			var next []flowState
			for _, entry := range frontier {
				r.charge()
				if r.failed {
					return nil
				}
				iter := cloneFlowState(entry)
				// FOR SELECT targets are definitely assigned for each row path;
				// the no-row path remains in out from the loop's entry state.
				if n.condition.start < n.condition.end && isWord(r.model.items[n.condition.start], "SELECT") {
					r.applyWritesInRange(n.condition.start, n.condition.end, &iter, true)
				}
				body := r.executeSequence(n.children, []flowState{iter})
				for _, result := range body {
					if !r.stateIn(out, result) {
						out = append(out, cloneFlowState(result))
					}
					if !result.terminated && !r.stateIn(visited, result) {
						next = append(next, cloneFlowState(result))
						visited = append(visited, cloneFlowState(result))
					}
				}
				if len(out)+len(next) > flowMaxPaths {
					r.failed = true
					return nil
				}
			}
			frontier = next
		}
		return out
	case flowAssignment:
		for i := range states {
			if states[i].terminated {
				continue
			}
			r.checkReads(n.start, n.end, states[i])
			if r.expressionHasCall(n.start, n.end) {
				r.invalidateState(&states[i])
			}
			r.applyWritesInRange(n.start, n.end, &states[i], true)
		}
		return states
	case flowMaybeAssignment:
		var out []flowState
		for _, st := range states {
			if st.terminated {
				out = append(out, st)
				continue
			}
			r.checkReads(n.start, n.end, st)
			out = append(out, cloneFlowState(st))
			if !st.terminated {
				written := cloneFlowState(st)
				r.applyWritesInRange(n.start, n.end, &written, true)
				out = append(out, written)
			}
		}
		return r.limitStates(out)
	case flowSuspend:
		for _, st := range states {
			if !st.terminated {
				r.observeOutputs(st, "SUSPEND")
			}
		}
		return states
	case flowExit:
		for i := range states {
			if !states[i].terminated {
				r.observeOutputs(states[i], "EXIT")
				states[i].terminated = true
			}
		}
		return states
	case flowUnknown:
		for i := range states {
			if states[i].terminated {
				continue
			}
			r.invalidateState(&states[i])
		}
		return states
	default:
		return states
	}
}

func (r *flowRun) hasUnsupportedSyntax(n *flowNode) bool {
	start, end := n.start, n.end
	switch n.kind {
	case flowBlock:
		return false // child nodes are checked independently
	case flowIf, flowLoop:
		start, end = n.condition.start, n.condition.end
	}
	for i := start; i < end && i < len(r.model.items); i++ {
		r.charge()
		if r.failed {
			return true
		}
		if r.model.Unsupported(i) {
			return true
		}
	}
	return false
}

func (r *flowRun) executeSequence(nodes []*flowNode, states []flowState) []flowState {
	for _, node := range nodes {
		states = r.execute(node, states)
		if r.failed {
			return nil
		}
	}
	return states
}

func (r *flowRun) limitStates(states []flowState) []flowState {
	unique := make([]flowState, 0, len(states))
	for _, state := range states {
		unique = r.appendState(unique, state)
		if r.failed || len(unique) > flowMaxPaths {
			r.failed = true
			return nil
		}
	}
	return unique
}

func cloneFlowState(in flowState) flowState {
	out := flowState{values: make(map[*Symbol]flowValueState, len(in.values)), nulls: make(map[*Symbol]flowNullState, len(in.nulls)), terminated: in.terminated}
	for symbol, value := range in.values {
		out.values[symbol] = value
	}
	for symbol, value := range in.nulls {
		out.nulls[symbol] = value
	}
	return out
}

func (r *flowRun) stateIn(states []flowState, candidate flowState) bool {
	for _, state := range states {
		r.charge()
		if r.failed {
			return false
		}
		if state.terminated != candidate.terminated || len(state.values) != len(candidate.values) || len(state.nulls) != len(candidate.nulls) {
			continue
		}
		equal := true
		for symbol, value := range state.values {
			r.charge()
			if r.failed {
				return false
			}
			if candidate.values[symbol] != value || candidate.nulls[symbol] != state.nulls[symbol] {
				equal = false
				break
			}
		}
		if equal {
			return true
		}
	}
	return false
}

func (r *flowRun) appendState(states []flowState, state flowState) []flowState {
	if !r.stateIn(states, state) {
		return append(states, state)
	}
	return states
}

func (r *flowRun) checkReads(start, end int, state flowState) {
	if !r.options.anyOn(codeReadBeforeAssignment) {
		return
	}
	for _, symbol := range r.symbols {
		if state.values[symbol] != flowNoExplicitAssignment {
			continue
		}
		for _, read := range symbol.Reads {
			r.charge()
			if r.failed {
				return
			}
			idx := flowIndexAt(r.model.items, read.Start)
			if idx < start || idx >= end {
				continue
			}
			r.add(codeReadBeforeAssignment, idx, fmt.Sprintf("%s is read before an explicit assignment; its initial value is NULL and may be unintended", symbol.Name.Text), 2)
		}
	}
}

func (r *flowRun) applyWritesInRange(start, end int, state *flowState, definite bool) {
	for _, symbol := range r.symbols {
		for _, write := range symbol.Writes {
			r.charge()
			if r.failed {
				return
			}
			idx := flowIndexAt(r.model.items, write.Start)
			if idx >= start && idx < end {
				if definite {
					state.values[symbol] = flowExplicitlyAssigned
					if flowWriteRHSIsNull(r.model.items, idx, end) {
						state.nulls[symbol] = flowKnownNull
					} else {
						state.nulls[symbol] = flowNullUnknown
					}
				} else if state.values[symbol] != flowExplicitlyAssigned {
					state.values[symbol] = flowUnknownValue
					state.nulls[symbol] = flowNullUnknown
				}
			}
		}
	}
}

func (r *flowRun) observeOutputs(state flowState, when string) {
	if r.options.anyOn(codeOutputNotAssigned) {
		for _, output := range r.outputs {
			r.charge()
			if r.failed {
				return
			}
			if state.values[output] == flowNoExplicitAssignment {
				r.add(codeOutputNotAssigned, flowIndexAt(r.model.items, output.Declaration.Start),
					fmt.Sprintf("output %s reaches %s without an explicit assignment on this path; its value is not proven to be NULL", output.Name.Text, when), 2)
			}
		}
	}
}

func (r *flowRun) invalidateState(state *flowState) {
	for symbol := range state.values {
		r.charge()
		if r.failed {
			return
		}
		state.values[symbol] = flowUnknownValue
		state.nulls[symbol] = flowNullUnknown
	}
}

func (r *flowRun) charge() {
	r.work++
	if r.work > r.workLimit {
		r.failed = true
	}
}

func (r *flowRun) expressionHasCall(start, end int) bool {
	for i := start; i+1 < end; i++ {
		r.charge()
		if r.failed {
			return true
		}
		if isNameToken(r.model.items[i]) && r.model.items[i+1].Token.Kind == token.LParen {
			return true
		}
	}
	return false
}

func flowWriteRHSIsNull(items []lexeme, lhs, end int) bool {
	if lhs < 0 || lhs+2 >= end || items[lhs+1].Token.Kind != token.Eq || !isWord(items[lhs+2], "NULL") {
		return false
	}
	return lhs+3 == end || (lhs+4 == end && items[lhs+3].Token.Kind == token.Semicolon)
}

func (r *flowRun) add(code string, item int, message string, severity int) {
	if item < 0 || item >= len(r.model.items) {
		return
	}
	span := r.model.items[item].Span
	key := fmt.Sprintf("%s:%d:%d", code, span.Start, span.End)
	if _, ok := r.findings[key]; !ok {
		r.findings[key] = Finding{Span: span, Code: code, Message: message, Severity: severity}
	}
}

func flowIndexAt(items []lexeme, byteStart int) int {
	i := sort.Search(len(items), func(i int) bool { return items[i].Span.Start >= byteStart })
	if i < len(items) && items[i].Span.Start == byteStart {
		return i
	}
	return -1
}
