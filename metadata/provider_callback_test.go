package metadata

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Notifications return before the test-only resource-return gate. The gate
// models unfinished Provider.Close, not a callback waiting on its owner.
type ownerCallbackProvider struct {
	*controlledWatchProvider
	onClose            func()
	notified, returned chan struct{}
	returnGate         <-chan struct{}
}

func (p *ownerCallbackProvider) Close() error {
	err := p.controlledWatchProvider.Close()
	p.onClose()
	close(p.notified)
	<-p.returnGate
	close(p.returned)
	return err
}

func TestProviderCloseSupportsNonWaitingOwnerCallbacks(t *testing.T) {
	cases := []struct {
		name                                                              string
		releaseInCallback, requestFromCallback, expired, startWithRelease bool
	}{
		{name: "bookkeeping-and-state"},
		{name: "lease-release", releaseInCallback: true},
		{name: "already-cancelled-request", requestFromCallback: true},
		{name: "already-expired-request", requestFromCallback: true, expired: true},
		{name: "cancelled-request-starts-owner-close", requestFromCallback: true, startWithRelease: true},
		{name: "expired-request-starts-owner-close", requestFromCallback: true, expired: true, startWithRelease: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry(context.Background(), Options{})
			gate := make(chan struct{})
			p := &ownerCallbackProvider{controlledWatchProvider: newControlledWatch(), notified: make(chan struct{}), returned: make(chan struct{}), returnGate: gate}
			l, err := r.Acquire(context.Background(), Key{Engine: "test", URL: t.Name()}, Scope{Principal: "P"}, func(context.Context) (Provider, error) { return p, nil })
			if err != nil {
				t.Fatal(err)
			}
			if err = l.track.loader.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			sub := l.Subscribe()
			defer sub.Close()
			before := l.Snapshot()
			type observation struct {
				err                                 error
				live, closing, ownerDone, trackDone bool
				generation                          uint64
				refs                                int
			}
			observed := make(chan observation, 1)
			p.onClose = func() {
				var state observation
				// These inspect/bookkeeping locks and lease operations must not
				// be held by the owner while invoking Provider.Close.
				state.generation = l.Snapshot().Generation
				if tc.releaseInCallback {
					l.Release()
					l.Release()
				}
				if tc.requestFromCallback {
					ctx, cancel := context.WithCancel(context.Background())
					if tc.expired {
						cancel()
						ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					}
					cancel() // Already done BEFORE calling the completion API.
					state.err = r.Close(ctx)
				}
				r.mu.Lock()
				_, state.live = r.liveTracks[l.track]
				state.closing, state.refs = r.closed, l.track.refs
				r.mu.Unlock()
				select {
				case <-r.closeDone:
					state.ownerDone = true
				default:
				}
				select {
				case <-l.track.finalized:
					state.trackDone = true
				default:
				}
				observed <- state // Non-waiting notification; no owner join.
			}
			type completion struct {
				err                                    error
				providerReturned, trackDone, ownerDone bool
			}
			results := make(chan completion, 3)
			closeExternally := func() {
				result := completion{err: r.Close(context.Background())}
				select {
				case <-p.returned:
					result.providerReturned = true
				default:
				}
				select {
				case <-l.track.finalized:
					result.trackDone = true
				default:
				}
				select {
				case <-r.closeDone:
					result.ownerDone = true
				default:
				}
				results <- result
			}
			closers := 2
			if tc.startWithRelease {
				l.Release()
			} else {
				go closeExternally()
				closers++
			}
			var state observation
			select {
			case state = <-observed:
			case <-time.After(3 * time.Second):
				close(gate)
				t.Fatal("supported callback blocked on its owner")
			}
			select {
			case <-p.notified:
			case <-time.After(3 * time.Second):
				close(gate)
				t.Fatal("notification did not return")
			}
			if state.generation != before.Generation || !state.live || !state.closing || state.ownerDone || state.trackDone {
				close(gate)
				t.Fatal("notification observed false owner completion or lost bookkeeping", state)
			}
			if tc.requestFromCallback {
				want := context.Canceled
				if tc.expired {
					want = context.DeadlineExceeded
				}
				if !errors.Is(state.err, want) {
					close(gate)
					t.Fatal("already-done request did not return truthful context error", state.err)
				}
			} else if state.err != nil {
				close(gate)
				t.Fatal(state.err)
			}
			if (tc.releaseInCallback || tc.startWithRelease) && state.refs != 0 {
				close(gate)
				t.Fatal("lease callback did not withdraw its reference", state.refs)
			}
			// No sleeps: an already-cancelled EXTERNAL waiter must still report
			// its error while the actual Provider.Close return remains gated.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err = r.Close(ctx); !errors.Is(err, context.Canceled) {
				close(gate)
				t.Fatal("external close falsely completed the gated provider", err)
			}
			for i := 0; i < 2; i++ {
				go closeExternally()
			}
			close(gate)
			for i := 0; i < closers; i++ {
				select {
				case result := <-results:
					if result.err != nil || !result.providerReturned || !result.trackDone || !result.ownerDone {
						t.Fatal("external close did not join actual owned completion", result)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("external close or supported callback did not finish")
				}
			}
			assertWatchClosed(t, r, l, p.controlledWatchProvider, sub)
			l.Release()
			if err = r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p.closes.Load() != 1 || p.plans.Load() != 1 || p.scans.Load() != 0 || p.fulls.Load() != 0 || p.partials.Load() != 0 || p.details.Load() != 0 {
				t.Fatal("notification duplicated teardown or started metadata I/O")
			}
		})
	}
}
