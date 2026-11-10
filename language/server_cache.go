package language

import (
	"container/list"
	"sync"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/metadata"
)

const serverCacheLimit = 2048

type serverIdentity struct {
	key         metadata.Key
	incarnation uint64
	variant     dialect.DriverVariant
	partition   string
}

type serverGen struct {
	generation uint64
	revision   uint64
}

type serverCacheKey struct {
	id         serverIdentity
	generation serverGen
	sql        string
}

type serverOutcome struct {
	err *StatementError
}

type serverCache struct {
	mu      sync.Mutex
	entries map[serverCacheKey]*list.Element
	order   *list.List
	gens    map[serverIdentity]serverGen
	// counts tracks cached entries per identity so a generation record can be
	// dropped with its identity's last entry, keeping gens bounded.
	counts map[serverIdentity]int
}

func (c *serverCache) sync(id serverIdentity, generation serverGen) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializeLocked()
	if previous, ok := c.gens[id]; ok {
		comparison := compareServerGen(generation, previous)
		if comparison < 0 {
			return
		}
		if comparison == 0 {
			return
		}
		for key, element := range c.entries {
			if key.id == id {
				delete(c.entries, key)
				c.order.Remove(element)
			}
		}
		delete(c.counts, id)
		c.gens[id] = generation
		return
	}
	c.gens[id] = generation
	if len(c.gens) > serverCacheLimit {
		c.pruneEmptyIdentitiesLocked(id)
	}
}

// pruneEmptyIdentitiesLocked drops the generation records of identities that
// hold no cached entries, except keep. Losing a record only costs a cache miss:
// reads and writes still require an exact generation match.
func (c *serverCache) pruneEmptyIdentitiesLocked(keep serverIdentity) {
	for id := range c.gens {
		if id != keep && c.counts[id] == 0 {
			delete(c.gens, id)
		}
	}
}

func compareServerGen(left, right serverGen) int {
	if left.generation < right.generation {
		return -1
	}
	if left.generation > right.generation {
		return 1
	}
	if left.revision < right.revision {
		return -1
	}
	if left.revision > right.revision {
		return 1
	}
	return 0
}

func (c *serverCache) get(key serverCacheKey) (serverOutcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return serverOutcome{}, false
	}
	if active, ok := c.gens[key.id]; !ok || active != key.generation {
		return serverOutcome{}, false
	}
	element, ok := c.entries[key]
	if !ok {
		return serverOutcome{}, false
	}
	c.order.MoveToFront(element)
	return cloneServerOutcome(element.Value.(serverCacheEntry).outcome), true
}

func (c *serverCache) put(key serverCacheKey, outcome serverOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if active, ok := c.gens[key.id]; !ok || active != key.generation {
		return
	}
	c.initializeLocked()
	if element, ok := c.entries[key]; ok {
		element.Value = serverCacheEntry{key: key, outcome: cloneServerOutcome(outcome)}
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(serverCacheEntry{key: key, outcome: cloneServerOutcome(outcome)})
	c.entries[key] = element
	c.counts[key.id]++
	for len(c.entries) > serverCacheLimit {
		oldest := c.order.Back()
		entry := oldest.Value.(serverCacheEntry)
		delete(c.entries, entry.key)
		c.order.Remove(oldest)
		if c.counts[entry.key.id]--; c.counts[entry.key.id] <= 0 {
			delete(c.counts, entry.key.id)
			delete(c.gens, entry.key.id)
		}
	}
}

type serverCacheEntry struct {
	key     serverCacheKey
	outcome serverOutcome
}

func (c *serverCache) initializeLocked() {
	if c.entries == nil {
		c.entries = make(map[serverCacheKey]*list.Element)
	}
	if c.order == nil {
		c.order = list.New()
	}
	if c.gens == nil {
		c.gens = make(map[serverIdentity]serverGen)
	}
	if c.counts == nil {
		c.counts = make(map[serverIdentity]int)
	}
}

func cloneServerOutcome(outcome serverOutcome) serverOutcome {
	if outcome.err != nil {
		copy := *outcome.err
		outcome.err = &copy
	}
	return outcome
}
