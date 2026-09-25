package sqlsymbol

import "fmt"

// diagnosticRegistry lists every finding code DiagnosticsWithOptions knows
// how to configure, together with the severity it reports today when no
// explicit level override applies -- copied from each rule's own Finding
// literal, not invented here. Every code here is on by default except those
// listed in diagnosticDefaultOff (codeLossyAssignment and procedural-flow advisories): "off"
// is a level any code's caller may choose, but it is also the default level
// for a default-off code even when no explicit entry names it.
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
	codeInvalidAssignment:    1,
	codeLossyAssignment:      2,
	codeReadBeforeAssignment: 2,
	codeOutputNotAssigned:    2,
	codeDeadStore:            4,
	codeUnreachable:          4,
	codeNullableAssignment:   2,
	codeNullableNotIn:        2,
	codeOuterJoinFilter:      2,
}

// diagnosticDefaultOff lists every registered code whose default level (an
// absent Rules entry, or an explicit "default" override) is "off" rather
// than "on". codeLossyAssignment and procedural-flow advisories default off;
// every other registered code defaults to on. off() consults this set so a
// default-off code stays off both when Rules has no entry for it at all and
// when a caller explicitly writes "default" for it -- "default" always
// means whatever this code's own default is, never a way to force it on.
var diagnosticDefaultOff = map[string]bool{
	codeLossyAssignment:      true,
	codeReadBeforeAssignment: true,
	codeOutputNotAssigned:    true,
	codeDeadStore:            true,
	codeUnreachable:          true,
	codeNullableAssignment:   true,
	codeNullableNotIn:        true,
	codeOuterJoinFilter:      true,
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
//     severity for that code, unless the code is one of the few registered
//     in diagnosticDefaultOff, whose "default" means off.
//   - "off": never compute or report that code's findings.
//   - "error"/"warning"/"information"/"hint": report at that severity,
//     turning the code on even when its own default level is off.
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

// off reports whether code is disabled entirely under o: either because the
// caller explicitly wrote "off", or because the caller wrote nothing (or
// explicitly wrote "default") for a code whose own default level is off
// (diagnosticDefaultOff). An explicit "error"/"warning"/"information"/"hint"
// override always turns a default-off code on at that severity -- only
// "off" and "default"/absent keep it off.
func (o DiagnosticOptions) off(code string) bool {
	level := o.level(code)
	if level == "off" {
		return true
	}
	return level == "default" && diagnosticDefaultOff[code]
}

// severity resolves the severity a produced finding for code must carry
// under o, given fallback -- the severity the rule itself already assigned
// on the Finding it produced. fallback is used whenever the registry or
// level table cannot resolve a severity: an unregistered code (registry
// drift when a future rule adds a code and forgets to register it) or an
// unrecognized level string (defence in depth for a policy that somehow
// bypassed Validate). Never consulted for an "off" code, since those are
// never produced.
func (o DiagnosticOptions) severity(code string, fallback int) int {
	level := o.level(code)
	if level != "default" {
		if severity, ok := diagnosticLevelSeverity[level]; ok {
			return severity
		}
		return fallback
	}
	if severity, ok := diagnosticRegistry[code]; ok {
		return severity
	}
	return fallback
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
		f.Severity = options.severity(f.Code, f.Severity)
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
	if options.anyOn(codeReadBeforeAssignment, codeOutputNotAssigned, codeDeadStore, codeUnreachable,
		codeUnknownVariable, codeDuplicateDeclaration,
		codeUnknownRelation, codeUnknownColumn, codeUnknownQualifier, codeAmbiguousColumn,
		codeTargetCount, codeProcedureArity, codeInvalidAssignment, codeLossyAssignment,
		codeNullableAssignment, codeNullableNotIn, codeOuterJoinFilter) {
		m := a.diagnosticModel(c)
		if options.anyOn(codeReadBeforeAssignment, codeOutputNotAssigned, codeDeadStore, codeUnreachable) {
			findings = append(findings, applyDiagnosticOptions(m.flowFindings(options), options)...)
		}
		if options.anyOn(codeUnknownVariable, codeDuplicateDeclaration) {
			findings = append(findings, applyDiagnosticOptions(m.localFindings(), options)...)
		}
		if options.anyOn(codeUnknownRelation, codeUnknownColumn, codeUnknownQualifier, codeAmbiguousColumn) {
			findings = append(findings, applyDiagnosticOptions(m.nameFindings(), options)...)
		}
		if options.anyOn(codeTargetCount, codeProcedureArity) {
			findings = append(findings, applyDiagnosticOptions(m.shapeFindings(), options)...)
		}
		if options.anyOn(codeInvalidAssignment) {
			findings = append(findings, applyDiagnosticOptions(m.assignmentFindings(), options)...)
		}
		if options.anyOn(codeLossyAssignment) {
			findings = append(findings, applyDiagnosticOptions(m.lossyAssignmentFindings(), options)...)
		}
		if options.anyOn(codeNullableAssignment, codeNullableNotIn, codeOuterJoinFilter) {
			findings = append(findings, applyDiagnosticOptions(m.queryLogicFindings(options), options)...)
		}
	}
	return findings
}
