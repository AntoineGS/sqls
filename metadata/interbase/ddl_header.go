package ibmeta

import (
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/formatter"
	"github.com/sqls-server/sqls/internal/lsp"
)

// FormatDDLHeader lays out a generated DDL header the way the SQL formatter
// lays out the same statement, so the shown header and a formatted one agree.
func FormatDDLHeader(header string, sqlDialect int) (string, error) {
	params := lsp.DocumentFormattingParams{Options: lsp.FormattingOptions{TabSize: 2, InsertSpaces: true}}
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.InterBaseSQLVariant(sqlDialect)}
	edits, err := formatter.FormatWithDriverVariant(header, params, &config.Config{}, variant)
	if err != nil {
		return "", err
	}
	// The header has no surrounding whitespace, so the one edit spans it all.
	return edits[0].NewText, nil
}

func formatDDLHeader(header string, sqlDialect int) (string, error) {
	return FormatDDLHeader(header, sqlDialect)
}
