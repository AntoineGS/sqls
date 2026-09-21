package database

// FailureKind classifies a statement failure that a cancellation can explain.
// The enum is declared here, untagged, so the native and stub implementations
// of ClassifyFailure cannot drift apart.
type FailureKind int

const (
	// FailureNone means the error is not a cancellation outcome.
	FailureNone FailureKind = iota
	// FailureCanceled means the statement was stopped and its outcome is known.
	FailureCanceled
	// FailureUncertain means a write was canceled and its effect could not be
	// established. Such a statement must not be blindly retried.
	FailureUncertain
)
