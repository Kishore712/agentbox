package hostagent

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
)

// SubnetAllocator hands out collision-free /30 subnets from a private
// range, one per instance, reclaimed on release. This replaces the
// previous placeholder (subnetFor), which always returned the same IP
// regardless of instance — a guaranteed collision the moment two instances
// ran concurrently on one host, not just a low-probability risk.
type SubnetAllocator struct {
	mu        sync.Mutex
	base      uint32
	poolSize  int
	assigned  map[string]int // instanceID -> slot index
	freeSlots []int          // stack of available slot indices
}

// NewSubnetAllocator carves poolSize /30 blocks out of baseCIDR (e.g.
// "172.16.0.0/16"), each block yielding one host IP (.1) and one guest IP
// (.2). The caller is responsible for choosing a poolSize that fits inside
// baseCIDR's address space.
func NewSubnetAllocator(baseCIDR string, poolSize int) (*SubnetAllocator, error) {
	if poolSize <= 0 {
		return nil, fmt.Errorf("poolSize must be positive, got %d", poolSize)
	}
	ip, ipNet, err := net.ParseCIDR(baseCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse base CIDR: %w", err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("base CIDR must be IPv4: %s", baseCIDR)
	}
	ones, bits := ipNet.Mask.Size()
	available := int64(1) << uint(bits-ones)
	if int64(poolSize)*4 > available {
		return nil, fmt.Errorf("poolSize %d (needs %d addresses) exceeds %s's capacity (%d addresses)", poolSize, poolSize*4, baseCIDR, available)
	}

	base := binary.BigEndian.Uint32(ip4)
	// Populated in descending order so Allocate's LIFO pop (from the end)
	// yields ascending slot numbers on a fresh allocator — slot 0 first,
	// not slot poolSize-1. Order doesn't affect correctness (any free slot
	// is as good as any other), but ascending is the predictable,
	// debuggable behavior worth having on purpose.
	free := make([]int, poolSize)
	for i := range free {
		free[i] = poolSize - 1 - i
	}
	return &SubnetAllocator{base: base, poolSize: poolSize, assigned: map[string]int{}, freeSlots: free}, nil
}

// Allocate hands out a /30 for instanceID: hostIP (.1) and guestIP (.2).
// Idempotent — calling it again for an instanceID that still holds a slot
// returns that same slot rather than leaking a second one, which matters
// because ResumeVM calls SetupNetwork again for an instance that may or may
// not have cleanly released its previous allocation.
func (a *SubnetAllocator) Allocate(instanceID string) (hostIP, guestIP string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if slot, ok := a.assigned[instanceID]; ok {
		h, g := a.addresses(slot)
		return h, g, nil
	}
	if len(a.freeSlots) == 0 {
		return "", "", fmt.Errorf("subnet pool exhausted (%d slots all in use)", a.poolSize)
	}
	slot := a.freeSlots[len(a.freeSlots)-1]
	a.freeSlots = a.freeSlots[:len(a.freeSlots)-1]
	a.assigned[instanceID] = slot
	h, g := a.addresses(slot)
	return h, g, nil
}

// Lookup reads instanceID's current allocation without side effects — ok
// is false if it has no active allocation. Used by callers that need to
// know the address of an existing allocation (e.g. to remove a matching
// iptables rule on teardown) without accidentally creating a new one.
func (a *SubnetAllocator) Lookup(instanceID string) (hostIP, guestIP string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot, found := a.assigned[instanceID]
	if !found {
		return "", "", false
	}
	h, g := a.addresses(slot)
	return h, g, true
}

// Release returns instanceID's slot to the pool. Safe to call on an
// instanceID with no active allocation — a no-op, not an error, since
// TeardownNetwork must stay idempotent (§4.3).
func (a *SubnetAllocator) Release(instanceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot, ok := a.assigned[instanceID]
	if !ok {
		return
	}
	delete(a.assigned, instanceID)
	a.freeSlots = append(a.freeSlots, slot)
}

func (a *SubnetAllocator) addresses(slot int) (hostIP, guestIP string) {
	network := a.base + uint32(slot*4)
	return ipString(network + 1), ipString(network + 2)
}

func ipString(v uint32) string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return net.IP(b).String()
}
