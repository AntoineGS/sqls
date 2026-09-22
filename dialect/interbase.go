package dialect

import (
	"sort"
	"strings"
)

// InterBaseDialect implements the lexical rules of one InterBase SQL dialect.
// Dialect 1 uses double quotes for string literals; Dialect 3 uses them for
// delimited identifiers. Both permit '$' in regular identifiers (for example,
// RDB$DATABASE) and use positional '?' placeholders.
//
// The zero value is Dialect 3, matching the interbase-go default: the driver's
// normalizeDialect maps a zero Config.Dialect to 3, so a zero-value dialect and
// a zero-value interbase.Config agree.
type InterBaseDialect struct {
	// SQLDialect is 1 or 3; zero is treated as 3.
	SQLDialect int
}

func (*InterBaseDialect) IsIdentifierStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func (*InterBaseDialect) IsIdentifierPart(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_' || r == '$'
}

func (d *InterBaseDialect) IsDelimitedIdentifierStart(r rune) bool {
	return r == '"' && d.SQLDialect != 1
}

// PreservesQuotedStringEscapes keeps doubled quotes in token text for both
// dialects, because the formatter reprints tokens verbatim. Without it, turning
// on delimited identifiers for Dialect 3 would make the formatter drop one
// quote from an escaped string literal and corrupt user SQL.
func (d *InterBaseDialect) PreservesQuotedStringEscapes() bool { return true }

// ScansWholeDelimitedIdentifier keeps `"My Column"` a single identifier token
// and keeps a doubled quote inside one. Dialect 1 never reaches the delimited
// identifier path, so returning true unconditionally is safe for both.
func (d *InterBaseDialect) ScansWholeDelimitedIdentifier() bool { return true }

func (*InterBaseDialect) IsPlaceHolderStart(r rune) bool {
	return r == '?'
}

func (*InterBaseDialect) IsPlaceHolderPart(r rune) bool {
	return false
}

// MatchKeyword adds the InterBase-specific words to the common SQL keyword
// table without changing how those words are parsed for other backends.
func (*InterBaseDialect) MatchKeyword(upperWord string) KeywordKind {
	if kind, ok := interbaseKeywordKinds[upperWord]; ok {
		return kind
	}
	return MatchKeyword(upperWord)
}

var interbaseKeywordKinds = map[string]KeywordKind{
	"ACTIVE":           Matched,
	"ASCENDING":        Matched,
	"BEFORE":           Matched,
	"COMPUTED":         Matched,
	"CONTAINING":       Matched,
	"DESCENDING":       Matched,
	"ENTRY_POINT":      Matched,
	"EXCEPTION":        Matched,
	"GENERATOR":        Matched,
	"INACTIVE":         Matched,
	"MANUAL":           Matched,
	"PLAN":             Matched,
	"POST_EVENT":       Matched,
	"RECORD_VERSION":   Matched,
	"RETURNING_VALUES": Matched,
	"SHADOW":           Matched,
	"SUSPEND":          Matched,
	"TRIGGER":          Matched,
	"UNCOMMITTED":      Matched,
	"VARIABLE":         Matched,
	"WAIT":             Matched,
	"WEEK":             Matched,
	"WORK":             Matched,
	"WRITE":            Matched,
	"YEAR":             Matched,
}

// InterBase keywords include the common SQL words used by the server and
// the Dialect 1 extensions most useful while editing a query.
var interbaseKeywords = []string{
	"ACTIVE",
	"ADD",
	"ADMIN",
	"AFTER",
	"ALL",
	"ALTER",
	"AND",
	"ANY",
	"AS",
	"ASC",
	"ASCENDING",
	"AT",
	"BEFORE",
	"BEGIN",
	"BETWEEN",
	"BLOB",
	"BOOLEAN",
	"BY",
	"CASE",
	"CAST",
	"CHAR",
	"CHARACTER",
	"CHECK",
	"CLOSE",
	"COLLATE",
	"COLUMN",
	"COMMIT",
	"COMPUTED",
	"CONNECT",
	"CONSTRAINT",
	"CONTAINING",
	"CREATE",
	"CROSS",
	"CURRENT",
	"CURRENT_DATE",
	"CURRENT_TIME",
	"CURRENT_TIMESTAMP",
	"CURRENT_USER",
	"CURSOR",
	"DATABASE",
	"DATE",
	"DAY",
	"DEC",
	"DECIMAL",
	"DECLARE",
	"DEFAULT",
	"DELETE",
	"DESC",
	"DESCENDING",
	"DISTINCT",
	"DO",
	"DOMAIN",
	"DROP",
	"ELSE",
	"END",
	"ENTRY_POINT",
	"ESCAPE",
	"EXCEPTION",
	"EXECUTE",
	"EXISTS",
	"EXIT",
	"EXTERNAL",
	"FILTER",
	"FLOAT",
	"FOR",
	"FOREIGN",
	"FROM",
	"FULL",
	"GENERATOR",
	"GRANT",
	"GROUP",
	"HAVING",
	"HOUR",
	"IF",
	"IN",
	"INACTIVE",
	"INDEX",
	"INNER",
	"INSERT",
	"INTEGER",
	"INTO",
	"IS",
	"JOIN",
	"KEY",
	"LAST",
	"LEADING",
	"LEFT",
	"LIKE",
	"LONG",
	"MANUAL",
	"MAX",
	"MIN",
	"MINUTE",
	"MONTH",
	"NATIONAL",
	"NATURAL",
	"NCHAR",
	"NO",
	"NOT",
	"NULL",
	"NUMERIC",
	"OF",
	"ON",
	"ONLY",
	"OPEN",
	"OR",
	"ORDER",
	"OUTER",
	"PARAMETER",
	"PLAN",
	"POST_EVENT",
	"PRECISION",
	"PRIMARY",
	"PROCEDURE",
	"RECORD_VERSION",
	"REFERENCES",
	"RETAIN",
	"RETURNING_VALUES",
	"RETURNS",
	"REVOKE",
	"RIGHT",
	"ROLLBACK",
	"ROWS",
	"SAVEPOINT",
	"SECOND",
	"SELECT",
	"SET",
	"SHADOW",
	"SMALLINT",
	"SOME",
	"SORT",
	"SQL",
	"START",
	"SUBSTRING",
	"SUSPEND",
	"TABLE",
	"THEN",
	"TO",
	"TRAILING",
	"TRANSACTION",
	"TRIGGER",
	"UNCOMMITTED",
	"UNION",
	"UNIQUE",
	"UPDATE",
	"USER",
	"USING",
	"VALUE",
	"VALUES",
	"VARCHAR",
	"VARIABLE",
	"VARYING",
	"VIEW",
	"WHEN",
	"WHERE",
	"WHILE",
	"WITH",
	"WORK",
	"WRITE",
	"YEAR",
}

// IsInterBaseReservedWord reports whether word is reserved by InterBase's SQL
// syntax for variant. It deliberately uses the dedicated vendor list below,
// rather than the broader completion inventory in interbaseKeywords.
func IsInterBaseReservedWord(word string, variant SQLVariant) bool {
	word = strings.ToUpper(word)
	if variant == SQLVariantInterBase1 && (word == "TIME" || word == "TIMESTAMP") {
		// Dialect 1 keeps the pre-Dialect-3 type vocabulary. The vendor
		// appendix lists these words, but they remain unavailable as Dialect 1
		// types and are kept out of the Dialect 1 rename vocabulary.
		return false
	}
	if interbaseReservedWords[word] {
		return true
	}
	return false
}

// InterBase 2020 Language Reference Guide, Appendix "InterBase Keywords":
// https://docwiki.embarcadero.com/docs/products/interbase/2020/LangRef.pdf
// The vendor list is intentionally separate from interbaseKeywords: the latter
// is a completion-oriented inventory and must not define rename validity.
var interbaseReservedWords = func() map[string]bool {
	words := strings.Fields(`
ACTION ACTIVE ADD ADMIN AFTER ALL ALTER AND ANY AS ASC ASCENDING AT AUTO AUTODDL AVG
BASED BASENAME BASE_NAME BEFORE BEGIN BETWEEN BLOB BLOBEDIT BOOLEAN BUFFER BY
CACHE CASCADE CASE CAST CHAR CHARACTER CHARACTER_LENGTH CHAR_LENGTH CHECK
CHECK_POINT_LEN CHECK_POINT_LENGTH COALESCE COLLATE COLLATION COLUMN COMMIT
COMMITTED COMPILETIME COMPUTED CLOSE CONDITIONAL CONNECT CONSTRAINT CONTAINING
CONTINUE COUNT CREATE CSTRING CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP
CURSOR DATABASE DATE DAY DB_KEY DEBUG DEC DECIMAL DECLARE DECRYPT DEFAULT DELETE
DESC DESCENDING DESCRIBE DESCRIPTOR DISCONNECT DISPLAY DISTINCT DO DOMAIN DOUBLE
DROP ECHO EDIT ELSE ENCRYPT ENCRYPTION END ENTRY_POINT ESCAPE EVENT EXCEPTION
EXECUTE EXISTS EXIT EXTERN EXTERNAL EXTRACT FALSE FETCH FILE FILTER FLOAT FOR
FOREIGN FOUND FREE_IT FROM FULL FUNCTION GDSCODE GENERATOR GEN_ID GLOBAL GOTO
GRANT GROUP GROUP_COMMIT_WAIT GROUP_COMMIT_WAIT_TIME HAVING HELP HOUR IF IMMEDIATE
IN INACTIVE INDEX INDICATOR INIT INNER INPUT INPUT_TYPE INSERT INT INTEGER INTO IS
ISOLATION ISQL JOIN KEY LC_MESSAGES LC_TYPE LEFT LENGTH LEV LEVEL LIKE LOGFILE
LOG_BUFFER_SIZE LOG_BUF_SIZE LONG MANUAL MAX MAXIMUM MAXIMUM_SEGMENT MAX_SEGMENT
MERGE MESSAGE MIN MINIMUM MINUTE MODULE_NAME MONTH NAMES NATIONAL NATURAL NCHAR
NO NOAUTO NOT NULL NULLIF NUMERIC NUM_LOG_BUFS NUM_LOG_BUFFERS OCTET_LENGTH OF
ON ONLY OPEN OPTION OR ORDER OUTER OUTPUT OUTPUT_TYPE OVERFLOW PAGE PAGELENGTH
PAGES PAGE_SIZE PARAMETERS PASSWORD PERCENT PLAN POSITION POST_EVENT PRECISION
PREPARE PRESERVE PROCEDURE PROTECTED PRIMARY PRIVILEGES PUBLIC QUIT RAW_PARTITIONS
RDB$DB_KEY READ REAL RECORD_VERSION REFERENCES RELEASE RESERV RESERVING RESTRICT
RETAIN RETURN RETURNING_VALUES RETURNS REVOKE RIGHT ROLE ROLLBACK ROW ROWS RUNTIME
SCHEMA SECOND SEGMENT SELECT SET SHADOW SHARED SHELL SHOW SINGULAR SIZE SMALLINT
SNAPSHOT SOME SORT SQLCODE SQLERROR SQLWARNING STABILITY STARTING STARTS STATEMENT
STATIC SUSPEND TABLE TEMPORARY TERMINATOR THEN TIES TIME TIMESTAMP TO TRANSACTION
TRANSLATE TRANSLATION TRIGGER TRIM TRUE TYPE UNCOMMITTED UNION UNIQUE UNKNOWN UPDATE
UPPER USER USING VALUE VALUES VARCHAR VARIABLE VARYING VERSION VIEW WAIT WEEKDAY
WHEN WHENEVER WHERE WHILE WITH WORK WRITE YEAR YEARDAY`)
	result := make(map[string]bool, len(words))
	for _, word := range words {
		result[word] = true
	}
	return result
}()

// interbaseDialect3Keywords is interbaseKeywords plus the two types that exist
// only in SQL Dialect 3. Deriving it from the Dialect 1 list keeps one source
// of truth, so a word added to interbaseKeywords reaches both dialects.
var interbaseDialect3Keywords = func() []string {
	words := make([]string, 0, len(interbaseKeywords)+2)
	words = append(words, interbaseKeywords...)
	words = append(words, "TIME", "TIMESTAMP")
	sort.Strings(words)
	return words
}()

// Keep this list to core InterBase functions. Installation-specific UDFs and
// Firebird builtins are not necessarily available on an InterBase server.
var interbaseFunctions = []string{
	"AVG",
	"CAST",
	"COALESCE",
	"COUNT",
	"EXTRACT",
	"GEN_ID",
	"MAX",
	"MIN",
	"NULLIF",
	"SUBSTRING",
	"SUM",
	"UPPER",
}

var _ Dialect = (*InterBaseDialect)(nil)
