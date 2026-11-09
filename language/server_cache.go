package language

import (
	"container/list"
	"sync"

	"github.com/sqls-server/sqls/metadata"
)

const serverCacheLimit = 2048

type serverIdentity struct {
	key         metadata.Key
	incarnation uint64
}

type serverGen struct {
	generation uint64
	revision   uint64
}

type serverCacheKey struct {
	id  serverIdentity
	sql string
}

type serverOutcome struct {
	err *StatementError
}

type serverCache struct {
	mu      sync.Mutex
	entries map[serverCacheKey]*list.Element
	order   *list.List
	gens    map[serverIdentity]serverGen
}

func (c *serverCache) sync(id serverIdentity, generation serverGen) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializeLocked()
	if previous, ok := c.gens[id]; ok && previous != generation {
		for key, element := range c.entries {
			if key.id == id {
				delete(c.entries, key)
				c.order.Remove(element)
			}
		}
	}
	c.gens[id] = generation
}

func (c *serverCache) get(key serverCacheKey) (serverOutcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return serverOutcome{}, false
	}
	element, ok := c.entries[key]
	if !ok {
		return serverOutcome{}, false
	}
	c.order.MoveToFront(element)
	return cloneServerOutcome(element.Value.(serverCacheEntry).outcome), true
}

func (c *serverCache) put(key serverCacheKey, generation serverGen, outcome serverOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if active, ok := c.gens[key.id]; !ok || active != generation {
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
	for len(c.entries) > serverCacheLimit {
		oldest := c.order.Back()
		entry := oldest.Value.(serverCacheEntry)
		delete(c.entries, entry.key)
		c.order.Remove(oldest)
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
}

func cloneServerOutcome(outcome serverOutcome) serverOutcome {
	if outcome.err != nil {
		copy := *outcome.err
		outcome.err = &copy
	}
	return outcome
}
