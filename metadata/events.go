package metadata

import "sync"

// Subscription delivers coalesced Event notifications for one lease. A slow
// reader never blocks the publisher: each subscriber has a single buffered
// slot, and a new event overwrites an unread one rather than queuing.
type Subscription struct {
	C     <-chan Event
	close func()
	once  sync.Once
}

// Close stops delivery to this subscription. It is idempotent.
func (s *Subscription) Close() {
	s.once.Do(func() {
		if s.close != nil {
			s.close()
		}
	})
}

// eventHub fans one store's events out to its subscribers. Publish and
// closeAll never run while a registry/lease mutex is held.
type eventHub struct {
	mu   sync.Mutex
	subs map[*eventSub]struct{}
}

type eventSub struct {
	ch chan Event
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[*eventSub]struct{})}
}

func (h *eventHub) subscribe() (*eventSub, <-chan Event) {
	sub := &eventSub{ch: make(chan Event, 1)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub, sub.ch
}

func (h *eventHub) unsubscribe(sub *eventSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

func (h *eventHub) publish(ev Event) {
	h.mu.Lock()
	subs := make([]*eventSub, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		coalesceSend(sub.ch, ev)
	}
}

// closeAll disconnects and closes every subscriber channel. It is called
// once a store is fully torn down; no further events will publish.
func (h *eventHub) closeAll() {
	h.mu.Lock()
	subs := make([]*eventSub, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.subs = make(map[*eventSub]struct{})
	h.mu.Unlock()
	for _, sub := range subs {
		close(sub.ch)
	}
}

// coalesceSend delivers ev into a size-1 buffered channel without blocking,
// dropping a previously unread event in favor of the newer one.
func coalesceSend(ch chan Event, ev Event) {
	for {
		select {
		case ch <- ev:
			return
		default:
			select {
			case <-ch:
			default:
			}
		}
	}
}
