package language

import (
	"context"
	"fmt"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// SignatureHelp returns call-signature help for position in req.Document,
// answered entirely from req.Context.Snapshot's already-published Cache.
// A nil snapshot/Cache yields a zero Result with no signatures, matching a
// document with no catalog knowledge rather than an error.
func (s *Service) SignatureHelp(ctx context.Context, req Request, position Position) (Result[SignatureHelp], error) {
	if err := ctx.Err(); err != nil {
		return Result[SignatureHelp]{}, ErrCanceled
	}

	var cache *catalog.Cache
	if req.Context.Snapshot != nil {
		cache = req.Context.Snapshot.Cache
	}
	if cache == nil {
		return Result[SignatureHelp]{}, nil
	}

	text := req.Document.Text
	parsed, err := parser.ParseWithDriverVariant(text, req.Context.Variant)
	if err != nil {
		return Result[SignatureHelp]{}, err
	}

	pos := token.Pos{Line: position.Line, Col: position.UTF16Column}
	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	types := getSignatureHelpTypes(nodeWalker)

	var sh *SignatureHelp
	switch {
	case procedureCallSignature(nodeWalker, pos, cache) != nil:
		// Keyed on "the callee is a known procedure" rather than on a
		// preceding EXECUTE PROCEDURE, so the same code serves
		// SELECT * FROM MYPROC(?), which is the other place procedure
		// arguments are typed. CheckSyntaxPosition is not consulted: it is
		// position-shaped and this is a node fact.
		sh = procedureCallSignature(nodeWalker, pos, cache)
	case signatureHelpIs(types, SignatureHelpTypeInsertValue):
		insert, err := parseutil.ExtractInsert(parsed, pos)
		if err != nil {
			return Result[SignatureHelp]{}, err
		}
		if !insert.Enable() {
			return Result[SignatureHelp]{}, nil
		}

		table := insert.GetTable()
		cols := insert.GetColumns()
		paramIdx := insert.GetValues().GetIndex(pos)
		tableName := table.Name

		params := []Parameter{}
		for _, col := range cols.GetIdentifiers() {
			colName := col.String()
			colDoc := ""
			colDesc, ok := cache.Column(tableName, colName)
			if ok {
				colDoc = colDesc.OnelineDesc()
			}
			params = append(params, Parameter{Label: colName, Documentation: colDoc})
		}

		sh = &SignatureHelp{
			Signatures: []Signature{
				{
					Label:         fmt.Sprintf("%s (%s)", tableName, cols.String()),
					Documentation: fmt.Sprintf("%s table columns", tableName),
					Parameters:    params,
				},
			},
			ActiveSignature: 0,
			ActiveParameter: paramIdx,
		}
	default:
		return Result[SignatureHelp]{}, nil
	}

	return Result[SignatureHelp]{Value: *sh, Stamp: stampFor(req)}, nil
}

// procedureCallSignature returns signature help when the cursor is inside the
// argument list of a call whose callee is a cached procedure, and nil
// otherwise. It is nil-safe on every input, which is what lets the switch
// above use it as a guard.
func procedureCallSignature(nw *parseutil.NodeWalker, pos token.Pos, cache *catalog.Cache) *SignatureHelp {
	if !cache.HasCatalog() {
		return nil
	}
	call, ok := parseutil.EnclosingCall(nw)
	if !ok || !call.Inside {
		return nil
	}
	// The accessor normalises the name it is given; the identifier text goes
	// in exactly as the user typed it.
	desc, ok := cache.Procedure(call.Callee)
	if !ok {
		return nil
	}
	return procedureSignatureHelp(desc, call.ActiveParameter(pos))
}

// procedureSignatureHelp renders the tooltip. Output parameters are not
// arguments, so they are not offered as signature parameters, but their count
// appears in the documentation so the user can tell a selectable procedure
// from an executable one.
func procedureSignatureHelp(desc *catalog.ProcedureDesc, activeParameter int) *SignatureHelp {
	params := []Parameter{}
	for _, param := range desc.InputParameters {
		params = append(params, Parameter{
			Label:         param.Name,
			Documentation: catalog.ParameterDoc(param),
		})
	}
	return &SignatureHelp{
		Signatures: []Signature{
			{
				Label:         catalog.ProcedureSignatureLabel(desc),
				Documentation: catalog.ProcedureSignatureDoc(desc),
				Parameters:    params,
			},
		},
		ActiveSignature: 0,
		ActiveParameter: activeParameter,
	}
}

type signatureHelpType int

const (
	_ signatureHelpType = iota
	SignatureHelpTypeInsertValue
	SignatureHelpTypeExecuteProcedure
)

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
