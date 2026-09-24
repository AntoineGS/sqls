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

// UniqueKeyCatalog optionally supplies complete, enforced column keys. Each
// inner slice is one whole primary key or active unique index, using catalog
// spelling. A known table with no keys returns an empty slice and true;
// unavailable metadata (including unsupported relations) returns false.
type UniqueKeyCatalog interface {
	Catalog
	UniqueKeys(table Name) ([][]string, bool)
}

// Finding is a source-level diagnostic produced by the symbol analysis.
type Finding struct {
	Span     Span
	Code     string
	Message  string
	Severity int
}

// Diagnostics reports InterBase findings, including advisory warnings about
// possible truncation and singleton selections lacking a uniqueness guarantee.
// Catalog supplies read-only metadata; unused-symbol analysis does not need it.
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
	findings = append(findings, a.widthDiagnostics(c)...)
	findings = append(findings, a.singletonDiagnostics(c)...)
	return append(findings, a.diagnosticModel(c).localFindings()...)
}
