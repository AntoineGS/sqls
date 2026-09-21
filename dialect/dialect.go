package dialect

type Dialect interface {
	IsIdentifierStart(r rune) bool
	IsIdentifierPart(r rune) bool
	IsDelimitedIdentifierStart(r rune) bool
	IsPlaceHolderStart(r rune) bool
	IsPlaceHolderPart(r rune) bool
}

type GenericSQLDialect struct {
}

// DialectForDriver returns the lexical rules for a driver's default variant.
// It is retained with its exact signature; callers that have resolved a
// server-side variant should use DialectForDriverVariant instead.
func DialectForDriver(driver DatabaseDriver) Dialect {
	return DialectForDriverVariant(DriverVariant{Driver: driver})
}

func (*GenericSQLDialect) IsIdentifierStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '@'
}

func (*GenericSQLDialect) IsIdentifierPart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '@' || r == '_'
}

func (*GenericSQLDialect) IsDelimitedIdentifierStart(r rune) bool {
	return r == '"' || r == '`'
}

func (*GenericSQLDialect) IsPlaceHolderStart(r rune) bool {
	return r == '$'
}

func (*GenericSQLDialect) IsPlaceHolderPart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

var _ Dialect = &GenericSQLDialect{}
