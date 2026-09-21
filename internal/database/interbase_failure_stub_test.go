//go:build !interbase || !cgo || !linux || !amd64

package database

import (
	"errors"
	"testing"
)

func TestClassifyFailureWithoutNativeBuildReportsNone(t *testing.T) {
	kind, operation := ClassifyFailure(errors.New("boom"))
	if kind != FailureNone {
		t.Errorf("kind = %v, want FailureNone", kind)
	}
	if operation != "" {
		t.Errorf("operation = %q, want \"\"", operation)
	}
	if kind, _ := ClassifyFailure(nil); kind != FailureNone {
		t.Errorf("ClassifyFailure(nil) = %v, want FailureNone", kind)
	}
}
