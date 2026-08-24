package apiservice

import (
	"sync"
	"time"
)

// routingCacheEntry is what the API Service knows about a session without
// asking the Controller: which Host Agent owns it. Deliberately never
// guest_ip/guest_port — see the design doc's 3.1→3.2 changelog for why
// caching a routable guest address (rather than a stable Host Agent
// address) was the actual bug in the 3.1 routing-token design.
type routingCacheEntry struct {
	hostID        string
	hostAgentAddr string
	cachedAt      time.Time
}

// routingCache is the API Service's one piece of local state (design doc
// §4.1's "Local routing cache") — in-memory, per-replica, entirely
// rebuildable from Controller calls the API Service already makes. Losing
// every entry costs one extra Controller round-trip per affected session
// on next use, nothing more — it is never a source of truth, only an
// accelerator. The TTL exists purely to bound memory for sessions deleted
// without this replica's knowledge; host_agent_addr itself doesn't go
// stale the way a cached guest_ip would have (resume happens on the same
// host, no re-placement, §4.2), so correctness never depends on the TTL.
type routingCache struct {
	mu      sync.RWMutex
	entries map[string]routingCacheEntry
	ttl     time.Duration
}

func newRoutingCache(ttl time.Duration) *routingCache {
	return &routingCache{entries: make(map[string]routingCacheEntry), ttl: ttl}
}

func (c *routingCache) set(sessionID, hostID, hostAgentAddr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[sessionID] = routingCacheEntry{hostID: hostID, hostAgentAddr: hostAgentAddr, cachedAt: time.Now()}
}

func (c *routingCache) get(sessionID string) (hostAgentAddr string, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[sessionID]
	if !ok {
		return "", false
	}
	if c.ttl > 0 && time.Since(e.cachedAt) > c.ttl {
		return "", false
	}
	return e.hostAgentAddr, true
}

func (c *routingCache) evict(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, sessionID)
}
