package language_test

import (
	"testing"

	"github.com/sqls-server/sqls/language"
)

func TestParseServerMessageExtractsPositionsAndTrailingNames(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		message string
		line    int
		column  int
		token   string
	}{
		{
			name:    "syntax token with line and char",
			code:    -104,
			message: "Dynamic SQL Error\nSQL error code = -104\nToken unknown - line 7, char 3\n_SELECT",
			line:    7,
			column:  3,
			token:   "_SELECT",
		},
		{
			name:    "unexpected end has no token",
			code:    -104,
			message: "...\nUnexpected end of command",
		},
		{
			name:    "unknown table trailing name",
			code:    -204,
			message: "Dynamic SQL Error\nSQL error code = -204\nTable unknown\nNO_SUCH_TABLE",
			token:   "NO_SUCH_TABLE",
		},
		{
			name:    "column name before at-line position",
			code:    -206,
			message: "...\nColumn unknown\nORDERID\nAt line 1, column 30",
			line:    1,
			column:  30,
			token:   "ORDERID",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := language.ParseServerMessage(test.code, test.message)
			if got.SQLCode != test.code || got.Message != test.message {
				t.Fatalf("ParseServerMessage() preserved SQLCode/Message as (%d, %q), want (%d, %q)",
					got.SQLCode, got.Message, test.code, test.message)
			}
			if got.Line != test.line || got.Column != test.column || got.Token != test.token {
				t.Fatalf("ParseServerMessage() = line %d, column %d, token %q; want line %d, column %d, token %q",
					got.Line, got.Column, got.Token, test.line, test.column, test.token)
			}
		})
	}
}
