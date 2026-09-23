//go:build interbase && cgo && linux && amd64

package database

import (
	"testing"

	interbase "interbase-go"
)

func TestInterBaseInputDescriptorMapsDriverFields(t *testing.T) {
	got := inputDescriptorFromDriver(interbase.InputDescriptor{
		Kind: "BIGINT", Subtype: 2, Scale: -2, Precision: 18, Nullable: true,
	})
	want := InputDescriptor{
		Kind: "BIGINT", Subtype: 2, Scale: -2, Precision: 18, Nullable: true,
	}
	if got != want {
		t.Fatalf("input descriptor = %+v, want %+v", got, want)
	}
}
