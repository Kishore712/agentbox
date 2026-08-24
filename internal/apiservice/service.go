package apiservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrSessionIDRequired: GET never implicitly creates a session (§4.1) — if
// session_id is omitted on GET, that's a 400, not a create.
var ErrSessionIDRequired = errors.New("session_id is required for this method")

// SessionFailedError wraps a FAILED instance/session result — maps to 409:
// a failed session doesn't self-heal, the client must delete and recreate.
type SessionFailedError struct {
	Reason string
}

func (e *SessionFailedError) Error() string { return "session failed: " + e.Reason }

// Service implements §4.1's core behavior. Holds no durable state of its
// own — every read and every invocation goes through ControllerClient. The
// one piece of local state is the routing cache (§4.1) — ephemeral,
// rebuildable, never authoritative.
type Service struct {
	ctrl  ControllerClient
	proxy HostAgentProxy
	cache *routingCache
}

func NewService(ctrl ControllerClient, proxy HostAgentProxy, cacheTTL time.Duration) *Service {
	return &Service{ctrl: ctrl, proxy: proxy, cache: newRoutingCache(cacheTTL)}
}

// --- Agent (Workload facade) ---

func (svc *Service) CreateAgent(ctx context.Context, req CreateWorkloadRequest) (*Workload, error) {
	return svc.ctrl.CreateWorkload(ctx, req)
}

func (svc *Service) GetAgent(ctx context.Context, agentID string) (*Workload, error) {
	return svc.ctrl.GetWorkload(ctx, agentID)
}

func (svc *Service) DeleteAgent(ctx context.Context, agentID string) error {
	return svc.ctrl.DeleteWorkload(ctx, agentID)
}

// --- Session (Instance facade) ---

func (svc *Service) CreateSession(ctx context.Context, agentID string) (*InstanceResult, error) {
	res, err := svc.ctrl.CreateInstance(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if res.State == StateRunning {
		svc.cache.set(res.InstanceID, res.HostID, res.HostAgentAddr)
	}
	return res, nil
}

func (svc *Service) GetSession(ctx context.Context, sessionID string) (*Instance, error) {
	return svc.ctrl.GetInstance(ctx, sessionID)
}

// DeleteSession evicts the routing cache entry immediately, rather than
// waiting for a proxy call to eventually fail — the session is gone the
// moment the client asked for it to be, no reason to keep routing to it in
// the meantime.
func (svc *Service) DeleteSession(ctx context.Context, sessionID string) error {
	svc.cache.evict(sessionID)
	return svc.ctrl.DeleteInstance(ctx, sessionID)
}

// --- Invocation ---

type InvokeRequest struct {
	Method    string
	SessionID string // "" means implicit create — only valid for POST
	Header    http.Header
	// Body is buffered in full by the caller (the HTTP handler) rather than
	// streamed, specifically so it can be replayed if the direct-proxy
	// attempt fails partway through and Invoke falls back to
	// resume-then-retry — a plain io.Reader would already be drained/
	// partially-consumed by the failed attempt and send a truncated body
	// on retry. An acceptable prototype-scale tradeoff (§1: "let's not
	// worry about scale"); a streaming design would need real replay
	// buffering or to forbid retry-after-partial-send instead.
	Body []byte
}

type InvokeResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	SessionID  string // set only when a new session id should be reported (X-Session-Id)
}

// Invoke implements §4.1's "Core behavior on invocation" in full: implicit
// creation on POST with no session_id, a local routing-cache hit on the
// warm path (zero Controller round trip, and the Host Agent — never this
// service — resolves the live guest address), and a resume fallback
// whenever routing can't be resolved locally — the client never sees a 503
// for suspend/resume, that dance is entirely internal.
func (svc *Service) Invoke(ctx context.Context, agentID string, req InvokeRequest) (*InvokeResult, error) {
	if req.SessionID == "" {
		if req.Method != http.MethodPost {
			return nil, ErrSessionIDRequired
		}
		res, err := svc.ctrl.CreateInstance(ctx, agentID)
		if err != nil {
			return nil, err
		}
		if res.State == StateFailed {
			return nil, &SessionFailedError{Reason: res.Error}
		}
		svc.cache.set(res.InstanceID, res.HostID, res.HostAgentAddr)
		return svc.proxyAndBuildResult(ctx, res.InstanceID, res.HostAgentAddr, req, true)
	}

	// Warm path: cache hit, zero Controller round trip. The Host Agent
	// resolves instance_id to a live guest address itself (§4.3) — this
	// service never sees or caches one.
	if hostAgentAddr, ok := svc.cache.get(req.SessionID); ok {
		result, proxyErr := svc.tryProxyDirect(ctx, hostAgentAddr, req.SessionID, req)
		if proxyErr == nil {
			svc.ctrl.Heartbeat(ctx, req.SessionID) // async, sampled — never blocks the response
			return result, nil
		}
		// The Host Agent rejected the call (registry miss — likely
		// suspended — or the guest itself was unreachable, §4.3). Don't
		// trust a cache entry proven wrong in practice; fall through.
	}

	// Fallback: cache miss, or the Host Agent call failed. ResumeInstance
	// is idempotent — safe to call even if the instance turns out to still
	// be RUNNING (§4.2), so this single path covers both "genuinely
	// suspended" and "we just lost track of routing info."
	res, err := svc.ctrl.ResumeInstance(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	if res.State == StateFailed {
		svc.cache.evict(req.SessionID)
		return nil, &SessionFailedError{Reason: res.Error}
	}
	svc.cache.set(res.InstanceID, res.HostID, res.HostAgentAddr)
	return svc.proxyAndBuildResult(ctx, res.InstanceID, res.HostAgentAddr, req, false)
}

func (svc *Service) tryProxyDirect(ctx context.Context, hostAgentAddr, instanceID string, req InvokeRequest) (*InvokeResult, error) {
	presp, err := svc.proxy.Forward(ctx, hostAgentAddr, instanceID, &ProxyRequest{Method: req.Method, Header: req.Header, Body: bytes.NewReader(req.Body)})
	if err != nil {
		return nil, err
	}
	return &InvokeResult{StatusCode: presp.StatusCode, Header: presp.Header, Body: presp.Body}, nil
}

// proxyAndBuildResult is the shared tail of the implicit-create and
// resume-fallback paths: proxy through the owning Host Agent, and report
// session_id back only on create (announceSessionID=true) — the client
// already knows it on the resume path, since they supplied it themselves.
func (svc *Service) proxyAndBuildResult(ctx context.Context, instanceID, hostAgentAddr string, req InvokeRequest, announceSessionID bool) (*InvokeResult, error) {
	presp, err := svc.proxy.Forward(ctx, hostAgentAddr, instanceID, &ProxyRequest{Method: req.Method, Header: req.Header, Body: bytes.NewReader(req.Body)})
	if err != nil {
		return nil, fmt.Errorf("host agent unreachable immediately after create/resume: %w", err)
	}
	result := &InvokeResult{StatusCode: presp.StatusCode, Header: presp.Header, Body: presp.Body}
	if announceSessionID {
		result.SessionID = instanceID
	}
	return result, nil
}
