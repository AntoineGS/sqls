package metadata

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestEventHubPublishRacingCloseAll(t *testing.T) {
	// There is no gate between the old publisher's copied subscriber list and
	// its sends. Start both operations together repeatedly instead of adding a
	// production-only test hook or relying on sleeps.
	var panics atomic.Int64
	for i := 0; i < 2000; i++ {
		hub := newEventHub()
		for j := 0; j < 32; j++ {
			hub.subscribe()
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()
			<-start
			hub.publish(Event{Generation: 2})
		}()
		go func() {
			defer wg.Done()
			<-start
			hub.closeAll()
		}()
		close(start)
		wg.Wait()
		// Publications after teardown must be silently dropped, too.
		hub.publish(Event{Generation: 3})
	}
	if got := panics.Load(); got != 0 {
		t.Fatalf("publish raced channel closure: %d send-on-closed-channel panics", got)
	}
}

func TestEventHubSubscribeAfterCloseAll(t *testing.T) {
	hub := newEventHub()
	hub.closeAll()
	sub, events := hub.subscribe()
	hub.publish(Event{Generation: 2})
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("closed hub delivered a late publication")
		}
	default:
		t.Fatal("subscription to a closed hub remained open")
	}
	hub.unsubscribe(sub)
	hub.closeAll()
}

func TestEventHubCoalescingAndSubscriptionClose(t *testing.T) {
	hub := newEventHub()
	sub, events := hub.subscribe()
	hub.publish(Event{Generation: 1})
	hub.publish(Event{Generation: 2})
	if event := <-events; event.Generation != 2 {
		t.Fatalf("unread event was not coalesced: %+v", event)
	}
	s := &Subscription{C: events, close: func() { hub.unsubscribe(sub) }}
	s.Close()
	s.Close()
	hub.publish(Event{Generation: 3})
	select {
	case <-events:
		t.Fatal("unsubscribe changed channel closure or delivered an event")
	default:
	}
	hub.closeAll()
}
