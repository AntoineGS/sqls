package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
)

func TestCancellationNoticeForContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := cancellationNotice(ctx, context.Canceled)
	if got != stoppedWaitingMessage {
		t.Errorf("cancellationNotice = %q, want %q", got, stoppedWaitingMessage)
	}
	// A driver that only reports context.Canceled has confirmed nothing, so the
	// notice must not claim the statement was stopped.
	if got == canceledMessage {
		t.Error("a bare context cancellation must not render the confirmed-stop message")
	}
	if strings.Contains(got, "UNCERTAIN") {
		t.Error("the fallback message must not mention an uncertain outcome")
	}
}

func TestCancellationNoticeIsEmptyForOrdinaryFailure(t *testing.T) {
	if got := cancellationNotice(context.Background(), errors.New("syntax error")); got != "" {
		t.Errorf("cancellationNotice = %q, want \"\"", got)
	}
}

// TestMessageForFailureKindMapsEachKindToItsOwnMessage drives the
// FailureKind-to-message mapping directly, bypassing database.ClassifyFailure
// entirely. ClassifyFailure is stubbed to always return FailureNone outside
// the interbase build tag, so cancellationNotice can never reach the
// FailureUncertain/FailureCanceled branches through the untagged
// `go test ./...` CI runs; calling messageForFailureKind directly is the only
// way this mapping is exercised in that build.
func TestMessageForFailureKindMapsEachKindToItsOwnMessage(t *testing.T) {
	err := errors.New("boom")
	if got := messageForFailureKind(database.FailureUncertain, "execute statement", err); got != uncertainOutcomeMessage {
		t.Errorf("messageForFailureKind(FailureUncertain, ...) = %q, want %q", got, uncertainOutcomeMessage)
	}
	if got := messageForFailureKind(database.FailureCanceled, "execute statement", err); got != canceledMessage {
		t.Errorf("messageForFailureKind(FailureCanceled, ...) = %q, want %q", got, canceledMessage)
	}
	if got := messageForFailureKind(database.FailureNone, "execute statement", err); got != "" {
		t.Errorf("messageForFailureKind(FailureNone, ...) = %q, want \"\"", got)
	}
}

func TestQueryFailureMessagesMatchClassification(t *testing.T) {
	if !strings.Contains(uncertainOutcomeMessage, "UNCERTAIN") {
		t.Error("the uncertain message must contain UNCERTAIN")
	}
	if !strings.Contains(uncertainOutcomeMessage, "Do not re-run") {
		t.Error("the uncertain message must tell the user not to re-run the statement")
	}
	if strings.Contains(canceledMessage, "UNCERTAIN") {
		t.Error("the canceled message must not claim the outcome is uncertain")
	}
	if strings.Contains(canceledMessage, "Do not re-run") {
		t.Error("the canceled message must not carry the do-not-retry warning")
	}
	// The canceled message says the statement was stopped, never that it was
	// guaranteed stopped: the driver's cancellation is best effort.
	if strings.Contains(canceledMessage, "guaranteed") {
		t.Error("the canceled message must not promise a guarantee the driver cannot make")
	}
	// No cancellation message may ever be rendered together with the note
	// saying the statement completed after all — they assert opposite things.
	for name, message := range map[string]string{
		"canceled":        canceledMessage,
		"uncertain":       uncertainOutcomeMessage,
		"stopped waiting": stoppedWaitingMessage,
	} {
		if strings.Contains(message, "arrived after the statement completed") {
			t.Errorf("the %s message must not contain the late-arrival note", name)
		}
	}
}
