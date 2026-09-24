package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

func (s *Server) handleTextDocumentSignatureHelp(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.SignatureHelpParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	res, err := SignatureHelpWithDriverVariant(snapshot.Text, params, snapshot.Cache, snapshot.Variant)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func SignatureHelp(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache) (*lsp.SignatureHelp, error) {
	return SignatureHelpWithDriver(text, params, dbCache, "")
}

func SignatureHelpWithDriver(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (*lsp.SignatureHelp, error) {
	return SignatureHelpWithDriverVariant(text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

func SignatureHelpWithDriverVariant(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.SignatureHelp, error) {
	if dbCache == nil {
		return nil, nil
	}

	parsed, err := parser.ParseWithDriverVariant(text, dv)
	if err != nil {
		return nil, err
	}

	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character,
	}
	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	types := getSignatureHelpTypes(nodeWalker)

	switch {
	case procedureCallSignature(nodeWalker, pos, dbCache) != nil:
		// Keyed on "the callee is a known procedure" rather than on a
		// preceding EXECUTE PROCEDURE, so the same code serves
		// SELECT * FROM MYPROC(?), which is the other place procedure
		// arguments are typed. CheckSyntaxPosition is not consulted: it is
		// position-shaped and this is a node fact.
		return procedureCallSignature(nodeWalker, pos, dbCache), nil
	case signatureHelpIs(types, SignatureHelpTypeInsertValue):
		insert, err := parseutil.ExtractInsert(parsed, pos)
		if err != nil {
			return nil, err
		}
		if !insert.Enable() {
			return nil, err
		}

		table := insert.GetTable()
		cols := insert.GetColumns()
		paramIdx := insert.GetValues().GetIndex(pos)
		tableName := table.Name

		params := []lsp.ParameterInformation{}
		for _, col := range cols.GetIdentifiers() {
			colName := col.String()
			colDoc := ""
			colDesc, ok := dbCache.Column(tableName, colName)
			if ok {
				colDoc = colDesc.OnelineDesc()
			}
			p := lsp.ParameterInformation{
				Label:         colName,
				Documentation: colDoc,
			}
			params = append(params, p)
		}

		signatureLabel := fmt.Sprintf("%s (%s)", tableName, cols.String())
		sh := &lsp.SignatureHelp{
			Signatures: []lsp.SignatureInformation{
				{
					Label:         signatureLabel,
					Documentation: fmt.Sprintf("%s table columns", tableName),
					Parameters:    params,
				},
			},
			ActiveSignature: 0.0,
			ActiveParameter: float64(paramIdx),
		}
		return sh, nil
	default:
		// pass
		return nil, nil
	}
}

// procedureCallSignature returns signature help when the cursor is inside the
// argument list of a call whose callee is a cached procedure, and nil
// otherwise. It is nil-safe on every input, which is what lets the switch
// above use it as a guard.
func procedureCallSignature(nw *parseutil.NodeWalker, pos token.Pos, dbCache *database.DBCache) *lsp.SignatureHelp {
	if !dbCache.HasCatalog() {
		return nil
	}
	call, ok := parseutil.EnclosingCall(nw)
	if !ok || !call.Inside {
		return nil
	}
	// The accessor normalises the name it is given; the identifier text goes
	// in exactly as the user typed it.
	desc, ok := dbCache.Procedure(call.Callee)
	if !ok {
		return nil
	}
	return procedureSignatureHelp(desc, call.ActiveParameter(pos))
}

// procedureSignatureHelp renders the tooltip. Output parameters are not
// arguments, so they are not offered as signature parameters, but their count
// appears in the documentation so the user can tell a selectable procedure
// from an executable one.
func procedureSignatureHelp(desc *database.ProcedureDesc, activeParameter int) *lsp.SignatureHelp {
	params := []lsp.ParameterInformation{}
	for _, param := range desc.InputParameters {
		params = append(params, lsp.ParameterInformation{
			Label:         param.Name,
			Documentation: database.ParameterDoc(param),
		})
	}
	return &lsp.SignatureHelp{
		Signatures: []lsp.SignatureInformation{
			{
				Label:         database.ProcedureSignatureLabel(desc),
				Documentation: database.ProcedureSignatureDoc(desc),
				Parameters:    params,
			},
		},
		ActiveSignature: 0.0,
		ActiveParameter: float64(activeParameter),
	}
}

type signatureHelpType int

const (
	_ signatureHelpType = iota
	SignatureHelpTypeInsertValue
	SignatureHelpTypeExecuteProcedure
	SignatureHelpTypeUnknown = 99
)

func (sht signatureHelpType) String() string {
	switch sht {
	case SignatureHelpTypeInsertValue:
		return "InsertValue"
	case SignatureHelpTypeExecuteProcedure:
		return "ExecuteProcedure"
	default:
		return ""
	}
}

func getSignatureHelpTypes(nw *parseutil.NodeWalker) []signatureHelpType {
	syntaxPos := parseutil.CheckSyntaxPosition(nw)
	types := []signatureHelpType{}
	switch {
	case syntaxPos == parseutil.InsertValue:
		types = []signatureHelpType{
			SignatureHelpTypeInsertValue,
		}
	default:
		// pass
	}
	return types
}

func signatureHelpIs(types []signatureHelpType, expect signatureHelpType) bool {
	for _, t := range types {
		if t == expect {
			return true
		}
	}
	return false
}
