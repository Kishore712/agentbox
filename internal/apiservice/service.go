package apiservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
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

// Service implements §4.1's core behavior. Holds no state of its own —
// every read and every invocation goes through ControllerClient; the only
// local computation is routing-token verification and proxying to the
// guest.
type Service struct {
	ctrl   ControllerClient
	tokens *TokenVerifier
	proxy  GuestProxy
}

func NewService(ctrl ControllerClient, tokens *TokenVerifier, proxy GuestProxy) *Service {
	return &Service{ctrl: ctrl, tokens: tokens, proxy: proxy}
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
	return svc.ctrl.CreateInstance(ctx, agentID)
}

func (svc *Service) GetSession(ctx context.Context, sessionID string) (*Instance, error) {
	return svc.ctrl.GetInstance(ctx, sessionID)
}

func (svc *Service) DeleteSession(ctx context.Context, sessionID string) error {
	return svc.ctrl.DeleteInstance(ctx, sessionID)
}

// --- Invocation ---

type InvokeRequest struct {
	Method       string
	SessionID    string // "" means implicit create — only valid for POST
	RoutingToken string // from the X-Routing-Token request header, may be empty
	Header       http.Header
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
	StatusCode   int
	Header       http.Header
	Body         []byte
	SessionID    string // set only when a new/refreshed session id should be reported (X-Session-Id)
	RoutingToken string // set whenever a (possibly refreshed) token should be reported (X-Routing-Token)
}

// Invoke implements §4.1's "Core behavior on invocation" in full: implicit
// creation on POST with no session_id, local token verification on the
// warm path (zero Controller round trip), and a resume fallback whenever
// routing can't be resolved locally — the client never sees a 503 for
// suspend/resume, that dance is entirely internal.
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
		return svc.proxyAndBuildResult(ctx, res, req, true)
	}

	// Warm path: verify the token locally, no Controller call at all, if
	// it's valid and actually belongs to this session_id.
	if claims, err := svc.tokens.Verify(req.RoutingToken); err == nil && claims.InstanceID == req.SessionID {
		result, proxyErr := svc.tryProxyDirect(ctx, claims.GuestIP, claims.GuestPort, req)
		if proxyErr == nil {
			svc.ctrl.Heartbeat(ctx, req.SessionID) // async, sampled — never blocks the response
			result.SessionID = ""                  // unchanged, no need to re-announce it
			result.RoutingToken = req.RoutingToken // unchanged, still valid
			return result, nil
		}
		// Direct connection failed despite a valid token (host rebooted,
		// process crashed, etc.) — don't trust a token proven wrong in
		// practice; fall through to the resume fallback below.
	}

	// Fallback: token missing/expired/invalid, or a direct hit failed.
	// ResumeInstance is idempotent — safe to call even if the instance
	// turns out to still be RUNNING (§4.2), so this single path covers
	// both "genuinely suspended" and "we just lost track of routing info."
	res, err := svc.ctrl.ResumeInstance(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	if res.State == StateFailed {
		return nil, &SessionFailedError{Reason: res.Error}
	}
	return svc.proxyAndBuildResult(ctx, res, req, false)
}

func (svc *Service) tryProxyDirect(ctx context.Context, guestIP string, guestPort int, req InvokeRequest) (*InvokeResult, error) {
	presp, err := svc.proxy.Forward(ctx, guestIP, guestPort, &ProxyRequest{Method: req.Method, Header: req.Header, Body: bytes.NewReader(req.Body)})
	if err != nil {
		return nil, err
	}
	return &InvokeResult{StatusCode: presp.StatusCode, Header: presp.Header, Body: presp.Body}, nil
}

// proxyAndBuildResult is the shared tail of the implicit-create and
// resume-fallback paths: proxy to the (possibly new) guest endpoint, and
// report the routing token back to the client — always refreshed here,
// whether newly issued (create) or re-issued (resume). session_id is only
// announced on create (announceSessionID=true): the client already knows
// it on the resume path, since they supplied it themselves.
func (svc *Service) proxyAndBuildResult(ctx context.Context, res *InstanceResult, req InvokeRequest, announceSessionID bool) (*InvokeResult, error) {
	presp, err := svc.proxy.Forward(ctx, res.GuestIP, res.GuestPort, &ProxyRequest{Method: req.Method, Header: req.Header, Body: bytes.NewReader(req.Body)})
	if err != nil {
		return nil, fmt.Errorf("guest unreachable immediately after create/resume: %w", err)
	}
	result := &InvokeResult{
		StatusCode: presp.StatusCode, Header: presp.Header, Body: presp.Body,
		RoutingToken: res.RoutingToken,
	}
	if announceSessionID {
		result.SessionID = res.InstanceID
	}
	return result, nil
}
