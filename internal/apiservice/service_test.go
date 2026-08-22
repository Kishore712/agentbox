package apiservice

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// --- Fakes ---

type fakeControllerClient struct {
	workloads         map[string]*Workload
	instances         map[string]*Instance
	createInstanceRes *InstanceResult
	createInstanceErr error
	resumeInstanceRes *InstanceResult
	resumeInstanceErr error
	heartbeatCalls    []string
	deleteInstanceErr error
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

type fakeGuestProxy struct {
	response  *ProxyResponse
	err       error
	forwarded []string // "guestIP:guestPort" per call, for assertions
}

func (f *fakeGuestProxy) Forward(ctx context.Context, guestIP string, guestPort int, req *ProxyRequest) (*ProxyResponse, error) {
	f.forwarded = append(f.forwarded, guestIP)
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// issueTestToken signs a token exactly the way the Controller's issuer
// would (§4.2's routing token contract) — constructed here directly since
// apiservice only ever verifies, never issues (§4.1).
func issueTestToken(t *testing.T, secret []byte, instanceID, guestIP string, guestPort int, ttl time.Duration) string {
	t.Helper()
	claims := RoutingClaims{
		InstanceID: instanceID, GuestIP: guestIP, GuestPort: guestPort,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl))},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// --- Invoke: implicit creation (no session_id) ---

func TestInvoke_ImplicitCreate_Success(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, GuestIP: "172.16.0.2", GuestPort: 8080, RoutingToken: "tok-1"}
	proxy := &fakeGuestProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("hello")}}
	svc := NewService(ctrl, NewTokenVerifier([]byte("secret")), proxy)

	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.SessionID != "inst-1" || res.RoutingToken != "tok-1" {
		t.Errorf("expected session_id/token to be announced on create, got %+v", res)
	}
	if string(res.Body) != "hello" {
		t.Errorf("body = %q", res.Body)
	}
	if len(proxy.forwarded) != 1 || proxy.forwarded[0] != "172.16.0.2" {
		t.Errorf("expected exactly one proxy call to 172.16.0.2, got %v", proxy.forwarded)
	}
}

func TestInvoke_ImplicitCreate_GETNotAllowed(t *testing.T) {
	svc := NewService(newFakeControllerClient(), NewTokenVerifier([]byte("secret")), &fakeGuestProxy{})
	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodGet, Header: http.Header{}})
	if !errors.Is(err, ErrSessionIDRequired) {
		t.Fatalf("got %v, want ErrSessionIDRequired", err)
	}
}

func TestInvoke_ImplicitCreate_Failed(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateFailed, Error: "boot timeout"}
	svc := NewService(ctrl, NewTokenVerifier([]byte("secret")), &fakeGuestProxy{})

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
	svc := NewService(ctrl, NewTokenVerifier([]byte("secret")), &fakeGuestProxy{})

	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodPost, Header: http.Header{}})
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("got %v, want ErrAtCapacity", err)
	}
}

// --- Invoke: warm path (valid token, zero Controller calls for routing) ---

func TestInvoke_WarmPath_NoControllerCallForRouting(t *testing.T) {
	secret := []byte("shared-secret")
	ctrl := newFakeControllerClient()
	// If the warm path is broken and falls through to resume, this would
	// be used — leaving it unset (nil) makes the test fail loudly instead
	// of silently succeeding via the fallback.
	proxy := &fakeGuestProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("warm")}}
	svc := NewService(ctrl, NewTokenVerifier(secret), proxy)

	token := issueTestToken(t, secret, "inst-1", "172.16.0.5", 8080, time.Minute)
	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", RoutingToken: token, Header: http.Header{},
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
	if res.RoutingToken != token {
		t.Errorf("expected the same token echoed back, got %q", res.RoutingToken)
	}
	if len(proxy.forwarded) != 1 || proxy.forwarded[0] != "172.16.0.5" {
		t.Errorf("expected exactly one direct proxy call to 172.16.0.5, got %v", proxy.forwarded)
	}
	if len(ctrl.heartbeatCalls) != 1 || ctrl.heartbeatCalls[0] != "inst-1" {
		t.Errorf("expected exactly one async heartbeat for inst-1, got %v", ctrl.heartbeatCalls)
	}
}

func TestInvoke_WarmPath_TokenForDifferentSessionIgnored(t *testing.T) {
	secret := []byte("shared-secret")
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-2", State: StateRunning, GuestIP: "172.16.0.9", GuestPort: 8080, RoutingToken: "fresh-tok"}
	proxy := &fakeGuestProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("via-resume")}}
	svc := NewService(ctrl, NewTokenVerifier(secret), proxy)

	// Token is valid but for a DIFFERENT instance than the one requested —
	// must not be trusted for routing.
	token := issueTestToken(t, secret, "inst-1", "172.16.0.5", 8080, time.Minute)
	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-2", RoutingToken: token, Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(res.Body) != "via-resume" {
		t.Errorf("expected the resume-fallback path to have been taken, got body %q", res.Body)
	}
	if len(proxy.forwarded) != 1 || proxy.forwarded[0] != "172.16.0.9" {
		t.Errorf("expected proxy call to the resumed instance's IP, got %v", proxy.forwarded)
	}
}

// --- Invoke: fallback to resume (expired/missing/invalid token, or a failed direct hit) ---

func TestInvoke_MissingToken_FallsBackToResume(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, GuestIP: "172.16.0.9", GuestPort: 8080, RoutingToken: "fresh-tok"}
	proxy := &fakeGuestProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("resumed")}}
	svc := NewService(ctrl, NewTokenVerifier([]byte("secret")), proxy)

	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", RoutingToken: "", Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// session_id is only announced when newly assigned (implicit create) —
	// the client already knows it here, they supplied it themselves. The
	// token, however, genuinely changed on resume and must be refreshed.
	if res.SessionID != "" {
		t.Errorf("resume should not re-announce session_id (client already has it), got %q", res.SessionID)
	}
	if res.RoutingToken != "fresh-tok" {
		t.Errorf("expected the refreshed token to be announced after a resume, got %q", res.RoutingToken)
	}
}

func TestInvoke_ExpiredToken_FallsBackToResume(t *testing.T) {
	secret := []byte("shared-secret")
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, GuestIP: "172.16.0.9", GuestPort: 8080, RoutingToken: "fresh-tok"}
	proxy := &fakeGuestProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("resumed")}}
	svc := NewService(ctrl, NewTokenVerifier(secret), proxy)

	expired := issueTestToken(t, secret, "inst-1", "172.16.0.5", 8080, -time.Minute) // already expired
	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", RoutingToken: expired, Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(proxy.forwarded) != 1 || proxy.forwarded[0] != "172.16.0.9" {
		t.Errorf("expected the resume-fallback endpoint to be used, got %v", proxy.forwarded)
	}
}

func TestInvoke_DirectConnectionFails_FallsBackToResume(t *testing.T) {
	secret := []byte("shared-secret")
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, GuestIP: "172.16.0.9", GuestPort: 8080, RoutingToken: "fresh-tok"}

	proxy := &proxyThatFailsOnce{onSuccess: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("ok")}}
	svc := NewService(ctrl, NewTokenVerifier(secret), proxy)

	validButStale := issueTestToken(t, secret, "inst-1", "172.16.0.5", 8080, time.Minute) // valid token, but host actually unreachable
	res, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "inst-1", RoutingToken: validButStale, Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(proxy.calls) != 2 {
		t.Fatalf("expected 2 proxy attempts (failed direct hit, then resume-fallback success), got %d: %v", len(proxy.calls), proxy.calls)
	}
	if proxy.calls[0] != "172.16.0.5" || proxy.calls[1] != "172.16.0.9" {
		t.Errorf("calls = %v, want [172.16.0.5 172.16.0.9]", proxy.calls)
	}
	if string(res.Body) != "ok" {
		t.Errorf("body = %q", res.Body)
	}
}

func TestInvoke_ResumeReturnsFailed(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateFailed, Error: "snapshot corrupt"}
	svc := NewService(ctrl, NewTokenVerifier([]byte("secret")), &fakeGuestProxy{})

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
	svc := NewService(ctrl, NewTokenVerifier([]byte("secret")), &fakeGuestProxy{})

	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{
		Method: http.MethodPost, SessionID: "does-not-exist", Header: http.Header{},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestInvoke_GET_RequiresSessionID(t *testing.T) {
	svc := NewService(newFakeControllerClient(), NewTokenVerifier([]byte("secret")), &fakeGuestProxy{})
	_, err := svc.Invoke(context.Background(), "agt_1", InvokeRequest{Method: http.MethodGet, SessionID: "", Header: http.Header{}})
	if !errors.Is(err, ErrSessionIDRequired) {
		t.Fatalf("got %v, want ErrSessionIDRequired", err)
	}
}

// proxyThatFailsOnce fails the first Forward call and succeeds thereafter —
// simulates "valid token, but the host is actually unreachable" without
// needing a real network failure.
type proxyThatFailsOnce struct {
	calls     []string
	failed    bool
	onSuccess *ProxyResponse
}

func (p *proxyThatFailsOnce) Forward(ctx context.Context, guestIP string, guestPort int, req *ProxyRequest) (*ProxyResponse, error) {
	p.calls = append(p.calls, guestIP)
	if !p.failed {
		p.failed = true
		return nil, errConnRefused
	}
	return p.onSuccess, nil
}

var errConnRefused = errors.New("connection refused")
