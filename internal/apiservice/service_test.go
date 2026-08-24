package apiservice

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// --- Fakes ---

type fakeControllerClient struct {
	workloads           map[string]*Workload
	instances           map[string]*Instance
	createInstanceRes   *InstanceResult
	createInstanceErr   error
	createInstanceCalls int
	resumeInstanceRes   *InstanceResult
	resumeInstanceErr   error
	resumeInstanceCalls int
	heartbeatCalls      []string
	deleteInstanceErr   error
}

func newFakeControllerClient() *fakeControllerClient {
	return &fakeControllerClient{workloads: map[string]*Workload{}, instances: map[string]*Instance{}}
}

func (f *fakeControllerClient) CreateWorkload(ctx context.Context, req CreateWorkloadRequest) (*Workload, error) {
	w := &Workload{WorkloadID: "wl_test", Name: req.Name, Status: "PROVISIONING"}
	f.workloads[w.WorkloadID] = w
	return w, nil
}
func (f *fakeControllerClient) GetWorkload(ctx context.Context, workloadID string) (*Workload, error) {
	w, ok := f.workloads[workloadID]
	if !ok {
		return nil, ErrNotFound
	}
	return w, nil
}
func (f *fakeControllerClient) DeleteWorkload(ctx context.Context, workloadID string) error {
	delete(f.workloads, workloadID)
	return nil
}
func (f *fakeControllerClient) CreateInstance(ctx context.Context, workloadID string) (*InstanceResult, error) {
	f.createInstanceCalls++
	if f.createInstanceErr != nil {
		return nil, f.createInstanceErr
	}
	return f.createInstanceRes, nil
}
func (f *fakeControllerClient) GetInstance(ctx context.Context, instanceID string) (*Instance, error) {
	inst, ok := f.instances[instanceID]
	if !ok {
		return nil, ErrNotFound
	}
	return inst, nil
}
func (f *fakeControllerClient) ResumeInstance(ctx context.Context, instanceID string) (*InstanceResult, error) {
	f.resumeInstanceCalls++
	if f.resumeInstanceErr != nil {
		return nil, f.resumeInstanceErr
	}
	return f.resumeInstanceRes, nil
}
func (f *fakeControllerClient) Heartbeat(ctx context.Context, instanceID string) {
	f.heartbeatCalls = append(f.heartbeatCalls, instanceID)
}
func (f *fakeControllerClient) DeleteInstance(ctx context.Context, instanceID string) error {
	return f.deleteInstanceErr
}

// fakeHostAgentProxy implements HostAgentProxy — records which
// host_agent_addr each call went to, so tests can assert this service
// never resolves a guest address itself (§4.1/§4.3: only the Host Agent
// ever does that).
type fakeHostAgentProxy struct {
	response  *ProxyResponse
	err       error
	forwarded []string // hostAgentAddr per call
}

func (f *fakeHostAgentProxy) Forward(ctx context.Context, hostAgentAddr, instanceID string, req *ProxyRequest) (*ProxyResponse, error) {
	f.forwarded = append(f.forwarded, hostAgentAddr)
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// proxyThatFailsOnce fails the first Forward call and succeeds
// thereafter — simulates "cached host_agent_addr, but the Host Agent
// rejected the call" (registry-miss or guest-unreachable, §4.3) without
// needing a real network failure.
type proxyThatFailsOnce struct {
	calls     []string
	failed    bool
	onSuccess *ProxyResponse
}

func (p *proxyThatFailsOnce) Forward(ctx context.Context, hostAgentAddr, instanceID string, req *ProxyRequest) (*ProxyResponse, error) {
	p.calls = append(p.calls, hostAgentAddr)
	if !p.failed {
		p.failed = true
		return nil, ErrHostAgentRoutingFailed
	}
	return p.onSuccess, nil
}

// --- Invoke: implicit creation (no session_id) ---

func TestInvoke_ImplicitCreate_Success(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.5:9000"}
	proxy := &fakeHostAgentProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("hello")}}
	svc := NewService(ctrl, proxy, 0)

	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.SessionID != "inst-1" {
		t.Errorf("expected session_id to be announced on create, got %+v", res)
	}
	if string(res.Body) != "hello" {
		t.Errorf("body = %q", res.Body)
	}
	if len(proxy.forwarded) != 1 || proxy.forwarded[0] != "10.0.1.5:9000" {
		t.Errorf("expected exactly one proxy call to the Host Agent, got %v", proxy.forwarded)
	}
}

func TestInvoke_ImplicitCreate_GETNotAllowed(t *testing.T) {
	svc := NewService(newFakeControllerClient(), &fakeHostAgentProxy{}, 0)
	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodGet, Header: http.Header{}})
	if !errors.Is(err, ErrSessionIDRequired) {
		t.Fatalf("got %v, want ErrSessionIDRequired", err)
	}
}

func TestInvoke_ImplicitCreate_Failed(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateFailed, Error: "boot timeout"}
	svc := NewService(ctrl, &fakeHostAgentProxy{}, 0)

	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}})
	var failed *SessionFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("got %v, want *SessionFailedError", err)
	}
	if failed.Reason != "boot timeout" {
		t.Errorf("reason = %q", failed.Reason)
	}
}

func TestInvoke_ImplicitCreate_AtCapacity(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceErr = ErrAtCapacity
	svc := NewService(ctrl, &fakeHostAgentProxy{}, 0)

	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}})
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("got %v, want ErrAtCapacity", err)
	}
}

// --- Invoke: warm path (cache hit, zero Controller calls for routing) ---

func TestInvoke_WarmPath_NoControllerCallForRouting(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.5:9000"}
	proxy := &fakeHostAgentProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("warm")}}
	svc := NewService(ctrl, proxy, 0)

	// Cold start populates the cache.
	if _, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}

	// If the warm path is broken and falls through to resume, this would be
	// used — leaving it unset (nil) makes the test fail loudly instead of
	// silently succeeding via the fallback.
	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(res.Body) != "warm" {
		t.Errorf("body = %q", res.Body)
	}
	if res.SessionID != "" {
		t.Errorf("warm path should not re-announce session_id, got %q", res.SessionID)
	}
	if len(proxy.forwarded) != 2 || proxy.forwarded[1] != "10.0.1.5:9000" {
		t.Errorf("expected a second direct proxy call via the cached host_agent_addr, got %v", proxy.forwarded)
	}
	if ctrl.resumeInstanceCalls != 0 {
		t.Errorf("expected zero ResumeInstance calls on a cache hit, got %d", ctrl.resumeInstanceCalls)
	}
	if len(ctrl.heartbeatCalls) != 1 || ctrl.heartbeatCalls[0] != "inst-1" {
		t.Errorf("expected exactly one async heartbeat for inst-1, got %v", ctrl.heartbeatCalls)
	}
}

func TestInvoke_WarmPath_DifferentSessionsRouteIndependently(t *testing.T) {
	ctrl := newFakeControllerClient()
	proxy := &fakeHostAgentProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("ok")}}
	svc := NewService(ctrl, proxy, 0)

	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.5:9000"}
	if _, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-2", State: StateRunning, HostID: "host-2", HostAgentAddr: "10.0.1.6:9000"}
	if _, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, SessionID: "inst-1", Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, SessionID: "inst-2", Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}

	want := []string{"10.0.1.5:9000", "10.0.1.6:9000", "10.0.1.5:9000", "10.0.1.6:9000"}
	if len(proxy.forwarded) != len(want) {
		t.Fatalf("forwarded = %v, want %v", proxy.forwarded, want)
	}
	for i, addr := range want {
		if proxy.forwarded[i] != addr {
			t.Errorf("call %d went to %q, want %q — sessions must not cross-route", i, proxy.forwarded[i], addr)
		}
	}
}

// --- Invoke: fallback to resume (cache miss, or the Host Agent rejected the call) ---

func TestInvoke_CacheMiss_FallsBackToResume(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.9:9000"}
	proxy := &fakeHostAgentProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("resumed")}}
	svc := NewService(ctrl, proxy, 0)

	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// session_id is only announced when newly assigned (implicit create) —
	// the client already knows it here, they supplied it themselves.
	if res.SessionID != "" {
		t.Errorf("resume should not re-announce session_id (client already has it), got %q", res.SessionID)
	}
	if ctrl.resumeInstanceCalls != 1 {
		t.Errorf("expected exactly one ResumeInstance call on a cache miss, got %d", ctrl.resumeInstanceCalls)
	}
	if len(proxy.forwarded) != 1 || proxy.forwarded[0] != "10.0.1.9:9000" {
		t.Errorf("expected the resume-fallback host_agent_addr to be used, got %v", proxy.forwarded)
	}
}

func TestInvoke_HostAgentRejectsCachedEntry_FallsBackToResume(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.5:9000"}
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.9:9000"}

	proxy := &proxyThatFailsOnce{onSuccess: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("ok")}}
	svc := NewService(ctrl, proxy, 0)

	// CreateSession populates the cache directly from the Controller
	// response — no proxy call involved, so it doesn't consume
	// proxyThatFailsOnce's one scripted failure.
	if _, err := svc.CreateSession(context.Background(), "agt_1"); err != nil {
		t.Fatal(err)
	}

	// Warm call: the cached address is stale (e.g. the instance suspended
	// since caching) — the Host Agent rejects it, so this must fall back
	// to Controller.ResumeInstance and retry via the fresh address.
	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(proxy.calls) != 2 {
		t.Fatalf("expected 2 proxy attempts (rejected cached addr, then resume-fallback success), got %d: %v", len(proxy.calls), proxy.calls)
	}
	if proxy.calls[0] != "10.0.1.5:9000" || proxy.calls[1] != "10.0.1.9:9000" {
		t.Errorf("calls = %v, want [10.0.1.5:9000 10.0.1.9:9000]", proxy.calls)
	}
	if string(res.Body) != "ok" {
		t.Errorf("body = %q", res.Body)
	}
}

func TestInvoke_ResumeReturnsFailed(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateFailed, Error: "snapshot corrupt"}
	svc := NewService(ctrl, &fakeHostAgentProxy{}, 0)

	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", Header: http.Header{},
	})
	var failed *SessionFailedError
	if !errors.As(err, &failed) || failed.Reason != "snapshot corrupt" {
		t.Fatalf("got %v, want SessionFailedError{snapshot corrupt}", err)
	}
}

func TestInvoke_UnknownSession(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceErr = ErrNotFound
	svc := NewService(ctrl, &fakeHostAgentProxy{}, 0)

	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "does-not-exist", Header: http.Header{},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestInvoke_GET_RequiresSessionID(t *testing.T) {
	svc := NewService(newFakeControllerClient(), &fakeHostAgentProxy{}, 0)
	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodGet, SessionID: "", Header: http.Header{}})
	if !errors.Is(err, ErrSessionIDRequired) {
		t.Fatalf("got %v, want ErrSessionIDRequired", err)
	}
}

// --- DeleteSession evicts the routing cache ---

func TestDeleteSession_EvictsCacheEntry(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.5:9000"}
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.9:9000"}
	proxy := &fakeHostAgentProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("ok")}}
	svc := NewService(ctrl, proxy, 0)

	if _, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSession(context.Background(), "inst-1"); err != nil {
		t.Fatal(err)
	}

	// The cache must be empty now — the next call for this session_id has
	// to go through the Controller again (it's a cache-miss), not reuse
	// the deleted instance's stale host_agent_addr.
	if _, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, SessionID: "inst-1", Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	if ctrl.resumeInstanceCalls != 1 {
		t.Errorf("expected DeleteSession to force a ResumeInstance call on next use, got %d calls", ctrl.resumeInstanceCalls)
	}
}
