package handler

import (
	"reflect"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
)

func TestInterBaseSelectIntoOutputIsNotPromptedOrExecuted(t *testing.T) {
	f := newParameterFixture(t, "SELECT config_value FROM branch_config WHERE branchid = '00' AND config_name = 'WEB_IMPORT_DISCOUNT_SKU' into :discountSku;", nil)
	d := f.discover(t, nil)
	if len(d.Parameters) != 0 {
		t.Fatalf("discovered parameters = %+v, want no output variables prompted", d.Parameters)
	}
	if _, err := f.execute(t, nil, nil); err != nil {
		t.Fatal(err)
	}
	want := "SELECT config_value FROM branch_config WHERE branchid = '00' AND config_name = 'WEB_IMPORT_DISCOUNT_SKU';"
	if calls := f.backend.calls(); len(calls) != 1 || calls[0].SQL != want || calls[0].Route != parameterRouteReadOnly {
		t.Fatalf("repository calls = %#v, want one SELECT without INTO", calls)
	}
}

func TestInterBaseForSelectPromptsOnlyForInputs(t *testing.T) {
	f := newParameterFixture(t, "FOR SELECT config_value FROM branch_config WHERE branchid = :branchid INTO :discountSku DO;", func(backend *parameterBackend) {
		backend.setInputDescription(
			"SELECT config_value FROM branch_config WHERE branchid = ?",
			[]database.InputDescriptor{{Kind: "INTEGER"}},
			nil,
		)
	})
	d := f.discover(t, nil)
	if len(d.Parameters) != 1 || d.Parameters[0].Key != "BRANCHID" || d.Parameters[0].InferredType != "integer" {
		t.Fatalf("discovered parameters = %+v, want only branchid inferred as integer", d.Parameters)
	}
	if described := f.backend.describedInputs(); len(described) != 1 || described[0] != "SELECT config_value FROM branch_config WHERE branchid = ?" {
		t.Fatalf("described statements = %#v, want normalized SELECT without INTO output", described)
	}
	if _, err := f.execute(t, submissionFor(d, textValue("branchid", "00")), nil); err != nil {
		t.Fatal(err)
	}
	want := "SELECT config_value FROM branch_config WHERE branchid = ?"
	if calls := f.backend.calls(); len(calls) != 1 || calls[0].SQL != want || calls[0].Route != parameterRouteReadOnly || !reflect.DeepEqual(calls[0].Args, []any{"00"}) {
		t.Fatalf("repository calls = %#v, want bound SELECT without FOR/INTO/DO", calls)
	}
}

func TestInterBaseExactDecimalInferenceBindsTextWithoutPrecisionLoss(t *testing.T) {
	const exact = "9007199254740993.25"
	f := newParameterFixture(t, "SELECT :amount FROM T", func(backend *parameterBackend) {
		backend.setInputDescription("SELECT ? FROM T", []database.InputDescriptor{{
			Kind: "BIGINT", Subtype: 2, Scale: -2, Precision: 18,
		}}, nil)
	})
	d := f.discover(t, nil)
	if len(d.Parameters) != 1 || d.Parameters[0].InferredType != "text" || d.Parameters[0].DatabaseType != "DECIMAL(18,2)" {
		t.Fatalf("discovered decimal parameter = %+v, want text / DECIMAL(18,2)", d.Parameters)
	}
	if _, err := f.execute(t, submissionFor(d, textValue("amount", exact)), nil); err != nil {
		t.Fatal(err)
	}
	if calls := f.backend.calls(); len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []any{exact}) {
		t.Fatalf("bound decimal calls = %#v, want exact decimal string argument", calls)
	}
}
