package metadata

import (
	"context"
	"errors"
	"github.com/sqls-server/sqls/catalog"
	"testing"
)

func TestMaintainActualFailureOrigin(t *testing.T) {
	for _, req := range []MaintenanceRequest{watchedCheck, watchedFull} {
		for _, scan := range []bool{true, false} {
			p := newControlledWatch()
			_, l := acquireWatch(t, p)
			maintainWatch(t, l, watchedCheck)
			providerErr := errors.New("same provider error")
			p.mu.Lock()
			p.fingerprint.value++
			if scan {
				p.scanErr = providerErr
			} else {
				p.readErr = providerErr
			}
			p.mu.Unlock()
			got, err := l.Maintain(context.Background(), req)
			want := MaintenanceFailureReplacement
			if scan {
				want = MaintenanceFailureScan
			}
			if !errors.Is(err, providerErr) || got.FailureOrigin != want || got.ScanComplete == scan {
				t.Fatalf("scan=%v mode=%v result=%+v err=%v", scan, req.Mode, got, err)
			}
		}
	}
}

func TestMaintainOriginValidationAndFence(t *testing.T) {
	for _, phase := range []string{"merge", "validation", "invalidation", "publication-cancel", "invalid-scan", "expired-admission"} {
		t.Run(phase, func(t *testing.T) {
			p := newControlledWatch()
			_, l := acquireWatch(t, p)
			maintainWatch(t, l, watchedCheck)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := watchedCheck
			want := MaintenanceFailureReplacement
			complete := true
			gate := make(chan struct{})
			p.mu.Lock()
			p.fingerprint.value++
			switch phase {
			case "merge":
				p.deltaOverride = &ObjectDelta{Affected: -1}
			case "validation":
				req = watchedFull
				p.fullStatus = map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataProcedures: {State: catalog.MetadataFailed}}
			case "invalid-scan":
				p.fingerprint.bytes = -1
				want = MaintenanceFailureScan
				complete = false
			case "expired-admission":
				cancel()
				want = MaintenanceFailureAdmission
				complete = false
			case "invalidation", "publication-cancel":
				p.gate = gate
			}
			p.mu.Unlock()
			if phase == "invalidation" || phase == "publication-cancel" {
				// Empty the initial attempt notifications before waiting for replacement.
				for len(p.entered) > 0 {
					<-p.entered
				}
				done := make(chan struct{})
				var got MaintenanceResult
				var err error
				go func() { got, err = l.Maintain(ctx, req); close(done) }()
				for event := range p.entered {
					if event == "partial" {
						break
					}
				}
				if phase == "invalidation" {
					_ = l.Invalidate()
					want = MaintenanceFailureUncertainty
				} else {
					cancel()
				}
				close(gate)
				<-done
				if err == nil || got.FailureOrigin != want || !got.ScanComplete {
					t.Fatal(got, err)
				}
			} else {
				got, err := l.Maintain(ctx, req)
				if err == nil || got.FailureOrigin != want || got.ScanComplete != complete {
					t.Fatal(got, err)
				}
			}
		})
	}
}

func TestMaintainOriginAvailability(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	got := maintainWatch(t, l, watchedCheck)
	if !got.ScanComplete || got.FailureOrigin != MaintenanceFailureNone {
		t.Fatal(got)
	}
	got = maintainWatch(t, l, watchedCheck)
	if !got.ScanComplete || got.Outcome != "unchanged" {
		t.Fatal(got)
	}
	got, err := l.Maintain(context.Background(), MaintenanceRequest{Mode: MaintenanceFull})
	if err != nil || got.ScanComplete || got.FailureOrigin != MaintenanceFailureNone {
		t.Fatal(got, err)
	}
	p.mu.Lock()
	p.scanErr = ErrWatchLimit
	p.mu.Unlock()
	got, err = l.Maintain(context.Background(), watchedFull)
	if err != nil || got.ScanComplete || got.WatchError != "limit" || got.FailureOrigin != MaintenanceFailureNone {
		t.Fatal(got, err)
	}
	got, err = l.Maintain(context.Background(), MaintenanceRequest{Mode: MaintenanceCheck})
	if !errors.Is(err, ErrUnsupported) || got.FailureOrigin != MaintenanceFailureAdmission {
		t.Fatal(got, err)
	}
	l.Invalidate()
	got, err = l.Maintain(context.Background(), watchedCheck)
	if !errors.Is(err, ErrStale) || got.FailureOrigin != MaintenanceFailureUncertainty {
		t.Fatal(got, err)
	}
}
