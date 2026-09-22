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

// parameterSelection is the pure input the discovery and (future)
// submission commands both need: the selected SQL text, its identity for
// stale-submission detection, and the driver variant to compile it under.
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
			Version:              1,
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
