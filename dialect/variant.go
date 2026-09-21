package dialect

// SQLVariant identifies a server-side SQL variant within one driver. The empty
// variant selects the driver's default. It exists because a single driver can
// speak more than one dialect of its server's SQL — InterBase SQL Dialect 1 and
// Dialect 3 differ in whether a double quote delimits a string or an identifier.
type SQLVariant string

const (
	SQLVariantDefault    SQLVariant = ""
	SQLVariantInterBase1 SQLVariant = "interbase-dialect-1"
	SQLVariantInterBase3 SQLVariant = "interbase-dialect-3"
)

// DriverVariant pairs a driver with the variant resolved for a connection.
// The zero value selects the driver's default variant.
type DriverVariant struct {
	Driver  DatabaseDriver
	Variant SQLVariant
}

// InterBaseSQLVariant maps an InterBase SQL dialect number to its variant.
// Zero maps to the driver default, dialect 3.
func InterBaseSQLVariant(sqlDialect int) SQLVariant {
	if sqlDialect == 1 {
		return SQLVariantInterBase1
	}
	return SQLVariantInterBase3
}

// InterBaseSQLDialect is the inverse of InterBaseSQLVariant; it returns 1 or 3.
// Every variant other than SQLVariantInterBase1 — including the default and any
// unrecognized value — resolves to 3, matching the driver's normalizeDialect.
func (v SQLVariant) InterBaseSQLDialect() int {
	if v == SQLVariantInterBase1 {
		return 1
	}
	return 3
}

// DialectForDriverVariant returns the lexical rules for a driver and variant.
func DialectForDriverVariant(dv DriverVariant) Dialect {
	if dv.Driver == DatabaseDriverInterBase {
		return &InterBaseDialect{SQLDialect: dv.Variant.InterBaseSQLDialect()}
	}
	return &GenericSQLDialect{}
}

// DataBaseKeywordsForVariant returns the completion keyword list for a driver
// and variant. Only InterBase distinguishes variants today.
func DataBaseKeywordsForVariant(dv DriverVariant) []string {
	if dv.Driver == DatabaseDriverInterBase && dv.Variant.InterBaseSQLDialect() == 1 {
		return interbaseKeywords
	}
	return dataBaseKeywords(dv.Driver)
}

// DataBaseFunctionsForVariant returns the completion function list for a driver
// and variant. No driver distinguishes variants today; the seam exists so a
// caller can pass a DriverVariant uniformly.
func DataBaseFunctionsForVariant(dv DriverVariant) []string {
	return dataBaseFunctions(dv.Driver)
}
