package handler

import (
	"context"
	"errors"
	"log"

	"github.com/sqls-server/sqls/internal/database"
)

const canceledMessage = "Cancelled. The statement was stopped before it finished."

// stoppedWaitingMessage is the fallback for a driver that reports a cancelled
// statement as a bare context.Canceled. It deliberately does not say the
// statement was stopped: for MySQL or PostgreSQL a cancelled UPDATE commonly
// surfaces this way while the server-side outcome is unestablished, and
// canceledMessage would assert something the driver never confirmed.
const stoppedWaitingMessage = `Cancelled. sqls stopped waiting for this statement.

Whether it took effect depends on the database driver, which did not report a
confirmed outcome. Check the database state before re-running it.`

const uncertainOutcomeMessage = `Cancelled, but the outcome is UNCERTAIN.

InterBase could not confirm whether this statement took effect. Do not re-run it
until you have checked the database state — reconcile by operation id or by
querying the affected rows.`

// cancellationNotice renders the results-pane text for a statement that failed
// after its request was cancelled, or "" when the failure is something else.
// The driver's classifier is authoritative when it recognises the error;
// canceledMessage is reserved for the case where the driver confirmed the
// statement stopped. A bare context cancellation degrades to the weaker
// stoppedWaitingMessage, which keeps the path reachable on every driver and on
// an ordinary, untagged build without claiming a confirmation nobody gave.
func cancellationNotice(ctx context.Context, err error) string {
	kind, operation := database.ClassifyFailure(err)
	if msg := messageForFailureKind(kind, operation, err); msg != "" {
		return msg
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return stoppedWaitingMessage
	}
	return ""
}

// messageForFailureKind renders the notice for an already-classified failure
// kind, or "" when kind is not a cancellation outcome. Split out of
// cancellationNotice as its own seam because database.ClassifyFailure is
// stubbed to always return FailureNone outside the interbase build tag: no
// value of err can drive the FailureUncertain/FailureCanceled branches through
// cancellationNotice in the build CI actually runs, so this kind-to-message
// mapping must be exercised directly to be tested at all.
func messageForFailureKind(kind database.FailureKind, operation string, err error) string {
	switch kind {
	case database.FailureUncertain:
		log.Printf("interbase: %s outcome is uncertain: %v", operation, err)
		return uncertainOutcomeMessage
	case database.FailureCanceled:
		log.Printf("interbase: %s canceled: %v", operation, err)
		return canceledMessage
	}
	return ""
}
