package apiservice

import (
	"testing"
	"time"
)

func TestRoutingCache_SetThenGet(t *testing.T) {
	c := newRoutingCache(0)
	c.set("inst-1", "host-1", "10.0.1.5:9000")

	addr, ok := c.get("inst-1")
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if addr != "10.0.1.5:9000" {
		t.Errorf("addr = %q, want 10.0.1.5:9000", addr)
	}
}

func TestRoutingCache_MissForUnknownSession(t *testing.T) {
	c := newRoutingCache(0)
	if _, ok := c.get("never-cached"); ok {
		t.Error("expected a cache miss")
	}
}

func TestRoutingCache_Evict(t *testing.T) {
	c := newRoutingCache(0)
	c.set("inst-1", "host-1", "10.0.1.5:9000")
	c.evict("inst-1")
	if _, ok := c.get("inst-1"); ok {
		t.Error("expected a cache miss after evict")
	}
}

func TestRoutingCache_ZeroTTLNeverExpires(t *testing.T) {
	c := newRoutingCache(0)
	c.set("inst-1", "host-1", "10.0.1.5:9000")
	// Backdate the entry far past any real TTL would allow, to prove ttl=0
	// really does mean "never expire" rather than "expire immediately".
	c.mu.Lock()
	e := c.entries["inst-1"]
	e.cachedAt = time.Now().Add(-24 * time.Hour)
	c.entries["inst-1"] = e
	c.mu.Unlock()

	if _, ok := c.get("inst-1"); !ok {
		t.Error("ttl=0 should mean the entry never expires")
	}
}

func TestRoutingCache_ExpiresAfterTTL(t *testing.T) {
	c := newRoutingCache(10 * time.Millisecond)
	c.set("inst-1", "host-1", "10.0.1.5:9000")

	if _, ok := c.get("inst-1"); !ok {
		t.Fatal("expected a hit immediately after set")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.get("inst-1"); ok {
		t.Error("expected the entry to have expired past its TTL")
	}
}

func TestRoutingCache_SetOverwritesExistingEntry(t *testing.T) {
	c := newRoutingCache(0)
	c.set("inst-1", "host-1", "10.0.1.5:9000")
	c.set("inst-1", "host-2", "10.0.1.9:9000") // e.g. re-cached after a resume

	addr, ok := c.get("inst-1")
	if !ok || addr != "10.0.1.9:9000" {
		t.Errorf("addr = %q, ok=%v, want the overwritten 10.0.1.9:9000", addr, ok)
	}
}
