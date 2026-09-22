package database

import (
	"bytes"
	"fmt"
	"strings"
)

// collapseLines flattens catalog text to a single markdown line: a raw line
// break would otherwise split a table row, or spill a list item across
// lines.
func collapseLines(s string) string {
	return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
}

func longestBacktickRun(s string) int {
	longest, current := 0, 0
	for _, r := range s {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	return longest
}

// wrapCodeSpan wraps s as a markdown inline code span. Naive backtick
// wrapping breaks when s itself contains a backtick: the first internal
// backtick closes the span early and leaks the remainder of s as unescaped
// markdown, and backslash escapes do not work inside code spans, so escaping
// is not an option there. When s contains no backtick this is byte-identical
// to naive wrapping; only then does it widen the fence past the longest
// internal backtick run, padding with a space on any edge where s itself
// starts or ends with a backtick, so the fence can never fuse with content
// into one ambiguous run.
func wrapCodeSpan(s string) string {
	run := longestBacktickRun(s)
	if run == 0 {
		return "`" + s + "`"
	}
	fence := strings.Repeat("`", run+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// codeSpan safely renders s as a single-line inline code span.
func codeSpan(s string) string {
	return wrapCodeSpan(collapseLines(s))
}

// tableCellCode safely renders s as an inline code span placed inside a
// markdown table cell. GFM splits a table row into cells on raw "|"
// characters before it parses inline code spans, so a pipe inside the span
// still needs its own backslash escape - the one place a backslash escape is
// honored inside a code span.
func tableCellCode(s string) string {
	return wrapCodeSpan(strings.ReplaceAll(collapseLines(s), "|", `\|`))
}

// tableCellText safely renders s as plain (non-code) text inside a markdown
// table cell.
func tableCellText(s string) string {
	s = strings.ReplaceAll(collapseLines(s), "|", `\|`)
	return strings.ReplaceAll(s, "`", "\\`")
}

// escapeProse escapes backticks in free-form catalog text that renders as
// its own paragraph, leaving line breaks alone so a legitimate multi-line
// description still reads as one. An unescaped run of three or more
// backticks at the start of a line would open a fenced code block and
// swallow everything after it in the same document, including a real Source
// block rendered later; a lone backtick could pair with another stray one
// elsewhere and open an unintended code span. Escaping every backtick
// defuses both.
func escapeProse(s string) string {
	return strings.ReplaceAll(s, "`", "\\`")
}

// escapeInline is escapeProse plus line collapsing, for catalog text placed
// directly into a single markdown line such as a list item label.
func escapeInline(s string) string {
	return escapeProse(collapseLines(s))
}

// codeFence returns a backtick fence long enough to safely wrap s in a
// fenced code block: one longer than the longest backtick run inside s
// (minimum three, the shortest valid fence), so embedded backticks in
// catalog source text can never close the block early.
func codeFence(s string) string {
	n := longestBacktickRun(s) + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

func writeFencedSource(buf *bytes.Buffer, source string) {
	source = strings.TrimRight(source, "\n")
	fence := codeFence(source)
	fmt.Fprintf(buf, "Source:\n\n%ssql\n%s\n%s\n", fence, source, fence)
}

// ParameterDoc renders one procedure parameter as a single line, for example
// "`VARCHAR(3)` input NOT NULL".
//
// Nullability renders only as NOT NULL, and only when the catalog proved it:
// Nullable.Valid && !Nullable.Bool. An invalid Nullable is the normal case
// for InterBase procedure parameters - RDB$PROCEDURE_PARAMETERS carries no
// declaration nullability flag - and renders nothing: printing "nullable"
// would assert a fact the catalog does not contain, and printing "unknown
// nullability" is noise in a one-line tooltip. An empty Type means the
// catalog could not render one; the element is omitted, never replaced by a
// placeholder.
func ParameterDoc(param *ProcedureParameterDesc) string {
	if param == nil {
		return ""
	}
	items := []string{}
	if param.Type != "" {
		items = append(items, codeSpan(param.Type))
	}
	if param.Direction != "" {
		items = append(items, string(param.Direction))
	}
	if param.Nullable.Valid && !param.Nullable.Bool {
		items = append(items, "NOT NULL")
	}
	return strings.Join(items, " ")
}

// ProcedureDoc renders the catalog's own view of a procedure: name,
// description, parameter lists, and the verbatim PSQL body. No CREATE header
// is synthesized - what the catalog cannot reproduce as DDL is shown as its
// own fields, never as an invented declaration.
func ProcedureDoc(desc *ProcedureDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s procedure\n\n", codeSpan(desc.Name))
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", escapeProse(strings.TrimSpace(desc.Description.String)))
	}
	writeParameterList(buf, "Input parameters:", desc.InputParameters)
	writeParameterList(buf, "Output parameters:", desc.OutputParameters)
	if desc.Source.Valid && strings.TrimSpace(desc.Source.String) != "" {
		writeFencedSource(buf, desc.Source.String)
	}
	return buf.String()
}

func writeParameterList(buf *bytes.Buffer, heading string, params []*ProcedureParameterDesc) {
	if len(params) == 0 {
		return
	}
	fmt.Fprintf(buf, "%s\n\n", heading)
	for _, param := range params {
		name := escapeInline(param.Name)
		if doc := ParameterDoc(param); doc != "" {
			fmt.Fprintf(buf, "- %s: %s\n", name, doc)
		} else {
			fmt.Fprintf(buf, "- %s\n", name)
		}
	}
	fmt.Fprintln(buf)
}

// ProcedureSignatureLabel renders the signature-help label, "MYPROC (A, B)".
// Output parameters are not arguments and never appear here. This is an LSP
// SignatureInformation.Label, plain text rather than markdown, so it is not
// markdown-escaped.
func ProcedureSignatureLabel(desc *ProcedureDesc) string {
	if desc == nil {
		return ""
	}
	names := make([]string, 0, len(desc.InputParameters))
	for _, param := range desc.InputParameters {
		names = append(names, param.Name)
	}
	return fmt.Sprintf("%s (%s)", desc.Name, strings.Join(names, ", "))
}

// ProcedureSignatureDoc counts both parameter lists so the user can tell a
// selectable procedure from an executable one.
func ProcedureSignatureDoc(desc *ProcedureDesc) string {
	if desc == nil {
		return ""
	}
	return fmt.Sprintf("%s procedure — %s, %s",
		desc.Name,
		pluralParameters(len(desc.InputParameters), "input"),
		pluralParameters(len(desc.OutputParameters), "output"),
	)
}

func pluralParameters(n int, kind string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s parameter", n, kind)
	}
	return fmt.Sprintf("%d %s parameters", n, kind)
}

// ViewDoc renders a view as its column table plus the verbatim catalog
// source.
func ViewDoc(desc *ViewDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "# %s view\n\n\n", codeSpan(desc.Name))
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", escapeProse(strings.TrimSpace(desc.Description.String)))
	}
	if len(desc.Columns) > 0 {
		writeColumnTable(buf, desc.Columns)
		fmt.Fprintln(buf)
	}
	if desc.ViewSource.Valid && strings.TrimSpace(desc.ViewSource.String) != "" {
		writeFencedSource(buf, desc.ViewSource.String)
	}
	return buf.String()
}

// GeneratorDoc renders a generator's name and nothing else. GeneratorDesc
// carries only Name and ID, so there is no description to show and none is
// invented.
func GeneratorDoc(desc *GeneratorDesc) string {
	if desc == nil {
		return ""
	}
	return fmt.Sprintf("%s generator", codeSpan(desc.Name))
}

// FunctionDoc renders an external function's declaration metadata.
//
// An argument whose Type is empty renders as its label alone. That is
// permanent for CHAR and VARCHAR arguments - RDB$CHARACTER_LENGTH is never
// populated for function arguments - and it must never hide the argument or
// the function: a UDF the catalog cannot fully describe is still a UDF the
// user needs to call.
func FunctionDoc(desc *FunctionDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s external function\n\n", codeSpan(desc.Name))
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", escapeProse(strings.TrimSpace(desc.Description.String)))
	}
	if len(desc.Arguments) > 0 {
		fmt.Fprintf(buf, "Arguments:\n\n")
		for i, arg := range desc.Arguments {
			label := escapeInline(arg.Name)
			if label == "" {
				position := int64(i + 1)
				if arg.Position.Valid {
					position = arg.Position.Int64
				}
				label = fmt.Sprintf("argument %d", position)
			}
			if arg.Type == "" {
				fmt.Fprintf(buf, "- %s\n", label)
				continue
			}
			fmt.Fprintf(buf, "- %s: %s\n", label, codeSpan(arg.Type))
		}
		fmt.Fprintln(buf)
	}
	if desc.ReturnType != "" {
		fmt.Fprintf(buf, "Returns %s.\n\n", codeSpan(desc.ReturnType))
	}
	if desc.ModuleName.Valid && desc.ModuleName.String != "" {
		if desc.EntryPoint.Valid && desc.EntryPoint.String != "" {
			fmt.Fprintf(buf, "Declared in module %s, entry point %s.\n", codeSpan(desc.ModuleName.String), codeSpan(desc.EntryPoint.String))
		} else {
			fmt.Fprintf(buf, "Declared in module %s.\n", codeSpan(desc.ModuleName.String))
		}
	}
	return buf.String()
}

// TriggerDoc renders a trigger's catalog fields. An empty Event means the
// catalog did not decode one; the line is omitted and nothing stands in for
// it.
func TriggerDoc(desc *TriggerDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%s trigger\n\n", codeSpan(desc.Name))
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", escapeProse(strings.TrimSpace(desc.Description.String)))
	}
	if desc.RelationName.Valid && desc.RelationName.String != "" {
		fmt.Fprintf(buf, "On %s.\n\n", codeSpan(desc.RelationName.String))
	}
	if desc.Event != "" {
		fmt.Fprintf(buf, "%s\n\n", codeSpan(desc.Event))
	}
	if desc.Active.Valid {
		state := "inactive"
		if desc.Active.Bool {
			state = "active"
		}
		fmt.Fprintf(buf, "Currently %s.\n\n", state)
	}
	if desc.Source.Valid && strings.TrimSpace(desc.Source.String) != "" {
		writeFencedSource(buf, desc.Source.String)
	}
	return buf.String()
}
