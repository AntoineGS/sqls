package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/queryparams"
)

// CommandGetQueryParameters discovers the named parameters in a selection. It
// is a pure scan over the document text under the active connection's SQL
// dialect: it never touches the catalog cache or the database.
const CommandGetQueryParameters = "getQueryParameters"

// parameterProtocolVersion is the only query-parameter envelope version this
// server speaks. A submission carrying any other version is refused rather
// than interpreted under assumptions its client never agreed to.
const parameterProtocolVersion = 1

// parameterSelection is the pure input discovery and submission both need:
// the selected SQL text, its identity for stale-submission detection, and the
// driver variant to compile it under.
type parameterSelection struct {
	Text    string
	Context lsp.QueryParameterContext
	Variant dialect.DriverVariant
}

// parameterSelection resolves the requested document/range and the active
// connection's identity into a parameterSelection. The caller must hold
// connMu.RLock: reconnectionDB replaces dbConn and curDBCfg under connMu, and
// this reads both.
//
// It copies the document text and every identity scalar out from under
// stateMu, releases the lock, and only then extracts the range, hashes, or
// compiles: the *File is never retained past the lock, and no mutable config
// is ever hashed once the lock that guards its concurrent replacement is
// gone.
func (s *Server) parameterSelection(params lsp.ExecuteCommandParams) (parameterSelection, error) {
	if len(params.Arguments) == 0 {
		return parameterSelection{}, fmt.Errorf("required arguments were not provided: <File URI>")
	}
	uri, ok := params.Arguments[0].(string)
	if !ok {
		return parameterSelection{}, fmt.Errorf("specify the file uri as a string")
	}

	s.stateMu.RLock()
	f, ok := s.files[uri]
	if !ok {
		s.stateMu.RUnlock()
		return parameterSelection{}, fmt.Errorf("document not found, %q", uri)
	}
	text := f.Text
	dbConn := s.dbConn
	cfg := s.curDBCfg
	generation := s.connGeneration
	s.stateMu.RUnlock()

	if params.Range != nil {
		if err := validateSelectionRange(text, *params.Range); err != nil {
			return parameterSelection{}, err
		}
	}

	selected := text
	if params.Range != nil {
		selected = extractRangeText(
			text,
			params.Range.Start.Line,
			params.Range.Start.Character,
			params.Range.End.Line,
			params.Range.End.Character,
		)
	}

	variant := dbConn.DriverVariant()
	sqlDialect := variant.Variant.InterBaseSQLDialect()

	queryKey, err := hashJSON([]interface{}{sqlDialect, selected})
	if err != nil {
		return parameterSelection{}, err
	}
	documentKey, err := hashJSON(text)
	if err != nil {
		return parameterSelection{}, err
	}

	sel := parameterSelection{
		Text: selected,
		Context: lsp.QueryParameterContext{
			Version:              parameterProtocolVersion,
			ConnectionGeneration: generation,
			QueryKey:             queryKey,
			DocumentKey:          documentKey,
		},
		Variant: variant,
	}

	if variant.Driver == dialect.DatabaseDriverInterBase {
		identity, err := database.NewInterBaseConnectionIdentity(cfg, dbConn)
		if err != nil {
			return parameterSelection{}, err
		}
		connectionKey, err := hashJSON(identity)
		if err != nil {
			return parameterSelection{}, err
		}
		sel.Context.ConnectionKey = connectionKey
	}

	return sel, nil
}

// hashJSON is the connectionKey/queryKey/documentKey algorithm: the hex
// SHA-256 of v's JSON encoding.
func hashJSON(v interface{}) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// validateSelectionRange rejects the range coordinates extractRangeText would
// otherwise silently clamp: negative positions, a start after the end, or a
// line/character past the end of the current document.
func validateSelectionRange(text string, r lsp.Range) error {
	if r.Start.Line < 0 || r.Start.Character < 0 || r.End.Line < 0 || r.End.Character < 0 {
		return fmt.Errorf("invalid range: position cannot be negative")
	}
	if r.Start.Line > r.End.Line || (r.Start.Line == r.End.Line && r.Start.Character > r.End.Character) {
		return fmt.Errorf("invalid range: start is after end")
	}

	lines := strings.Split(text, "\n")
	if r.End.Line >= len(lines) {
		return fmt.Errorf("invalid range: line %d is past the end of the document", r.End.Line)
	}
	endLineLen := utf16Len(strings.TrimSuffix(lines[r.End.Line], "\r"))
	if r.End.Character > endLineLen {
		return fmt.Errorf("invalid range: character %d is past the end of line %d", r.End.Character, r.End.Line)
	}
	startLineLen := utf16Len(strings.TrimSuffix(lines[r.Start.Line], "\r"))
	if r.Start.Character > startLineLen {
		return fmt.Errorf("invalid range: character %d is past the end of line %d", r.Start.Character, r.Start.Line)
	}
	return nil
}

// getQueryParameters discovers the named parameters in the requested
// document/range. It never queries the database: an unsupported connection
// (including no connection at all) simply reports supported=false so the
// client can fall back to the legacy execution flow. A supported,
// no-parameter selection still returns the complete identity so the client
// can make that choice too.
func (s *Server) getQueryParameters(ctx context.Context, params lsp.ExecuteCommandParams) (interface{}, error) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()

	sel, err := s.parameterSelection(params)
	if err != nil {
		return nil, err
	}

	discovery := lsp.QueryParameterDiscovery{
		QueryParameterContext: sel.Context,
		Parameters:            []queryparams.Parameter{},
	}
	if sel.Variant.Driver != dialect.DatabaseDriverInterBase {
		return discovery, nil
	}

	batch, err := queryparams.Compile(sel.Text, sel.Variant.Variant.InterBaseSQLDialect())
	if err != nil {
		return nil, err
	}
	discovery.Supported = true
	if len(batch.Parameters) > 0 {
		discovery.Parameters = batch.Parameters
	}
	return discovery, nil
}

// boundStatement is one preflighted statement of a parameterized batch: the
// exact SQL that will be sent, its ordered arguments, and the route resolved
// once, before anything ran. The routing decision travels with the statement
// so execution never asks a possibly-replaced worker cache a second time and
// gets a different answer for a statement already under way.
type boundStatement struct {
	sql     string
	args    []any
	routing procedureRouting
}

// executeBoundStatements runs a submitted parameter batch. The caller holds
// connMu.RLock for the whole call — preflight and execution alike — so no
// reconnect can land between the connection this batch was validated against
// and the one it runs on.
func (s *Server) executeBoundStatements(ctx context.Context, params lsp.ExecuteCommandParams, vertical bool) (interface{}, error) {
	plan, err := s.preflightBoundBatch(ctx, params)
	if err != nil {
		return nil, err
	}
	rendered, err := renderStatements(ctx, len(plan), func(i int) (string, error) {
		return s.runRoutedStatement(ctx, plan[i].sql, vertical, plan[i].routing, plan[i].args...)
	})
	if err != nil {
		return nil, err
	}
	return rendered, nil
}

// preflightBoundBatch validates a submission and turns it into the statements
// to run, or fails without running anything. Everything that can be checked
// is checked before the first statement: the prompt context still describes
// the current document and connection, the whole selection compiles, every
// value binds, and the repository offers the capability each route needs. A
// batch that fails here has not executed a single statement, which is what
// makes re-running it after fixing a value safe.
//
// params.ParameterValues must be non-nil: a request without one is a legacy
// execution and never reaches here.
func (s *Server) preflightBoundBatch(ctx context.Context, params lsp.ExecuteCommandParams) ([]boundStatement, error) {
	submission := params.ParameterValues
	sel, err := s.parameterSelection(params)
	if err != nil {
		return nil, err
	}
	if err := sel.validateSubmission(submission.QueryParameterContext); err != nil {
		return nil, err
	}
	if sel.Variant.Driver != dialect.DatabaseDriverInterBase {
		return nil, fmt.Errorf("bound query parameters are not supported on a %s connection", sel.Variant.Driver)
	}

	batch, err := queryparams.Compile(sel.Text, sel.Variant.Variant.InterBaseSQLDialect())
	if err != nil {
		return nil, err
	}
	args, err := queryparams.Bind(batch, submission.Values)
	if err != nil {
		return nil, err
	}

	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, err
	}
	plan := make([]boundStatement, 0, len(batch.Statements))
	for i, stmt := range batch.Statements {
		bound := boundStatement{
			sql:     stmt.SQL,
			args:    args[i],
			routing: s.statementRouting(stmt.SQL),
		}
		if err := boundRouteSupported(repo, bound); err != nil {
			return nil, err
		}
		plan = append(plan, bound)
	}
	return plan, nil
}

// validateSubmission rejects values entered under a context that no longer
// describes what would run now. The document is compared whole: an edit
// outside the selection is still an edit the user made while prompting, and
// executing a version they never reviewed is the outcome this refuses.
func (sel parameterSelection) validateSubmission(got lsp.QueryParameterContext) error {
	if got.Version != parameterProtocolVersion {
		return fmt.Errorf(
			"unsupported query parameter protocol version %d, want %d",
			got.Version, parameterProtocolVersion)
	}
	want := sel.Context
	switch {
	case got.ConnectionKey != want.ConnectionKey, got.ConnectionGeneration != want.ConnectionGeneration:
		return staleParameterContextError("the database connection")
	case got.QueryKey != want.QueryKey:
		return staleParameterContextError("the selected SQL")
	case got.DocumentKey != want.DocumentKey:
		return staleParameterContextError("the document")
	}
	return nil
}

func staleParameterContextError(what string) error {
	return fmt.Errorf(
		"%s changed after these parameter values were entered; nothing was executed. "+
			"Run the command again to re-enter them", what)
}

// boundRouteSupported reports whether repo can run stmt on the route preflight
// chose for it. A statement with no arguments keeps the legacy repository
// method and needs no optional capability at all, so a batch that mixes plain
// and parameterized statements is only held to what it actually uses.
func boundRouteSupported(repo database.DBRepository, stmt boundStatement) error {
	if len(stmt.args) == 0 {
		return nil
	}
	// Mirrors queryResult's own selection exactly: a read may use the
	// parameterized read-only transaction, and a repository offering the
	// unparameterized one without its counterpart is refused rather than
	// silently downgraded.
	if stmt.routing.isQuery {
		if _, ok := repo.(database.ParameterizedReadOnlyQuerier); ok {
			return nil
		}
		if _, ok := repo.(database.ReadOnlyQuerier); ok {
			return errParameterizedReadOnlyUnsupported
		}
	}
	if _, ok := repo.(database.ParameterizedRepository); !ok {
		return errBoundParametersUnsupported
	}
	return nil
}

// refuseLegacyNamedParameters rejects an InterBase execution request that
// carries genuine named markers but no submitted values, before the raw
// ":NAME" text can reach the driver as if it were SQL.
//
// It deliberately only claims text the parameter compiler fully accepts.
// Anything it rejects — a PSQL body whose ":V" is a local variable, DDL, or a
// bare "?" — keeps the unparameterized path it has always taken, with the
// driver as the authority on it.
func (s *Server) refuseLegacyNamedParameters(text string) error {
	variant := s.parserDriverVariant()
	if variant.Driver != dialect.DatabaseDriverInterBase {
		return nil
	}
	batch, err := queryparams.Compile(text, variant.Variant.InterBaseSQLDialect())
	if err != nil || len(batch.Parameters) == 0 {
		return nil
	}

	names := make([]string, 0, len(batch.Parameters))
	for _, parameter := range batch.Parameters {
		names = append(names, ":"+parameter.Name)
	}
	return fmt.Errorf(
		"this statement has named parameters (%s) but no values were submitted; "+
			"run it from a client that supports the %q command so the values can be entered and bound",
		strings.Join(names, ", "), CommandGetQueryParameters)
}
