package database

import "github.com/sqls-server/sqls/catalog"

// ParameterDoc renders one procedure parameter as a single line.
func ParameterDoc(param *ProcedureParameterDesc) string { return catalog.ParameterDoc(param) }

// ProcedureDoc renders the catalog's own view of a procedure.
func ProcedureDoc(desc *ProcedureDesc) string { return catalog.ProcedureDoc(desc) }

// ProcedureSignatureLabel renders the signature-help label, "MYPROC (A, B)".
func ProcedureSignatureLabel(desc *ProcedureDesc) string {
	return catalog.ProcedureSignatureLabel(desc)
}

// ProcedureSignatureDoc counts both parameter lists so the user can tell a
// selectable procedure from an executable one.
func ProcedureSignatureDoc(desc *ProcedureDesc) string { return catalog.ProcedureSignatureDoc(desc) }

// ViewDoc renders a view as its column table plus the verbatim catalog
// source.
func ViewDoc(desc *ViewDesc) string { return catalog.ViewDoc(desc) }

// GeneratorDoc renders a generator's name and nothing else.
func GeneratorDoc(desc *GeneratorDesc) string { return catalog.GeneratorDoc(desc) }

// FunctionDoc renders an external function's declaration metadata.
func FunctionDoc(desc *FunctionDesc) string { return catalog.FunctionDoc(desc) }

// TriggerDoc renders a trigger's catalog fields.
func TriggerDoc(desc *TriggerDesc) string { return catalog.TriggerDoc(desc) }
