package sqlsymbol

// Knowledge classifies how confidently a SemanticCatalog lookup answer is
// known. Unknown means the relevant metadata category has not finished
// loading, so absence proves nothing; Missing means that category is complete
// and the name genuinely is not in it; Present means the name resolved to a
// fact.
type Knowledge uint8

const (
	Unknown Knowledge = iota
	Missing
	Present
)

// Nullability classifies whether a catalog fact's value can be SQL NULL.
// NullUnknown means the catalog cannot determine nullability, distinct from a
// proven Nullable or NotNullable answer.
type Nullability uint8

const (
	NullUnknown Nullability = iota
	NotNullable
	Nullable
)

// ColumnFact describes one relation column or procedure parameter's catalog
// type and nullability.
type ColumnFact struct {
	Name, Type  string
	Nullability Nullability
}

// RelationFact describes a table, view, or other queryable relation.
// ColumnsKnown is true only when Columns is the relation's complete column
// list; a known relation whose columns have not finished loading reports
// ColumnsKnown=false with a nil Columns, independently of the relation's own
// existence being Present.
type RelationFact struct {
	Columns      []ColumnFact
	ColumnsKnown bool
}

// ProcedureFact describes a stored procedure's parameters. MinInputs is the
// fewest input arguments a call can supply, accounting for defaulted trailing
// parameters; MinInputsKnown is false when the catalog cannot determine that
// count even though the parameter list itself is known.
type ProcedureFact struct {
	Inputs, Outputs           []ColumnFact
	InputsKnown, OutputsKnown bool
	MinInputs                 int
	MinInputsKnown            bool
}

// DomainFact describes a user domain's underlying type and nullability.
type DomainFact struct {
	Type        string
	Nullability Nullability
}

// SemanticCatalog optionally supplies completeness-aware facts about user
// relations, procedures, and domains, distinguishing metadata that has not
// finished loading (Unknown) from a name proven absent from a complete
// namespace (Missing). System catalog objects (e.g. RDB$RELATIONS) always
// report Unknown: this catalog only claims knowledge of user objects.
type SemanticCatalog interface {
	Catalog
	RelationInfo(name Name) (RelationFact, Knowledge)
	ProcedureInfo(name Name) (ProcedureFact, Knowledge)
	DomainInfo(name Name) (DomainFact, Knowledge)
}
