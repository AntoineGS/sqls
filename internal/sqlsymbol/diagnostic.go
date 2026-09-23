package sqlsymbol

// ColumnType describes a catalog column using its SQL type declaration.
type ColumnType struct {
	Name string
	Type string
}

// Catalog provides read-only table column metadata for diagnostics.
type Catalog interface {
	Columns(table Name) ([]ColumnType, bool)
}

// Finding is a source-level diagnostic produced by the symbol analysis.
type Finding struct {
	Span     Span
	Code     string
	Message  string
	Severity int
}

// Diagnostics reports statically proven InterBase findings. Catalog supplies
// read-only table widths for assignment diagnostics; unused-symbol analysis
// does not require it.
func (a *Analysis) Diagnostics(c Catalog) []Finding {
	findings := make([]Finding, 0)
	for _, symbol := range a.Symbols {
		if symbol.Kind == OutputParameter || len(symbol.Reads) != 0 || symbol.RenameBlocked != "" {
			continue
		}
		findings = append(findings, Finding{
			Span:     symbol.Declaration,
			Code:     "interbase-unused",
			Message:  "Unused declaration",
			Severity: 4,
		})
	}
	return append(findings, a.widthDiagnostics(c)...)
}
