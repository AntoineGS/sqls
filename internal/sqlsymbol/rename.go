package sqlsymbol

import (
	"fmt"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

// Rename returns edits for one declaration and all of its bound occurrences.
// The symbol must belong to this request-local analysis; accepting a symbol
// from another analysis would make a rename silently edit the wrong document.
func (a *Analysis) Rename(symbol *Symbol, newName string) ([]Edit, error) {
	if a == nil {
		return nil, fmt.Errorf("cannot rename with a nil analysis")
	}
	if symbol == nil {
		return nil, fmt.Errorf("cannot rename a nil symbol")
	}
	procedureIndex := -1
	for i := range a.procedures {
		for _, symbols := range a.procedures[i].Symbols {
			for _, candidate := range symbols {
				if candidate == symbol {
					procedureIndex = i
					break
				}
			}
			if procedureIndex >= 0 {
				break
			}
		}
		if procedureIndex >= 0 {
			break
		}
	}
	if procedureIndex < 0 {
		return nil, fmt.Errorf("cannot rename a symbol from another analysis")
	}

	name, err := renameName(newName, a.Variant)
	if err != nil {
		return nil, err
	}
	for _, candidate := range a.procedures[procedureIndex].Symbols[name.Key()] {
		if candidate != symbol {
			return nil, fmt.Errorf("cannot rename %s to %s: name collides with another declaration", symbol.Name.Text, newName)
		}
	}
	if symbol.RenameBlocked != "" {
		return nil, fmt.Errorf("cannot rename %s: %s", symbol.Name.Text, symbol.RenameBlocked)
	}

	spans := a.References(symbol, true)
	edits := make([]Edit, 0, len(spans))
	for _, span := range spans {
		edits = append(edits, Edit{Span: span, NewText: newName})
	}
	return edits, nil
}

func renameName(source string, dv dialect.DriverVariant) (Name, error) {
	items, err := lex(source, dv)
	if err != nil {
		return Name{}, fmt.Errorf("invalid rename: %w", err)
	}
	if len(items) != 1 || items[0].Span.Start != 0 || items[0].Span.End != len(source) || items[0].Token.Kind != token.SQLKeyword {
		return Name{}, fmt.Errorf("invalid rename %q: expected one identifier", source)
	}
	name, ok := nameFromLexeme(source, items[0])
	if !ok {
		return Name{}, fmt.Errorf("invalid rename %q: expected one identifier", source)
	}
	if name.Text == "" {
		return Name{}, fmt.Errorf("invalid rename %q: identifier cannot be empty", source)
	}
	word, _ := items[0].Token.Value.(*token.SQLWord)
	// The dialect helper is backed by InterBase's syntax keyword list, not the
	// broad completion inventory, which can include valid function-like names.
	if !name.Quoted && dialect.IsInterBaseReservedWord(word.Keyword, dv.Variant) {
		return Name{}, fmt.Errorf("invalid rename %q: reserved InterBase keyword", source)
	}
	return name, nil
}
