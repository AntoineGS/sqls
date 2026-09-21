//go:build interbase && cgo && linux && amd64

package database

import (
	"errors"

	interbase "interbase-go"
)

// ClassifyFailure reports whether err is a canceled statement and, if so,
// whether its outcome is known. The uncertain case is checked first because an
// UncertainOutcomeError unwraps to the CancellationError it wraps, so the
// reverse order would report every uncertain write as merely canceled.
func ClassifyFailure(err error) (FailureKind, string) {
	if err == nil {
		return FailureNone, ""
	}
	var uncertain *interbase.UncertainOutcomeError
	if errors.As(err, &uncertain) && uncertain != nil {
		return FailureUncertain, uncertain.Operation
	}
	var canceled *interbase.CancellationError
	if errors.As(err, &canceled) && canceled != nil {
		return FailureCanceled, canceled.Operation
	}
	return FailureNone, ""
}
