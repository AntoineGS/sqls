package dialect

import (
	"strings"
	"testing"
)

func TestInterBaseDialectLexicalRulesBySQLDialect(t *testing.T) {
	tests := []struct {
		name               string
		sqlDialect         int
		wantDelimitedIdent bool
	}{
		{name: "zero means dialect 3", sqlDialect: 0, wantDelimitedIdent: true},
		{name: "dialect 1", sqlDialect: 1, wantDelimitedIdent: false},
		{name: "dialect 3", sqlDialect: 3, wantDelimitedIdent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &InterBaseDialect{SQLDialect: tt.sqlDialect}

			if got := d.IsDelimitedIdentifierStart('"'); got != tt.wantDelimitedIdent {
				t.Errorf("IsDelimitedIdentifierStart('\"') = %v, want %v", got, tt.wantDelimitedIdent)
			}
			if d.IsDelimitedIdentifierStart('`') {
				t.Error("InterBase never delimits identifiers with a back quote")
			}

			// Everything below is dialect independent.
			if !d.IsIdentifierPart('$') {
				t.Error("InterBase identifiers should allow '$' after the first character")
			}
			if d.IsIdentifierStart('$') {
				t.Error("'$' should not start an InterBase identifier")
			}
			if !d.IsIdentifierStart('a') || !d.IsIdentifierStart('Z') {
				t.Error("InterBase identifiers should start with a letter")
			}
			if !d.IsPlaceHolderStart('?') {
				t.Error("InterBase should accept positional '?' placeholders")
			}
			if d.IsPlaceHolderStart('$') {
				t.Error("InterBase should not use '$' as a placeholder")
			}
			if d.IsPlaceHolderPart('1') {
				t.Error("InterBase placeholders have no parts")
			}
			if got := d.MatchKeyword("GENERATOR"); got != Matched {
				t.Errorf("MatchKeyword(GENERATOR) = %v, want Matched", got)
			}
			if !d.PreservesQuotedStringEscapes() {
				t.Error("both InterBase dialects must preserve doubled quotes verbatim")
			}
			if !d.ScansWholeDelimitedIdentifier() {
				t.Error("both InterBase dialects must scan a delimited identifier whole")
			}
		})
	}
}

func TestDatabaseDriverInterBasePlumbing(t *testing.T) {
	if got := DialectForDriver(DatabaseDriverInterBase); got == nil {
		t.Fatal("DialectForDriver returned nil for InterBase")
	} else if _, ok := got.(*InterBaseDialect); !ok {
		t.Fatalf("DialectForDriver(InterBase) = %T, want *InterBaseDialect", got)
	}
	if _, ok := DialectForDriver(DatabaseDriver("mock")).(*GenericSQLDialect); !ok {
		t.Fatal("unknown drivers should retain generic parsing")
	}
}

func TestInterBaseKeywordsAndFunctions(t *testing.T) {
	for _, word := range []string{"CONTAINING", "GENERATOR"} {
		if got := (&InterBaseDialect{}).MatchKeyword(word); got != Matched {
			t.Errorf("InterBase keyword %q kind = %v, want Matched", word, got)
		}
	}

	keywords := DataBaseKeywords(DatabaseDriverInterBase)
	for _, want := range []string{"SELECT", "ROWS", "GENERATOR"} {
		if !containsString(keywords, want) {
			t.Errorf("InterBase keywords do not contain %q", want)
		}
	}

	functions := DataBaseFunctions(DatabaseDriverInterBase)
	for _, want := range []string{"GEN_ID", "COUNT", "SUM", "UPPER"} {
		if !containsString(functions, want) {
			t.Errorf("InterBase functions do not contain %q", want)
		}
	}
	for _, unsupported := range []string{"FIRST", "SKIP", "AUTONOMOUS", "WITH_LOCK"} {
		if containsString(keywords, unsupported) {
			t.Errorf("InterBase completion must not suggest Firebird syntax %q", unsupported)
		}
	}
	for _, unsupported := range []string{"IIF", "RDB$GET_CONTEXT", "RDB$SET_CONTEXT", "TRIM", "GEN_UUID"} {
		if containsString(functions, unsupported) {
			t.Errorf("InterBase completion must not suggest unsupported builtin %q", unsupported)
		}
	}
}

func TestInterBaseReservedWordsUseDedicatedVendorSet(t *testing.T) {
	for _, word := range []string{"EXTRACT", "extract", "TYPE", "type", "WEEKDAY", "weekday", "YEARDAY", "yearday", "OPEN", "open", "FALSE", "false", "FETCH", "fetch", "OPTION", "option"} {
		if !IsInterBaseReservedWord(word, SQLVariantInterBase3) {
			t.Errorf("IsInterBaseReservedWord(%q) = false, want true", word)
		}
	}
	for _, word := range []string{"ABS", "abs", "DATEADD", "dateadd"} {
		if IsInterBaseReservedWord(word, SQLVariantInterBase3) {
			t.Errorf("IsInterBaseReservedWord(%q) = true, want valid non-reserved function-like word", word)
		}
	}
}

func TestInterBaseReservedWordsMatchVendorAppendix(t *testing.T) {
	want := make(map[string]bool)
	for _, word := range strings.Fields(`
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
WHEN WHENEVER WHERE WHILE WITH WORK WRITE YEAR YEARDAY`) {
		want[word] = true
	}

	if len(interbaseReservedWords) != len(want) {
		t.Fatalf("InterBase reserved-word set has %d words, want vendor appendix's %d", len(interbaseReservedWords), len(want))
	}
	for word := range want {
		if !interbaseReservedWords[word] {
			t.Errorf("InterBase reserved-word set omits vendor keyword %q", word)
		}
	}
	for word := range interbaseReservedWords {
		if !want[word] {
			t.Errorf("InterBase reserved-word set adds non-vendor keyword %q", word)
		}
	}
}

func TestInterBaseReservedWordsKeepDialectTypeSemantics(t *testing.T) {
	for _, word := range []string{"TIME", "time", "TIMESTAMP", "timestamp"} {
		if IsInterBaseReservedWord(word, SQLVariantInterBase1) {
			t.Errorf("Dialect 1 IsInterBaseReservedWord(%q) = true, want false", word)
		}
		if !IsInterBaseReservedWord(word, SQLVariantInterBase3) {
			t.Errorf("Dialect 3 IsInterBaseReservedWord(%q) = false, want true", word)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
