package sqlsymbol

import (
	"fmt"
	"strings"

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
	word, _ := items[0].Token.Value.(*token.SQLWord)
	if !name.Quoted && isInterBaseReserved(word.Keyword, dv) {
		return Name{}, fmt.Errorf("invalid rename %q: reserved InterBase keyword", source)
	}
	return name, nil
}

// This is the InterBase syntax-word set, rather than the completion inventory:
// completion also includes functions and other words that are valid names.
var interBaseReserved = func() map[string]bool {
	words := strings.Fields(`
ACTIVE ADD ADMIN AFTER ALL ALTER AND ANY ASC ASCENDING AT BEFORE BETWEEN BLOB
BOOLEAN BY CASE CAST CHARACTER CHECK CLOSE COLLATE COLUMN COMMIT COMPUTED
CONNECT CONSTRAINT CONTAINING CREATE CROSS CURRENT CURRENT_DATE CURRENT_TIME
CURRENT_TIMESTAMP CURRENT_USER CURSOR DATABASE DATE DAY DEC DECIMAL DECLARE
DEFAULT DELETE DESC DESCENDING DISTINCT DO DOMAIN DROP ELSE END ENTRY_POINT
ESCAPE EXCEPTION EXECUTE EXISTS EXIT EXTERNAL FILTER FLOAT FOR FOREIGN FROM
FULL GENERATOR GRANT GROUP HAVING HOUR IF IN INACTIVE INDEX INNER INSERT
INTEGER INTO IS JOIN KEY LAST LEADING LEFT LIKE LONG MANUAL MAX MIN MINUTE
MONTH NATIONAL NATURAL NCHAR NO NOT NULL NUMERIC OF ON ONLY OR ORDER OUTER
PARAMETER PLAN POST_EVENT PRECISION PRIMARY PROCEDURE RECORD_VERSION REFERENCES
RETAIN RETURNING_VALUES RETURNS REVOKE RIGHT ROLLBACK ROWS SAVEPOINT SECOND
SELECT SET SHADOW SMALLINT SOME SORT SQL START SUBSTRING SUSPEND TABLE THEN TO
TRAILING TRANSACTION TRIGGER UNCOMMITTED UNION UNIQUE UPDATE USER USING VALUE
VALUES VARCHAR VARIABLE VARYING VIEW WHEN WHERE WHILE WITH WORK WRITE YEAR`)
	result := make(map[string]bool, len(words)+2)
	for _, word := range words {
		result[word] = true
	}
	return result
}()

func isInterBaseReserved(word string, dv dialect.DriverVariant) bool {
	if word == "TIME" || word == "TIMESTAMP" {
		return dv.Variant != dialect.SQLVariantInterBase1
	}
	return interBaseReserved[strings.ToUpper(word)]
}
