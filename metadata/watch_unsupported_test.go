package metadata_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sqls-server/sqls/metadata"
)

func TestMaintainUnsupportedDoesNotStartRead(t *testing.T) {
	p := newPhasedProvider()
	reg, lease := acquirePhased(t, p)
	defer func() { lease.Release(); _ = reg.Close(context.Background()) }()
	before := lease.Snapshot()
	_, err := lease.Maintain(context.Background(), metadata.MaintenanceRequest{Mode: metadata.MaintenanceCheck, Fingerprint: true})
	if !errors.Is(err, metadata.ErrUnsupported) {
		t.Fatal(err)
	}
	if lease.Snapshot() != before {
		t.Fatal("unsupported call published")
	}
}
