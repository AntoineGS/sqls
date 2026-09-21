//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"fmt"
	"testing"

	interbase "interbase-go"
)

func TestInterBaseClassifyFailureUncertainOutcome(t *testing.T) {
	canceled := &interbase.CancellationError{
		Operation: "execute statement",
		Mutating:  true,
		Context:   context.Canceled,
	}
	uncertain := &interbase.UncertainOutcomeError{
		Operation: "execute statement",
		Mutating:  true,
		Cause:     canceled,
		Cleanup:   fmt.Errorf("rollback failed"),
	}

	// An uncertain outcome wraps a cancellation, so the uncertain check must
	// come first or this reports FailureCanceled.
	if kind, operation := ClassifyFailure(uncertain); kind != FailureUncertain || operation != "execute statement" {
		t.Errorf("ClassifyFailure(uncertain) = (%v, %q), want (FailureUncertain, \"execute statement\")", kind, operation)
	}
	if kind, operation := ClassifyFailure(canceled); kind != FailureCanceled || operation != "execute statement" {
		t.Errorf("ClassifyFailure(canceled) = (%v, %q), want (FailureCanceled, \"execute statement\")", kind, operation)
	}
	if kind, _ := ClassifyFailure(fmt.Errorf("wrapped: %w", uncertain)); kind != FailureUncertain {
		t.Errorf("ClassifyFailure(wrapped uncertain) = %v, want FailureUncertain", kind)
	}
	if kind, _ := ClassifyFailure(fmt.Errorf("ordinary")); kind != FailureNone {
		t.Errorf("ClassifyFailure(ordinary) = %v, want FailureNone", kind)
	}
}
