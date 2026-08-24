package hostagent

import "sync"

// instanceRegistry is the Host Agent's live, in-memory record of which
// instances are actually running on this host right now, and where. It's
// the one piece of state that makes the data-plane proxy endpoint possible
// (design doc §4.3, "Live instance registry") — nothing before this needed
// the Host Agent to remember an instance past the call that booted it.
//
// Written by BootVM/ResumeVM on success, deleted by SuspendVM/DeleteVM
// unconditionally, read only by Proxy. In-memory only — a Host Agent
// process restart loses it; see the design doc for why that's an accepted
// gap for the prototype (the system self-heals via the existing resume
// fallback on next invoke).
type instanceRegistry struct {
	mu      sync.RWMutex
	entries map[string]VMEndpoint
}

func newInstanceRegistry() *instanceRegistry {
	return &instanceRegistry{entries: make(map[string]VMEndpoint)}
}

func (r *instanceRegistry) set(instanceID string, ep VMEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[instanceID] = ep
}

func (r *instanceRegistry) delete(instanceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, instanceID)
}

func (r *instanceRegistry) get(instanceID string) (VMEndpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ep, ok := r.entries[instanceID]
	return ep, ok
}
