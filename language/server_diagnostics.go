package language

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/queryparams"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// StatementChecker prepares one statement on a live database without
// executing it. Implementations must prepare inside a read-only transaction
// that is rolled back.
type StatementChecker interface {
	// CheckStatement returns (nil, nil) when the server accepts the statement,
	// (*StatementError, nil) when the server rejects it, and (nil, err) for
	// failures that do not establish whether the SQL is valid.
	CheckStatement(ctx context.Context, sql string) (*StatementError, error)
}

// ServerDiagnosticsOptions controls whether this call may prepare uncached
// statements or only return findings already computed for unchanged text.
type ServerDiagnosticsOptions struct {
	CachedOnly bool
}

const (
	serverPrepareLimit   = 200
	serverPrepareTimeout = 2 * time.Second
)

// ServerDiagnostics returns server-prepare findings for the InterBase
// statements in req.Document. It caches both accepted statements and server
// rejections, while CachedOnly requests never call checker.
func (s *Service) ServerDiagnostics(ctx context.Context, req Request, checker StatementChecker, opts ServerDiagnosticsOptions) (Result[[]Diagnostic], error) {
	if err := ValidateDiagnosticPolicy(req.Policy); err != nil {
		return Result[[]Diagnostic]{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result[[]Diagnostic]{}, ErrCanceled
	}

	stamp := stampFor(req)
	diagnostics := make([]Diagnostic, 0)
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase || !req.Context.DialectResolved ||
		(!opts.CachedOnly && checker == nil) {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}

	policy := sqlsymbol.DiagnosticOptions{Rules: req.Policy.Rules}
	_, syntaxOn := policy.Level(sqlsymbol.CodeServerSyntax, 1)
	_, checkOn := policy.Level(sqlsymbol.CodeServerCheck, 1)
	if !syntaxOn && !checkOn {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}

	statements, err := sqlsymbol.ScriptStatements(req.Document.Text, req.Context.Variant)
	if err != nil {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}
	if err := ctx.Err(); err != nil {
		return Result[[]Diagnostic]{}, ErrCanceled
	}

	id := serverIdentity{key: req.Context.Key, incarnation: req.Context.Incarnation}
	generation := serverGen{}
	if req.Context.Snapshot != nil {
		generation.generation = req.Context.Snapshot.Generation
		generation.revision = req.Context.Snapshot.Revision
	}
	s.server.sync(id, generation)

	serverFindings := make([]Diagnostic, 0)
	prepares := 0
	preparingStopped := false
	for _, statement := range statements {
		if err := ctx.Err(); err != nil {
			return Result[[]Diagnostic]{}, ErrCanceled
		}
		if statement.Span.Start < 0 || statement.Span.End < statement.Span.Start || statement.Span.End > len(req.Document.Text) {
			continue
		}
		sql := req.Document.Text[statement.Span.Start:statement.Span.End]
		if !statement.Routine {
			if padded, ok := queryparams.PadNamedMarkers(sql); ok {
				sql = padded
			}
		}

		key := serverCacheKey{id: id, sql: sql}
		outcome, cached := s.server.get(key)
		if !cached {
			if opts.CachedOnly || preparingStopped || prepares >= serverPrepareLimit {
				continue
			}
			prepares++
			checkCtx, cancel := context.WithTimeout(ctx, serverPrepareTimeout)
			statementErr, checkErr := checker.CheckStatement(checkCtx, sql)
			cancel()
			if ctx.Err() != nil {
				return Result[[]Diagnostic]{}, ErrCanceled
			}
			if checkErr != nil {
				preparingStopped = true
				continue
			}
			outcome = serverOutcome{err: statementErr}
			s.server.put(key, outcome)
		}
		if outcome.err == nil {
			continue
		}

		code := sqlsymbol.CodeServerCheck
		if outcome.err.SQLCode == -104 {
			code = sqlsymbol.CodeServerSyntax
		}
		severity, on := policy.Level(code, 1)
		if !on {
			continue
		}
		span, ok := locateServerError(req.Document.Text, statement, *outcome.err)
		if !ok {
			continue
		}
		rangeValue, ok := spanRange(req.Document.Text, span)
		if !ok {
			continue
		}
		serverFindings = append(serverFindings, Diagnostic{
			Range:    rangeValue,
			Severity: severity,
			Code:     code,
			Source:   "sqls",
			Message:  serverMessageText(outcome.err),
		})
	}

	if err := ctx.Err(); err != nil {
		return Result[[]Diagnostic]{}, ErrCanceled
	}
	if len(serverFindings) == 0 {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}
	static, err := s.Diagnostics(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return Result[[]Diagnostic]{}, ErrCanceled
		}
		return Result[[]Diagnostic]{}, err
	}

	for _, finding := range serverFindings {
		deduplicated := false
		for _, staticFinding := range static.Value {
			if diagnosticRangesOverlap(finding.Range, staticFinding.Range) {
				deduplicated = true
				break
			}
		}
		if !deduplicated {
			diagnostics = append(diagnostics, finding)
		}
	}
	sortDiagnostics(diagnostics)
	return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
}

func locateServerError(text string, statement sqlsymbol.ScriptStatement, serverErr StatementError) (sqlsymbol.Span, bool) {
	start, end := statement.Span.Start, statement.Span.End
	if start < 0 || end < start || end > len(text) {
		return sqlsymbol.Span{}, false
	}
	sql := text[start:end]

	if serverErr.Line > 0 && serverErr.Column > 0 {
		lineStart, lineEnd, ok := statementLineBounds(sql, serverErr.Line)
		if ok {
			charOffset := characterOffset(sql, lineStart, lineEnd, serverErr.Column-1)
			byteOffset := lineStart + serverErr.Column - 1
			if byteOffset > lineEnd {
				byteOffset = lineEnd
			}
			offset := charOffset
			matched := false
			if serverErr.Token != "" {
				for _, candidate := range []int{charOffset, byteOffset} {
					if candidate >= 0 && candidate+len(serverErr.Token) <= len(sql) &&
						strings.EqualFold(sql[candidate:candidate+len(serverErr.Token)], serverErr.Token) {
						offset = candidate
						matched = true
						break
					}
				}
			}
			if matched {
				return sqlsymbol.Span{Start: start + offset, End: start + offset + len(serverErr.Token)}, true
			}
			if lexeme, ok := scriptTokenAt(statement.Tokens, start+offset); ok {
				return lexeme.Span, true
			}
			return oneRuneSpan(sql, start, offset), true
		}
		return firstLineSpan(sql, start), true
	}

	if strings.Contains(strings.ToLower(serverErr.Message), "unexpected end of command") ||
		(serverErr.SQLCode == -104 && serverErr.Column == 0 && serverErr.Token == "") {
		if len(statement.Tokens) > 0 {
			return statement.Tokens[len(statement.Tokens)-1].Span, true
		}
	}

	if serverErr.Token != "" {
		for _, scriptToken := range statement.Tokens {
			if (!scriptToken.Quoted && scriptToken.Word == strings.ToUpper(serverErr.Token)) ||
				(scriptToken.Quoted && scriptToken.Word == serverErr.Token) {
				return scriptToken.Span, true
			}
		}
	}
	return firstLineSpan(sql, start), true
}

func statementLineBounds(sql string, line int) (start, end int, ok bool) {
	if line < 1 {
		return 0, 0, false
	}
	start = 0
	for current := 1; current < line; current++ {
		next := strings.IndexByte(sql[start:], '\n')
		if next < 0 {
			return 0, 0, false
		}
		start += next + 1
	}
	end = len(sql)
	if next := strings.IndexByte(sql[start:], '\n'); next >= 0 {
		end = start + next
	}
	return start, end, true
}

func characterOffset(text string, start, end, count int) int {
	offset := start
	for remaining := count; remaining > 0 && offset < end; remaining-- {
		if text[offset] == '\n' {
			break
		}
		_, size := utf8.DecodeRuneInString(text[offset:end])
		if size < 1 {
			size = 1
		}
		offset += size
	}
	return offset
}

func scriptTokenAt(tokens []sqlsymbol.ScriptToken, offset int) (sqlsymbol.ScriptToken, bool) {
	for _, scriptToken := range tokens {
		if scriptToken.Span.Start <= offset && offset < scriptToken.Span.End {
			return scriptToken, true
		}
	}
	return sqlsymbol.ScriptToken{}, false
}

func oneRuneSpan(sql string, statementStart, offset int) sqlsymbol.Span {
	start := statementStart + offset
	end := start
	if offset >= 0 && offset < len(sql) && sql[offset] != '\n' {
		_, size := utf8.DecodeRuneInString(sql[offset:])
		if size < 1 {
			size = 1
		}
		end += size
	}
	return sqlsymbol.Span{Start: start, End: end}
}

func firstLineSpan(sql string, statementStart int) sqlsymbol.Span {
	lineEnd := len(sql)
	if newline := strings.IndexByte(sql, '\n'); newline >= 0 {
		lineEnd = newline
	}
	return sqlsymbol.Span{Start: statementStart, End: statementStart + lineEnd}
}

func diagnosticRangesOverlap(left, right Range) bool {
	if positionsEqual(left.Start, left.End) && positionsEqual(right.Start, right.End) {
		return positionsEqual(left.Start, right.Start)
	}
	return comparePosition(left.Start, right.End) < 0 && comparePosition(right.Start, left.End) < 0
}

func positionsEqual(left, right Position) bool {
	return left.Line == right.Line && left.UTF16Column == right.UTF16Column
}

func comparePosition(left, right Position) int {
	if left.Line < right.Line {
		return -1
	}
	if left.Line > right.Line {
		return 1
	}
	if left.UTF16Column < right.UTF16Column {
		return -1
	}
	if left.UTF16Column > right.UTF16Column {
		return 1
	}
	return 0
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if comparison := comparePosition(left.Range.Start, right.Range.Start); comparison != 0 {
			return comparison < 0
		}
		if comparison := comparePosition(left.Range.End, right.Range.End); comparison != 0 {
			return comparison < 0
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Message != right.Message {
			return left.Message < right.Message
		}
		return left.Severity < right.Severity
	})
}
