package hostagent

import (
	"sync"
	"testing"
)

func TestSubnetAllocator_DistinctInstancesGetDistinctSubnets(t *testing.T) {
	a, err := NewSubnetAllocator("172.16.0.0/16", 10)
	if err != nil {
		t.Fatal(err)
	}

	h1, g1, err := a.Allocate("inst-1")
	if err != nil {
		t.Fatal(err)
	}
	h2, g2, err := a.Allocate("inst-2")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 || g1 == g2 {
		t.Fatalf("two distinct instances got colliding subnets: (%s,%s) vs (%s,%s)", h1, g1, h2, g2)
	}
	if h1 != "172.16.0.1" || g1 != "172.16.0.2" {
		t.Errorf("first allocation = (%s, %s), want (172.16.0.1, 172.16.0.2)", h1, g1)
	}
	if h2 != "172.16.0.5" || g2 != "172.16.0.6" {
		t.Errorf("second allocation = (%s, %s), want (172.16.0.5, 172.16.0.6) — the next /30 block", h2, g2)
	}
}

func TestSubnetAllocator_AllocateIsIdempotentPerInstance(t *testing.T) {
	a, err := NewSubnetAllocator("172.16.0.0/16", 10)
	if err != nil {
		t.Fatal(err)
	}
	h1, g1, err := a.Allocate("inst-1")
	if err != nil {
		t.Fatal(err)
	}
	h2, g2, err := a.Allocate("inst-1") // same instance, called again (e.g. re-entrant resume)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || g1 != g2 {
		t.Errorf("re-allocating the same instance should return the same subnet, got (%s,%s) then (%s,%s)", h1, g1, h2, g2)
	}
}

func TestSubnetAllocator_ReleaseAllowsReuse(t *testing.T) {
	a, err := NewSubnetAllocator("172.16.0.0/16", 1) // pool of exactly 1
	if err != nil {
		t.Fatal(err)
	}
	h1, g1, err := a.Allocate("inst-1")
	if err != nil {
		t.Fatal(err)
	}
	a.Release("inst-1")

	h2, g2, err := a.Allocate("inst-2")
	if err != nil {
		t.Fatalf("expected the released slot to be reusable: %v", err)
	}
	if h1 != h2 || g1 != g2 {
		t.Errorf("expected inst-2 to reuse inst-1's released subnet, got (%s,%s) vs (%s,%s)", h1, g1, h2, g2)
	}
}

func TestSubnetAllocator_Lookup(t *testing.T) {
	a, err := NewSubnetAllocator("172.16.0.0/16", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := a.Lookup("inst-1"); ok {
		t.Error("Lookup on an unallocated instance should return ok=false")
	}
	h, g, err := a.Allocate("inst-1")
	if err != nil {
		t.Fatal(err)
	}
	lh, lg, ok := a.Lookup("inst-1")
	if !ok || lh != h || lg != g {
		t.Errorf("Lookup = (%s,%s,%v), want (%s,%s,true)", lh, lg, ok, h, g)
	}
	if len(a.freeSlots) != 9 {
		t.Errorf("Lookup must not consume a slot, freeSlots = %d, want 9", len(a.freeSlots))
	}
}

func TestSubnetAllocator_ReleaseUnknownInstanceIsNoOp(t *testing.T) {
	a, err := NewSubnetAllocator("172.16.0.0/16", 10)
	if err != nil {
		t.Fatal(err)
	}
	a.Release("never-allocated") // must not panic or error
}

func TestSubnetAllocator_PoolExhausted(t *testing.T) {
	a, err := NewSubnetAllocator("172.16.0.0/16", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Allocate("inst-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Allocate("inst-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Allocate("inst-3"); err == nil {
		t.Fatal("expected pool exhaustion error on the 3rd allocation from a pool of 2")
	}
}

func TestSubnetAllocator_ConcurrentAllocationsNeverCollide(t *testing.T) {
	const n = 50
	a, err := NewSubnetAllocator("172.16.0.0/16", n)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan [2]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, g, err := a.Allocate(instID(i))
			if err != nil {
				t.Error(err)
				return
			}
			results <- [2]string{h, g}
		}(i)
	}
	wg.Wait()
	close(results)

	seen := map[string]bool{}
	for r := range results {
		key := r[0] + "/" + r[1]
		if seen[key] {
			t.Fatalf("subnet %v allocated more than once under concurrency", r)
		}
		seen[key] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct subnets, want %d", len(seen), n)
	}
}

func TestNewSubnetAllocator_RejectsPoolLargerThanCIDR(t *testing.T) {
	// /30 has only 4 addresses total — a pool of 1 needs exactly that many,
	// a pool of 2 needs 8 and must be rejected.
	if _, err := NewSubnetAllocator("172.16.0.0/30", 1); err != nil {
		t.Errorf("pool of 1 should fit in a /30: %v", err)
	}
	if _, err := NewSubnetAllocator("172.16.0.0/30", 2); err == nil {
		t.Error("expected an error: pool of 2 (/30 blocks) cannot fit inside a /30")
	}
}

func instID(i int) string {
	return "inst-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}
