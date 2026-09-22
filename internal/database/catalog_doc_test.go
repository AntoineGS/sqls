package database

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"
)

func testProcedureDesc() *ProcedureDesc {
	return &ProcedureDesc{
		Name:        "MYPROC",
		Description: sql.NullString{String: "Totals an order.", Valid: true},
		Source:      sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
		InputParameters: []*ProcedureParameterDesc{
			{Name: "IN_CODE", Position: 0, Direction: ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
			{Name: "IN_AMOUNT", Position: 1, Direction: ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
		},
		OutputParameters: []*ProcedureParameterDesc{
			{Name: "OUT_TOTAL", Position: 0, Direction: ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
		},
	}
}

func TestParameterDocRendersNotNullOnlyWhenKnownFalse(t *testing.T) {
	cases := []struct {
		name  string
		param *ProcedureParameterDesc
		want  string
	}{
		{
			name:  "known not null",
			param: &ProcedureParameterDesc{Name: "IN_CODE", Direction: ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
			want:  "`VARCHAR(3)` input NOT NULL",
		},
		{
			// The normal case: RDB$PROCEDURE_PARAMETERS has no declaration
			// nullability flag, so Valid is false. Rendering "nullable" here
			// would assert a fact the catalog does not contain.
			name:  "unknown nullability renders nothing",
			param: &ProcedureParameterDesc{Name: "IN_AMOUNT", Direction: ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
			want:  "`NUMERIC(18, 2)` input",
		},
		{
			name:  "known nullable renders nothing",
			param: &ProcedureParameterDesc{Name: "IN_NOTE", Direction: ParameterInput, Type: "VARCHAR(80)", Nullable: sql.NullBool{Bool: true, Valid: true}},
			want:  "`VARCHAR(80)` input",
		},
		{
			name:  "unrenderable type omits the type element",
			param: &ProcedureParameterDesc{Name: "IN_BLOB", Direction: ParameterInput, Type: "", Nullable: sql.NullBool{}},
			want:  "input",
		},
		{
			name:  "output direction",
			param: &ProcedureParameterDesc{Name: "OUT_TOTAL", Direction: ParameterOutput, Type: "INTEGER"},
			want:  "`INTEGER` output",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ParameterDoc(tt.param)
			if got != tt.want {
				t.Errorf("ParameterDoc = %q, want %q", got, tt.want)
			}
			// Guard against a renderer that invents a word for the unknown
			// case. This is the assertion that fails against a naive
			// implementation that maps Nullable to "nullable"/"unknown".
			for _, forbidden := range []string{"nullable", "NULLABLE", "unknown", "<unknown>", "NULL "} {
				if strings.Contains(got, forbidden) {
					t.Errorf("ParameterDoc = %q, must not contain %q", got, forbidden)
				}
			}
		})
	}
}

func TestProcedureDocRendersParametersDescriptionAndSource(t *testing.T) {
	got := ProcedureDoc(testProcedureDesc())

	for _, want := range []string{
		"`MYPROC` procedure",
		"Totals an order.",
		"- IN_CODE: `VARCHAR(3)` input NOT NULL",
		"- IN_AMOUNT: `NUMERIC(18, 2)` input",
		"- OUT_TOTAL: `INTEGER` output",
		"BEGIN\n  SUSPEND;\nEND",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ProcedureDoc missing %q:\n%s", want, got)
		}
	}
	// The catalog source is preserved verbatim and never wrapped in a
	// synthesized CREATE header.
	if strings.Contains(got, "CREATE PROCEDURE") {
		t.Errorf("ProcedureDoc fabricated a CREATE header:\n%s", got)
	}
}

func TestProcedureDocOmitsAbsentSections(t *testing.T) {
	got := ProcedureDoc(&ProcedureDesc{Name: "DOWORK"})

	if !strings.Contains(got, "`DOWORK` procedure") {
		t.Errorf("ProcedureDoc lost the name:\n%s", got)
	}
	for _, forbidden := range []string{"Input parameters", "Output parameters", "Source"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("ProcedureDoc rendered an empty %q section:\n%s", forbidden, got)
		}
	}
}

func TestProcedureSignatureLabelAndDoc(t *testing.T) {
	desc := testProcedureDesc()
	if got, want := ProcedureSignatureLabel(desc), "MYPROC (IN_CODE, IN_AMOUNT)"; got != want {
		t.Errorf("ProcedureSignatureLabel = %q, want %q", got, want)
	}
	if got, want := ProcedureSignatureDoc(desc), "MYPROC procedure — 2 input parameters, 1 output parameter"; got != want {
		t.Errorf("ProcedureSignatureDoc = %q, want %q", got, want)
	}
	if got, want := ProcedureSignatureLabel(&ProcedureDesc{Name: "NOARGS"}), "NOARGS ()"; got != want {
		t.Errorf("ProcedureSignatureLabel with no inputs = %q, want %q", got, want)
	}
}

func TestGeneratorDocRendersNameAndNothingElse(t *testing.T) {
	got := GeneratorDoc(&GeneratorDesc{Name: "GEN_ORDER_ID", ID: sql.NullInt64{Int64: 3, Valid: true}})
	if got != "`GEN_ORDER_ID` generator" {
		t.Errorf("GeneratorDoc = %q, want %q", got, "`GEN_ORDER_ID` generator")
	}
}

func TestFunctionDocRendersUnrenderableArgumentsWithoutPlaceholders(t *testing.T) {
	desc := &FunctionDesc{
		Name:        "MYUDF",
		ReturnType:  "DOUBLE PRECISION",
		Description: sql.NullString{String: "Rounds half up.", Valid: true},
		ModuleName:  sql.NullString{String: "udflib", Valid: true},
		EntryPoint:  sql.NullString{String: "myudf", Valid: true},
		Arguments: []*FunctionArgumentDesc{
			{Name: "", Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "DOUBLE PRECISION"},
			// RDB$CHARACTER_LENGTH is never populated for function arguments,
			// so a CHAR/VARCHAR argument renders "" permanently.
			{Name: "", Position: sql.NullInt64{Int64: 2, Valid: true}, Type: ""},
		},
	}

	got := FunctionDoc(desc)
	for _, want := range []string{
		"`MYUDF` external function",
		"Rounds half up.",
		"- argument 1: `DOUBLE PRECISION`",
		"- argument 2",
		"Returns `DOUBLE PRECISION`.",
		"`udflib`",
		"`myudf`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FunctionDoc missing %q:\n%s", want, got)
		}
	}
	// The untyped argument keeps its label and gains nothing else. A naive
	// renderer emits "- argument 2: ``" or "- argument 2: <unknown>"; both
	// fail here.
	if strings.Contains(got, "argument 2: ") || strings.Contains(got, "<unknown>") || strings.Contains(got, "``") {
		t.Errorf("FunctionDoc rendered a placeholder for the unknown argument type:\n%s", got)
	}
}

func TestFunctionDocOmitsReturnsLineWhenReturnTypeIsEmpty(t *testing.T) {
	got := FunctionDoc(&FunctionDesc{
		Name:      "MYUDF",
		Arguments: []*FunctionArgumentDesc{{Position: sql.NullInt64{Int64: 1, Valid: true}, Type: ""}},
	})
	if !strings.Contains(got, "`MYUDF` external function") {
		t.Errorf("a UDF with no renderable type at all lost its name:\n%s", got)
	}
	if !strings.Contains(got, "- argument 1") {
		t.Errorf("a UDF with no renderable argument type lost its argument list:\n%s", got)
	}
	if strings.Contains(got, "Returns") {
		t.Errorf("FunctionDoc rendered a returns line for an empty ReturnType:\n%s", got)
	}
}

func TestTriggerDocOmitsEmptyEvent(t *testing.T) {
	desc := &TriggerDesc{
		Name:         "MYTRIGGER",
		RelationName: sql.NullString{String: "CITY", Valid: true},
		Event:        "",
		Active:       sql.NullBool{Bool: true, Valid: true},
		Source:       sql.NullString{String: "BEGIN\n  NEW.ID = 1;\nEND", Valid: true},
	}

	got := TriggerDoc(desc)
	for _, want := range []string{"`MYTRIGGER` trigger", "CITY", "active", "NEW.ID = 1;"} {
		if !strings.Contains(got, want) {
			t.Errorf("TriggerDoc missing %q:\n%s", want, got)
		}
	}
	// An empty Event means the catalog did not decode it. The whole line is
	// omitted; nothing stands in for it.
	if strings.Contains(got, "<unknown>") {
		t.Errorf("TriggerDoc rendered a placeholder event:\n%s", got)
	}
	// Positive adjacency check rather than forbidding "``": the fenced
	// Source block below legitimately contains "```sql", which itself
	// contains "``", so a blanket ban on that substring would fail against
	// any correct renderer once Source is present. Asserting that the
	// RelationName line is directly followed by the Active line, with
	// nothing between them, is what "the Event line is omitted" actually
	// means.
	if !strings.Contains(got, "On `CITY`.\n\nCurrently active.\n\n") {
		t.Errorf("TriggerDoc did not cleanly omit the empty Event between RelationName and Active:\n%s", got)
	}

	desc.Event = "BEFORE INSERT"
	if got := TriggerDoc(desc); !strings.Contains(got, "BEFORE INSERT") {
		t.Errorf("TriggerDoc dropped a known event:\n%s", got)
	}
}

func TestViewDocRendersColumnsAndSource(t *testing.T) {
	desc := &ViewDesc{
		Name:       "MYVIEW",
		ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
		Columns: []*ColumnDesc{
			{ColumnBase: ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
		},
	}

	got := ViewDoc(desc)
	for _, want := range []string{"`MYVIEW` view", "| `ID` | `INTEGER` |", "SELECT ID FROM CITY"} {
		if !strings.Contains(got, want) {
			t.Errorf("ViewDoc missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "CREATE VIEW") {
		t.Errorf("ViewDoc fabricated a CREATE header:\n%s", got)
	}
}

func TestTableDocOutputIsUnchangedByTheExtraction(t *testing.T) {
	// writeColumnTable is extracted out of TableDoc in this task. TableDoc's
	// exact output is asserted by existing hover tests, so it is pinned here
	// too, byte for byte.
	cols := []*ColumnDesc{
		{ColumnBase: ColumnBase{Table: "city", Name: "ID"}, Type: "int(11)", Key: "PRI", Extra: "auto_increment"},
	}
	want := "# `city` table\n\n\n" +
		"| Name&nbsp;&nbsp; | Type&nbsp;&nbsp; | Primary&nbsp;key&nbsp;&nbsp; | Default&nbsp;&nbsp; | Extra&nbsp;&nbsp; |\n" +
		"| :--------------- | :--------------- | :---------------------- | :------------------ | :---------------- |\n" +
		"| `ID` | `int(11)` | `PRI` | `-` | auto_increment |\n"
	if got := TableDoc("city", cols); got != want {
		t.Errorf("TableDoc output changed:\ngot:  %q\nwant: %q", got, want)
	}
}

// --- Hostile-content tests -------------------------------------------------
//
// Catalog content (names, defaults, descriptions, source) is user-controlled:
// InterBase delimited identifiers can contain almost any character. These
// tests confirm the renderer defuses the three concrete hazards markdown
// exposes here (a table cell splits on a raw "|"; a code span closes at the
// first raw backtick; a line starting with "```" opens a fenced code block
// that swallows everything after it) instead of merely looking fine on
// ordinary names like CUSTOMER.

func TestWriteColumnTableEscapesPipeInColumnName(t *testing.T) {
	buf := new(bytes.Buffer)
	cols := []*ColumnDesc{
		{
			ColumnBase: ColumnBase{Table: "T", Name: "ID|DROP TABLE T;--"},
			Type:       "INTEGER",
			Key:        "PRI",
			Default:    sql.NullString{String: "0", Valid: true},
		},
	}
	writeColumnTable(buf, cols)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	row := lines[len(lines)-1]

	// A five-cell row has exactly six pipes usable as delimiters: one
	// leading, four between cells, one trailing. Subtracting escaped pipes
	// ("\|") from the raw count leaves the number a naive splitter would
	// treat as delimiters; an unescaped hostile pipe raises that to seven
	// and shifts every later column.
	unescaped := strings.Count(row, "|") - strings.Count(row, `\|`)
	if unescaped != 6 {
		t.Errorf("hostile pipe in column name was not escaped, got %d delimiter-eligible pipes, want 6:\n%s", unescaped, row)
	}
	if !strings.Contains(row, `ID\|DROP TABLE T;--`) {
		t.Errorf("column name lost content while escaping its pipe:\n%s", row)
	}
}

func TestWriteColumnTableEscapesBacktickAndNewlineInDefault(t *testing.T) {
	buf := new(bytes.Buffer)
	cols := []*ColumnDesc{
		{
			ColumnBase: ColumnBase{Table: "T", Name: "ID"},
			Type:       "VARCHAR(10)",
			Default:    sql.NullString{String: "CURRENT`_`TIMESTAMP\nSECOND LINE", Valid: true},
		},
	}
	writeColumnTable(buf, cols)
	got := buf.String()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("hostile default's embedded newline split the table (%d lines, want 3: header, delimiter, one data row):\n%s", len(lines), got)
	}
	row := lines[2]
	if !strings.Contains(row, "CURRENT") || !strings.Contains(row, "SECOND LINE") {
		t.Errorf("default value lost content while escaping it:\n%s", row)
	}
	// A plain single-backtick fence would close at the default's own first
	// backtick and leak the rest as raw markdown; the fence must widen past
	// the longest internal backtick run (here 1, so a fence of at least 2).
	if !strings.Contains(row, "``CURRENT") {
		t.Errorf("fence was not widened past the default's internal backtick, so it could close early:\n%s", row)
	}
}

func TestProcedureDocEscapesFenceAttemptInDescription(t *testing.T) {
	desc := &ProcedureDesc{
		Name:        "MYPROC",
		Description: sql.NullString{String: "Summary.\n\n```\nFAKE FENCE\n```\n\nEnd of summary.", Valid: true},
		Source:      sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
	}
	got := ProcedureDoc(desc)

	// Only the real Source block may open or close a fence: exactly two raw
	// ``` markers. An unescaped description would add two more of its own
	// and could swallow the real Source into what looks like its own code
	// block.
	if n := strings.Count(got, "```"); n != 2 {
		t.Errorf("description's embedded fence markers were not neutralized, got %d raw fence markers, want 2:\n%s", n, got)
	}
	escapedBacktick := "\\`"
	if !strings.Contains(got, strings.Repeat(escapedBacktick, 3)) {
		t.Errorf("ProcedureDoc did not escape the description's backticks:\n%s", got)
	}
	if !strings.Contains(got, "FAKE FENCE") {
		t.Errorf("ProcedureDoc lost description content while escaping it:\n%s", got)
	}
	if !strings.Contains(got, "```sql\nBEGIN\n  SUSPEND;\nEND\n```") {
		t.Errorf("the real Source block was corrupted by the description's fence attempt:\n%s", got)
	}
}

func TestProcedureDocFencesSourceContainingBackticks(t *testing.T) {
	desc := &ProcedureDesc{
		Name:   "MYPROC",
		Source: sql.NullString{String: "BEGIN\n  /* ``` */\n  SUSPEND;\nEND", Valid: true},
	}
	got := ProcedureDoc(desc)

	if !strings.Contains(got, "````sql\n") {
		t.Errorf("fence was not widened past the source's own triple backtick, so it could close early:\n%s", got)
	}
	if !strings.Contains(got, "/* ``` */") {
		t.Errorf("ProcedureDoc lost source content while fencing it:\n%s", got)
	}
}
