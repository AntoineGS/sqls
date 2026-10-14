//go:build interbase && cgo && linux && amd64

package ibmeta

import (
	"context"
	"errors"
	"testing"

	"github.com/sqls-server/sqls/metadata"
)

func TestWatchedChangesRejectForeignFingerprint(t *testing.T) {
	var reader metadata.ChangeReader = &nativeProvider{}
	_, err := reader.ReadChanges(context.Background(), nil, nil, nil)
	if !errors.Is(err, metadata.ErrWatchIncomplete) {
		t.Fatalf("invalid fingerprint: %v", err)
	}
}
