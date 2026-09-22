package handler

import (
	"reflect"
	"testing"
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
	f := newParameterFixture(t, "FOR SELECT config_value FROM branch_config WHERE branchid = :branchid INTO :discountSku DO;", nil)
	d := f.discover(t, nil)
	if len(d.Parameters) != 1 || d.Parameters[0].Key != "BRANCHID" {
		t.Fatalf("discovered parameters = %+v, want only branchid", d.Parameters)
	}
	if _, err := f.execute(t, submissionFor(d, textValue("branchid", "00")), nil); err != nil {
		t.Fatal(err)
	}
	want := "SELECT config_value FROM branch_config WHERE branchid = ?"
	if calls := f.backend.calls(); len(calls) != 1 || calls[0].SQL != want || calls[0].Route != parameterRouteReadOnly || !reflect.DeepEqual(calls[0].Args, []any{"00"}) {
		t.Fatalf("repository calls = %#v, want bound SELECT without FOR/INTO/DO", calls)
	}
}
