package sqlsymbol

import "fmt"

// diagnosticRegistry lists every finding code DiagnosticsWithOptions knows
// how to configure, together with the severity it reports today when no
// explicit level override applies -- copied from each rule's own Finding
// literal, not invented here. No code is off by default: "off" is a level a
// caller may choose, not a current default for any rule.
var diagnosticRegistry = map[string]int{
	codeUnused:               4,
	codeStringTruncation:     2,
	codeSingletonSelect:      2,
	codeNullComparison:       2,
	codeUnknownVariable:      1,
	codeDuplicateDeclaration: 1,
	codeUnknownRelation:      1,
	codeUnknownColumn:        1,
	codeUnknownQualifier:     1,
	codeAmbiguousColumn:      1,
	codeTargetCount:          1,
	codeProcedureArity:       1,
}

// diagnosticLevelSeverity maps every level that overrides a finding's
// reported severity (as opposed to "default"/"off", which do not) to its
// LSP-style severity number.
var diagnosticLevelSeverity = map[string]int{
	"error":       1,
	"warning":     2,
	"information": 3,
	"hint":        4,
}

// DiagnosticOptions configures rule-level policy. Rules maps a finding code
// to a level:
//
//   - "default" (or an absent entry): report at the registry's built-in
//     severity for that code.
//   - "off": never compute or report that code's findings.
//   - "error"/"warning"/"information"/"hint": report at that severity.
type DiagnosticOptions struct {
	Rules map[string]string
}

// Validate rejects an unknown code or an unknown level rather than silently
// ignoring either.
func (o DiagnosticOptions) Validate() error {
	for code, level := range o.Rules {
		if _, known := diagnosticRegistry[code]; !known {
			return fmt.Errorf("sqlsymbol: unknown diagnostic rule code %q", code)
		}
		if !isValidDiagnosticLevel(level) {
			return fmt.Errorf("sqlsymbol: unknown diagnostic level %q for rule %q", level, code)
		}
	}
	return nil
}

func isValidDiagnosticLevel(level string) bool {
	switch level {
	case "default", "off":
		return true
	}
	_, ok := diagnosticLevelSeverity[level]
	return ok
}

// level resolves code's configured level, defaulting to "default" when Rules
// has no entry (or an empty entry) for it.
func (o DiagnosticOptions) level(code string) string {
	if o.Rules == nil {
		return "default"
	}
	if level, ok := o.Rules[code]; ok && level != "" {
		return level
	}
	return "default"
}

// off reports whether code is disabled entirely under o.
func (o DiagnosticOptions) off(code string) bool {
	return o.level(code) == "off"
}

// severity resolves the severity a produced finding for code must carry
// under o. Never consulted for an "off" code, since those are never
// produced.
func (o DiagnosticOptions) severity(code string) int {
	level := o.level(code)
	if level == "default" {
		return diagnosticRegistry[code]
	}
	return diagnosticLevelSeverity[level]
}

// anyOn reports whether at least one of codes is not off under o. It gates
// whether a rule group's computation runs at all: an all-off group is
// skipped outright rather than computed and then filtered.
func (o DiagnosticOptions) anyOn(codes ...string) bool {
	for _, code := range codes {
		if !o.off(code) {
			return true
		}
	}
	return false
}

// applyDiagnosticOptions drops every off finding and overrides the severity
// of every remaining one to its option-resolved value.
func applyDiagnosticOptions(findings []Finding, options DiagnosticOptions) []Finding {
	kept := findings[:0]
	for _, f := range findings {
		if options.off(f.Code) {
			continue
		}
		f.Severity = options.severity(f.Code)
		kept = append(kept, f)
	}
	return kept
}

// DiagnosticsWithOptions reports the same findings as Diagnostics, governed
// by options: a rule group whose every code is off is never computed (not
// computed then filtered), and every produced finding's Severity is
// overridden to match its effective level.
func (a *Analysis) DiagnosticsWithOptions(c Catalog, options DiagnosticOptions) []Finding {
	findings := make([]Finding, 0)
	if options.anyOn(codeUnused) {
		findings = append(findings, applyDiagnosticOptions(a.unusedFindings(), options)...)
	}
	if options.anyOn(codeStringTruncation) {
		findings = append(findings, applyDiagnosticOptions(a.widthDiagnostics(c), options)...)
	}
	if options.anyOn(codeSingletonSelect) {
		findings = append(findings, applyDiagnosticOptions(a.singletonDiagnostics(c), options)...)
	}
	if options.anyOn(codeNullComparison) {
		findings = append(findings, applyDiagnosticOptions(a.nullComparisonFindings(), options)...)
	}
	if options.anyOn(codeUnknownVariable, codeDuplicateDeclaration,
		codeUnknownRelation, codeUnknownColumn, codeUnknownQualifier, codeAmbiguousColumn,
		codeTargetCount, codeProcedureArity) {
		m := a.diagnosticModel(c)
		if options.anyOn(codeUnknownVariable, codeDuplicateDeclaration) {
			findings = append(findings, applyDiagnosticOptions(m.localFindings(), options)...)
		}
		if options.anyOn(codeUnknownRelation, codeUnknownColumn, codeUnknownQualifier, codeAmbiguousColumn) {
			findings = append(findings, applyDiagnosticOptions(m.nameFindings(), options)...)
		}
		if options.anyOn(codeTargetCount, codeProcedureArity) {
			findings = append(findings, applyDiagnosticOptions(m.shapeFindings(), options)...)
		}
	}
	return findings
}
