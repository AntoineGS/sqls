package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/queryparams"
	"github.com/sqls-server/sqls/parser"
)

const (
	CommandExecuteQuery     = "executeQuery"
	CommandShowDatabases    = "showDatabases"
	CommandShowSchemas      = "showSchemas"
	CommandShowConnections  = "showConnections"
	CommandSwitchDatabase   = "switchDatabase"
	CommandSwitchConnection = "switchConnections"
	CommandShowTables       = "showTables"
	CommandExplainQuery     = "explainQuery"
)

const lateCancellationNote = "Note: the cancellation request arrived after the statement completed; the result\nbelow is the real result.\n\n"

const executeProcedureOneRowNote = "EXECUTE PROCEDURE returns at most one row."

const unknownProcedureHint = `%s is not in the catalog cache. If it was created after this connection
opened, switch to this connection again to refresh the cache.`

func (s *Server) handleTextDocumentCodeAction(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.CodeActionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	commands := []lsp.Command{
		{
			Title:     "Execute Query",
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{params.TextDocument.URI},
		},
		{
			Title:     "Explain SQL",
			Command:   CommandExplainQuery,
			Arguments: []interface{}{params.TextDocument.URI},
		},
		{
			Title:     "Show Databases",
			Command:   CommandShowDatabases,
			Arguments: []interface{}{},
		},
		{
			Title:     "Show Schemas",
			Command:   CommandShowSchemas,
			Arguments: []interface{}{},
		},
		{
			Title:     "Show Connections",
			Command:   CommandShowConnections,
			Arguments: []interface{}{},
		},
		{
			Title:     "Switch Database",
			Command:   CommandSwitchDatabase,
			Arguments: []interface{}{},
		},
		{
			Title:     "Switch Connections",
			Command:   CommandSwitchConnection,
			Arguments: []interface{}{},
		},
		{
			Title:     "Show Tables",
			Command:   CommandShowTables,
			Arguments: []interface{}{},
		},
	}
	return commands, nil
}

func (s *Server) handleWorkspaceExecuteCommand(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.ExecuteCommandParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	entry := s.cancels.register(req.ID, cancel)
	defer s.cancels.unregister(req.ID)

	result, err = s.dispatchCommand(ctx, params)

	// A statement that actually stopped reports itself through cancelledError.
	// It is rendered as its own notice and never carries the late-arrival note:
	// the two messages contradict each other, and Task 9 adds the regression
	// test that asserts they never appear together.
	var cancelled *cancelledError
	if errors.As(err, &cancelled) {
		return cancelled.rendered, nil
	}

	if err != nil || !entry.cancelRequested() {
		return result, err
	}
	// The statement finished before the native cancellation took effect. The
	// executing result stays authoritative, so it is rendered with a note.
	if text, ok := result.(string); ok {
		return lateCancellationNote + text, nil
	}
	return result, nil
}

// cancelledError carries the results-pane text for a statement that stopped
// because its request was cancelled. The outcome travels as an error rather
// than as a plain string so the wrapper above can tell "this statement was
// cancelled" apart from "this statement completed, and a cancellation arrived
// too late". Task 9 populates it; until then nothing returns one.
//
// It deliberately carries only the rendered text: nothing in this plan needs
// the underlying driver error past this boundary, so adding an Unwrap now
// would be speculative. A later plan that wants to log why a statement
// stopped should add a cause field here rather than reconstructing it from
// the rendered string, because the cause is otherwise discarded at
// executeQuery.
type cancelledError struct {
	rendered string
}

func (e *cancelledError) Error() string {
	return e.rendered
}

func (s *Server) dispatchCommand(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	switch params.Command {
	case CommandExecuteQuery:
		return s.executeQuery(ctx, params)
	case CommandShowDatabases:
		return s.showDatabases(ctx, params)
	case CommandShowSchemas:
		return s.showSchemas(ctx, params)
	case CommandShowConnections:
		return s.showConnections(ctx, params)
	case CommandSwitchDatabase:
		return s.switchDatabase(ctx, params)
	case CommandSwitchConnection:
		return s.switchConnections(ctx, params)
	case CommandShowTables:
		return s.showTables(ctx, params)
	case CommandExplainQuery:
		return s.explainQuery(ctx, params)
	case CommandGetQueryParameters:
		return s.getQueryParameters(ctx, params)
	}
	return nil, fmt.Errorf("unsupported command: %v", params.Command)
}

func (s *Server) executeQuery(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	// parse execute command arguments
	s.stateMu.RLock()
	connected := s.dbConn != nil
	s.stateMu.RUnlock()
	if !connected {
		return nil, errors.New("database connection is not open")
	}

	showVertical := showVerticalRequested(params)

	// A submission carries its own SQL snapshot and the identity it was
	// prompted under, so it validates, compiles and binds that selection
	// itself instead of re-parsing the live document here.
	if params.ParameterValues != nil {
		return s.executeBoundStatements(ctx, params, showVertical)
	}

	if len(params.Arguments) == 0 {
		return nil, fmt.Errorf("required arguments were not provided: <File URI>")
	}
	uri, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the file uri as a string")
	}
	text, ok := s.fileText(uri)
	if !ok {
		return nil, fmt.Errorf("document not found, %q", uri)
	}

	// extract target query
	if params.Range != nil {
		text = extractRangeText(
			text,
			params.Range.Start.Line,
			params.Range.Start.Character,
			params.Range.End.Line,
			params.Range.End.Character,
		)
	}
	if s.parserDriver() == dialect.DatabaseDriverInterBase {
		text = queryparams.ExecutableSelects(text)
	}
	if err := s.refuseLegacyNamedParameters(text); err != nil {
		return nil, err
	}
	stmts, err := getStatementsWithDriverVariant(text, s.parserDriverVariant())
	if err != nil {
		return nil, err
	}

	queries := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		query := strings.TrimSpace(stmt.String())
		if query == "" {
			continue
		}
		queries = append(queries, query)
	}

	// execute statements
	rendered, err := renderStatements(ctx, len(queries), func(i int) (string, error) {
		return s.runStatement(ctx, queries[i], showVertical)
	})
	if err != nil {
		return nil, err
	}
	return rendered, nil
}

// showVerticalRequested reports whether the request asked for vertical output.
func showVerticalRequested(params lsp.ExecuteCommandParams) bool {
	if len(params.Arguments) > 1 {
		if flag, ok := params.Arguments[1].(string); ok {
			return flag == "-show-vertical"
		}
	}
	return false
}

// renderStatements runs count statements in order through run and
// concatenates their output, stopping at the first failure. It is the single
// place the batch result and cancellation semantics live: the legacy path and
// the parameterized path differ in what they dispatch, never in how a
// cancelled or failed statement ends the batch.
func renderStatements(ctx context.Context, count int, run func(int) (string, error)) (string, error) {
	buf := new(bytes.Buffer)
	for i := 0; i < count; i++ {
		res, err := run(i)
		if err != nil {
			if notice := cancellationNotice(ctx, err); notice != "" {
				fmt.Fprintln(buf, notice)
				// Reported as a cancelledError, not as (string, nil): the
				// command wrapper must be able to tell this apart from a
				// statement that completed before a late cancellation, or it
				// prepends a note saying the opposite of this one.
				return "", &cancelledError{rendered: buf.String()}
			}
			return "", err
		}
		fmt.Fprintln(buf, res)
	}
	return buf.String(), nil
}

// runStatement resolves the statement's route once and runs it. A
// parameterized batch calls runRoutedStatement directly with the decision its
// preflight already made, so no statement is ever routed twice.
func (s *Server) runStatement(ctx context.Context, query string, vertical bool, args ...any) (string, error) {
	return s.runRoutedStatement(ctx, query, vertical, s.statementRouting(query), args...)
}

func (s *Server) runRoutedStatement(ctx context.Context, query string, vertical bool, routing procedureRouting, args ...any) (string, error) {
	if routing.isQuery {
		return s.query(ctx, query, vertical, args...)
	}
	if routing.returnsRows {
		return s.queryProcedure(ctx, query, vertical, args...)
	}

	res, err := s.exec(ctx, query, vertical, args...)
	// The cache did not know this procedure, so Exec was a fallback rather
	// than a decision. Say so, instead of letting the driver's rejection read
	// like a mistake in the user's statement. A cancelled statement is left
	// alone so Plan 1's cancellation notice still wins.
	if err != nil && routing.unknown && cancellationNotice(ctx, err) == "" {
		return fmt.Sprintf("Exec failed: %v\n\n"+unknownProcedureHint+"\n", err, routing.name), nil
	}
	if err == nil && routing.unknown && routing.metadataIncomplete {
		return res + fmt.Sprintf("\n"+unknownProcedureHint+"\n", routing.name), nil
	}
	return res, err
}

// statementRouting is the whole once-and-only-once routing decision for one
// statement: read versus write first, then — for an InterBase EXECUTE
// PROCEDURE — which of the two write paths its cached descriptor calls for.
func (s *Server) statementRouting(query string) procedureRouting {
	if _, isQuery := database.QueryExecType(query, ""); isQuery {
		return procedureRouting{isQuery: true}
	}
	return s.interBaseProcedureRouting(query)
}

// procedureRouting is the once-and-only-once routing decision for a
// statement, including which of the two write paths an EXECUTE PROCEDURE
// call takes.
type procedureRouting struct {
	name string
	// isQuery is true when the statement is a read and takes the query path,
	// which may use a read-only transaction.
	isQuery bool
	// returnsRows is true only when the cache says the procedure has at least
	// one output parameter.
	returnsRows bool
	// unknown is true when the statement is an EXECUTE PROCEDURE call whose
	// procedure the cache could not resolve.
	unknown bool
	// metadataIncomplete distinguishes an absent signature from a conclusive
	// miss in a successfully loaded procedure category.
	metadataIncomplete bool
}

// interBaseProcedureRouting decides how to run an EXECUTE PROCEDURE statement,
// once. It deliberately never tries one path and falls back to the other: the
// driver rejects the wrong path at prepare, before execution, but a
// try-then-retry shape could execute a mutating procedure twice if that
// reasoning were ever wrong, and a stale cache is not worth that risk.
func (s *Server) interBaseProcedureRouting(query string) procedureRouting {
	if s.parserDriver() != dialect.DatabaseDriverInterBase {
		return procedureRouting{}
	}
	name := interBaseProcedureName(query)
	if name == "" {
		return procedureRouting{}
	}

	cache := s.worker.Cache()
	if cache == nil || !cache.HasCatalog() {
		return procedureRouting{name: name, unknown: true, metadataIncomplete: true}
	}
	// The accessor normalises the name it is given, so the identifier goes in
	// exactly as the user typed it.
	desc, ok := cache.Procedure(name)
	if !ok {
		return procedureRouting{name: name, unknown: true, metadataIncomplete: !cache.MetadataReady(database.MetadataProcedures)}
	}
	return procedureRouting{name: name, returnsRows: len(desc.OutputParameters) > 0}
}

func extractRangeText(text string, startLine, startChar, endLine, endChar int) string {
	lines := strings.Split(text, "\n")
	if startLine < 0 {
		startLine = 0
	}
	if endLine >= len(lines) {
		endLine = len(lines) - 1
	}
	if startLine >= len(lines) || startLine > endLine {
		return ""
	}

	var builder strings.Builder
	for i := startLine; i <= endLine; i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		st, en := 0, utf16Len(line)
		if i == startLine {
			st = startChar
		}
		if i == endLine {
			en = endChar
		}
		builder.WriteString(sliceUTF16(line, st, en))
		if i != endLine {
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func utf16Len(s string) int {
	length := 0
	for _, r := range s {
		length++
		if r > utf8.MaxRune || r < 0 {
			continue
		}
		if r > 0xFFFF {
			length++
		}
	}
	return length
}

func sliceUTF16(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}

	utf16Pos := 0
	startByte, endByte := len(s), len(s)
	startFound, endFound := false, false

	for bytePos, r := range s {
		runeWidth := 1
		if r > 0xFFFF {
			runeWidth = 2
		}

		if !startFound && start <= utf16Pos {
			startByte = bytePos
			startFound = true
		}
		if !endFound && end <= utf16Pos {
			endByte = bytePos
			endFound = true
			break
		}

		nextUTF16Pos := utf16Pos + runeWidth
		if !startFound && start < nextUTF16Pos {
			startByte = bytePos
			startFound = true
		}
		if !endFound && end < nextUTF16Pos {
			endByte = bytePos + utf8.RuneLen(r)
			endFound = true
			break
		}

		utf16Pos = nextUTF16Pos
	}

	if !startFound && start <= utf16Pos {
		startByte = len(s)
	}
	if !endFound {
		endByte = len(s)
	}
	if startByte > endByte {
		startByte = endByte
	}
	return s[startByte:endByte]
}

func (s *Server) query(ctx context.Context, query string, vertical bool, args ...any) (string, error) {
	return s.renderQuery(ctx, query, vertical, true, nil, args...)
}

// queryProcedure runs an EXECUTE PROCEDURE statement that the cache says
// returns output. It never uses ReadOnlyQuerier: an implicit procedure query
// commits its write transaction, so a procedure call is a write even when it
// returns a row.
func (s *Server) queryProcedure(ctx context.Context, query string, vertical bool, args ...any) (string, error) {
	return s.renderQuery(ctx, query, vertical, false, []string{executeProcedureOneRowNote}, args...)
}

func (s *Server) renderQuery(ctx context.Context, query string, vertical, allowReadOnly bool, notes []string, args ...any) (string, error) {
	result, scanErr := s.queryResult(ctx, query, allowReadOnly, args...)
	if result == nil {
		return "", scanErr
	}
	// A cancelled fetch is reported by the caller, which renders the
	// cancellation notice; partial rows under that notice would suggest the
	// statement produced a result when it was stopped.
	if scanErr != nil && cancellationNotice(ctx, scanErr) != "" {
		return "", scanErr
	}
	result.Notes = append(result.Notes, notes...)
	return renderQueryResult(result, vertical, scanErr)
}

// errBoundParametersUnsupported and errParameterizedReadOnlyUnsupported are
// shared with the preflight that refuses a batch before its first statement,
// so the message a user sees never depends on which of the two noticed.
var (
	errBoundParametersUnsupported       = errors.New("bound parameters are not supported by this repository")
	errParameterizedReadOnlyUnsupported = errors.New("parameterized read-only queries are not supported by this repository")
)

// boundRead is the capability a bound read statement will use: exactly one of
// its fields is non-nil.
type boundRead struct {
	readOnly database.ParameterizedReadOnlyQuerier
	querier  database.ParameterizedRepository
}

// boundReadFor resolves which parameterized capability repo offers for a read
// statement on this route, or the error that refuses it.
//
// It is the single owner of the read capability ladder. The batch preflight
// calls it to refuse a whole batch before its first statement, and queryResult
// calls it to choose the method it actually calls, so the two cannot disagree:
// a tier added here is seen by both, and "preflight passed but statement two
// failed" — a partially executed batch — stays impossible.
func boundReadFor(repo database.DBRepository, allowReadOnly bool) (boundRead, error) {
	if allowReadOnly {
		if readOnly, ok := repo.(database.ParameterizedReadOnlyQuerier); ok {
			return boundRead{readOnly: readOnly}, nil
		}
		// A repository that offers a read-only transaction without its
		// parameterized counterpart is refused rather than silently
		// downgraded to an ordinary one.
		if _, ok := repo.(database.ReadOnlyQuerier); ok {
			return boundRead{}, errParameterizedReadOnlyUnsupported
		}
	}
	querier, ok := repo.(database.ParameterizedRepository)
	if !ok {
		return boundRead{}, errBoundParametersUnsupported
	}
	return boundRead{querier: querier}, nil
}

// boundExecFor is boundReadFor's counterpart for a bound write statement, and
// is shared with the preflight for the same reason.
func boundExecFor(repo database.DBRepository) (database.ParameterizedRepository, error) {
	bound, ok := repo.(database.ParameterizedRepository)
	if !ok {
		return nil, errBoundParametersUnsupported
	}
	return bound, nil
}

// queryResult materialises a read statement's result. It prefers an explicit
// read-only transaction when the repository offers one; that transaction's
// lifetime stays inside the repository, so an early return here cannot leak it.
// Every path renders through ScanRowsWithTypes, so the partial-result contract
// is the same on every driver.
//
// A statement with bound arguments takes the parameterized capabilities: the
// arguments go to the driver, never into the SQL text. A repository that
// offers a read-only transaction but no parameterized counterpart is an error
// rather than a silent downgrade to an ordinary transaction.
func (s *Server) queryResult(ctx context.Context, query string, allowReadOnly bool, args ...any) (*database.QueryResult, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, err
	}
	if len(args) > 0 {
		read, err := boundReadFor(repo, allowReadOnly)
		if err != nil {
			return nil, err
		}
		if read.readOnly != nil {
			return read.readOnly.QueryReadOnlyParams(ctx, query, args)
		}
		rows, err := read.querier.QueryParams(ctx, query, args)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		return database.ScanRowsWithTypes(rows, database.RenderOptionsFor(repo.Driver()))
	}
	if readOnly, ok := repo.(database.ReadOnlyQuerier); ok && allowReadOnly {
		return readOnly.QueryReadOnly(ctx, query)
	}
	rows, err := repo.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return database.ScanRowsWithTypes(rows, database.RenderOptionsFor(repo.Driver()))
}

// blobLimitHint is emitted only when the result actually has a BLOB column, so
// the advice is never wrong. The driver's own error text is passed through
// verbatim beside it and is never matched on.
const blobLimitHint = `One or more BLOB columns in this result are larger than the driver's 64 MiB
materialisation limit. Re-run the query without the BLOB column, or select a
substring of it.`

// renderQueryResult writes the header, the rows that were scanned, the
// row-count footer and any notes. scanErr is non-nil when the fetch died
// partway: the rows that preceded it are still rendered, because knowing which
// row broke is the fastest route to the value that broke it.
func renderQueryResult(result *database.QueryResult, vertical bool, scanErr error) (string, error) {
	columns := make([]string, len(result.Columns))
	for i, column := range result.Columns {
		columns[i] = column.Name
	}

	buf := new(bytes.Buffer)
	if vertical {
		table := newVerticalTableWriter(buf)
		table.setHeaders(columns)
		for _, stringRow := range result.Rows {
			table.appendRow(stringRow)
		}
		table.render()
	} else {
		table := tablewriter.NewTable(buf, tablewriter.WithHeaderConfig(tw.CellConfig{
			Formatting: tw.CellFormatting{AutoFormat: tw.Off},
		}))
		headers := make([]any, len(columns))
		for i, v := range columns {
			headers[i] = v
		}
		table.Header(headers...)
		for _, stringRow := range result.Rows {
			row := make([]any, len(stringRow))
			for i, v := range stringRow {
				row[i] = v
			}
			if err := table.Append(row...); err != nil {
				return "", err
			}
		}
		if err := table.Render(); err != nil {
			return "", err
		}
	}

	if result.Complete {
		fmt.Fprintf(buf, "%d rows in set", len(result.Rows))
	} else {
		fmt.Fprintf(buf, "%d rows in set (incomplete)", len(result.Rows))
	}
	fmt.Fprintln(buf, "")
	fmt.Fprintln(buf, "")

	notes := append([]string(nil), result.Notes...)
	if scanErr != nil {
		notes = append(notes, fmt.Sprintf("Fetch failed: %v", scanErr))
		if hasBlobColumn(result) {
			notes = append(notes, blobLimitHint)
		}
	}
	for _, note := range notes {
		fmt.Fprintln(buf, note)
		fmt.Fprintln(buf, "")
	}
	return buf.String(), nil
}

func hasBlobColumn(result *database.QueryResult) bool {
	for _, column := range result.Columns {
		// database.BlobTypeName, never a second "BLOB" literal: the same
		// value gates renderCell's placeholder, and two copies in two
		// packages would drift silently.
		if column.DatabaseTypeName == database.BlobTypeName {
			return true
		}
	}
	return false
}

func (s *Server) exec(ctx context.Context, query string, vertical bool, args ...any) (string, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	var result sql.Result
	if len(args) > 0 {
		bound, boundErr := boundExecFor(repo)
		if boundErr != nil {
			return "", boundErr
		}
		result, err = bound.ExecParams(ctx, query, args)
	} else {
		result, err = repo.Exec(ctx, query)
	}
	if err != nil {
		return "", err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return "", err
	}

	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "Query OK, %d row affected", rowsAffected)
	fmt.Fprintln(buf, "")
	fmt.Fprintln(buf, "")
	return buf.String(), nil
}

func (s *Server) showDatabases(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	databases, err := repo.Databases(ctx)
	if err != nil {
		return nil, err
	}
	return strings.Join(databases, "\n"), nil
}

func (s *Server) showSchemas(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	schemas, err := repo.Schemas(ctx)
	if err != nil {
		return nil, err
	}
	return strings.Join(schemas, "\n"), nil
}

// validateDatabaseSwitch asks the repository whether it can serve dbName.
// Repositories that do not implement database.DatabaseSwitchRepository accept
// every name, which is the behavior every driver had before InterBase.
func validateDatabaseSwitch(ctx context.Context, repo database.DBRepository, dbName string) error {
	switcher, ok := repo.(database.DatabaseSwitchRepository)
	if !ok {
		return nil
	}
	return switcher.ValidateDatabaseSwitch(ctx, dbName)
}

// isNoOpDatabaseSwitch reports whether dbName already names the database a
// single-attachment repository (today, only InterBase) is connected to.
// ValidateDatabaseSwitch accepts that same name on the understanding that
// switching to it is a harmless refresh, but actually reconnecting would feed
// the already-composed attachment string back into newDBConnection, which
// writes it into DBConfig.DBName and lets interBaseAttachment recompose it —
// turning an already-valid attachment into a broken, doubled one
// (host/port:host/port:path). switchDatabase short-circuits on this instead
// of reconnecting.
func isNoOpDatabaseSwitch(ctx context.Context, repo database.DBRepository, dbName string) bool {
	if _, ok := repo.(database.DatabaseSwitchRepository); !ok {
		return false
	}
	current, err := repo.CurrentDatabase(ctx)
	if err != nil || current == "" {
		return false
	}
	return strings.TrimSpace(current) == strings.TrimSpace(dbName)
}

func (s *Server) switchDatabase(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if len(params.Arguments) != 1 {
		return nil, fmt.Errorf("required arguments were not provided: <DB Name>")
	}
	dbName, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the db name as a string")
	}

	// Only consult an open connection. With none open, switchDatabase is how a
	// user selects the database to connect to, so there is nothing to validate.
	// newDBRepository takes stateMu internally, so this is safe to call while
	// holding only connMu.
	repo, err := s.newDBRepository(ctx)
	switch {
	case errors.Is(err, ErrNoConnection):
		// fall through: nothing to validate yet.
	case err != nil:
		return nil, err
	default:
		if err := validateDatabaseSwitch(ctx, repo, dbName); err != nil {
			return nil, err
		}
		if isNoOpDatabaseSwitch(ctx, repo, dbName) {
			// Already open: see isNoOpDatabaseSwitch for why reconnecting
			// would break rather than refresh it.
			return nil, nil
		}
	}

	// Change current database
	s.stateMu.Lock()
	s.curDBName = dbName
	s.stateMu.Unlock()

	// close and reconnection to database
	if err := s.reconnectionDB(ctx); err != nil {
		return nil, err
	}

	return nil, nil
}

func (s *Server) showConnections(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	results := []string{}
	conns := s.getConfig().Connections
	for i, conn := range conns {
		var desc string
		if conn.DataSourceName != "" {
			desc = conn.DataSourceName
		} else {
			switch conn.Proto {
			case database.ProtoTCP:
				desc = fmt.Sprintf("tcp(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
			case database.ProtoUDP:
				desc = fmt.Sprintf("udp(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
			case database.ProtoUnix:
				desc = fmt.Sprintf("unix(%s)/%s", conn.Path, conn.DBName)
			case database.ProtoHTTP:
				desc = fmt.Sprintf("http(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
			}
		}
		res := fmt.Sprintf("%d %s %s %s", i+1, conn.Driver, conn.Alias, desc)
		results = append(results, res)
	}
	return strings.Join(results, "\n"), nil
}

func (s *Server) switchConnections(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if len(params.Arguments) != 1 {
		return nil, fmt.Errorf("required arguments were not provided: <Connection Index>")
	}
	indexStr, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the connection index as a number")
	}

	var index int

	cfg := s.getConfig()
	if cfg != nil {
		for i, conn := range cfg.Connections {
			if conn.Alias == indexStr {
				index = i + 1
				break
			}
		}
	}
	if index <= 0 {
		index, err = strconv.Atoi(indexStr)
		if err != nil {
			return nil, fmt.Errorf("specify the connection index as a number, %w", err)
		}
	}

	if index <= 0 {
		return nil, fmt.Errorf("specify the connection index as a number")
	}
	index = index - 1

	// Reconnect database
	s.stateMu.Lock()
	s.curConnectionIndex = index
	s.stateMu.Unlock()

	// close and reconnection to database
	if err := s.reconnectionDB(ctx); err != nil {
		return nil, err
	}

	return nil, nil
}

func (s *Server) showTables(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return "", err
	}
	m, err := repo.SchemaTables(ctx)
	if err != nil {
		return nil, err
	}
	schema, err := repo.CurrentSchema(ctx)
	if err != nil {
		return nil, err
	}
	results := []string{}
	for k, vv := range m {
		for _, v := range vv {
			if k != "" {
				if schema != k {
					continue
				}
				results = append(results, k+"."+v)
			} else {
				results = append(results, v)
			}
		}
	}
	return strings.Join(results, "\n"), nil
}

// interBaseProcedureName returns the procedure named by an EXECUTE PROCEDURE
// statement, or "" when the statement is not one.
//
// It reads the statement text rather than the parse tree because grouping
// EXECUTE PROCEDURE into a single ast.MultiKeyword is a change to shared,
// dialect-independent parser state that a later plan owns. Returning "" is
// always safe: it routes to Exec, which is today's unconditional behaviour.
func interBaseProcedureName(query string) string {
	fields := strings.Fields(query)
	if len(fields) < 3 {
		return ""
	}
	if !strings.EqualFold(fields[0], "EXECUTE") || !strings.EqualFold(fields[1], "PROCEDURE") {
		return ""
	}

	name := fields[2]
	if index := strings.IndexAny(name, "(;"); index >= 0 {
		name = name[:index]
	}
	if strings.HasPrefix(name, `"`) {
		// A quoted name is only recoverable here when it contains no
		// whitespace; otherwise strings.Fields has already split it and the
		// caller falls back to Exec.
		if !strings.HasSuffix(name, `"`) || len(name) < 2 {
			return ""
		}
		return name[1 : len(name)-1]
	}
	return name
}

func getStatementsWithDriver(text string, driver dialect.DatabaseDriver) ([]*ast.Statement, error) {
	return getStatementsWithDriverVariant(text, dialect.DriverVariant{Driver: driver})
}

func getStatementsWithDriverVariant(text string, dv dialect.DriverVariant) ([]*ast.Statement, error) {
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	if err != nil {
		return nil, err
	}

	var stmts []*ast.Statement
	for _, node := range parsed.GetTokens() {
		stmt, ok := node.(*ast.Statement)
		if !ok {
			return nil, fmt.Errorf("invalid type want Statement parsed %T", node)
		}
		stmts = append(stmts, stmt)
	}
	return stmts, nil
}

type verticalTableWriter struct {
	writer       io.Writer
	headers      []string
	rows         [][]string
	headerMaxLen int
}

func newVerticalTableWriter(writer io.Writer) *verticalTableWriter {
	return &verticalTableWriter{
		writer: writer,
	}
}

func (vtw *verticalTableWriter) setHeaders(headers []string) {
	vtw.headers = headers
	for _, h := range headers {
		length := len(h)
		if vtw.headerMaxLen < length {
			vtw.headerMaxLen = length
		}
	}
}

func (vtw *verticalTableWriter) appendRow(row []string) {
	vtw.rows = append(vtw.rows, row)
}

func (vtw *verticalTableWriter) render() {
	for rowNum, row := range vtw.rows {
		fmt.Fprintf(vtw.writer, "***************************[ %d. row ]***************************", rowNum+1)
		fmt.Fprintln(vtw.writer, "")
		for colNum, col := range row {
			header := vtw.headers[colNum]

			padHeader := fmt.Sprintf("%"+strconv.Itoa(vtw.headerMaxLen)+"s", header)
			fmt.Fprintf(vtw.writer, "%s | %s", padHeader, col)
			fmt.Fprintln(vtw.writer, "")
		}
	}
}
