package apiservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProxyRequest/ProxyResponse are the minimal shape needed to forward a
// client's invoke request through the owning Host Agent and relay its
// response back (§4.1: "full request/response passthrough — method,
// headers minus hop-by-hop, body").
type ProxyRequest struct {
	Method string
	Header http.Header
	Body   io.Reader
}

type ProxyResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// hopByHopHeaders must never be forwarded in either direction (RFC 7230 §6.1).
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// proxyErrorHeader must match the Host Agent's own constant of the same
// name (internal/hostagent/httpapi.go) — the two services share no Go
// types, only this header-name convention over HTTP. Its presence, not the
// response's status code, is what marks a response as a Host-Agent-
// generated routing failure rather than something relayed from the guest
// app, which can legitimately return any status — including 404 or 502 —
// as a normal response on the single fixed doorway.
const proxyErrorHeader = "X-Agentbox-Proxy-Error"

// ErrHostAgentRoutingFailed covers both of the Host Agent's own proxy
// failure modes (registry-miss and guest-unreachable, §4.3) — Invoke's
// fallback treats them identically: refresh routing via
// Controller.ResumeInstance and retry once (§4.1).
var ErrHostAgentRoutingFailed = errors.New("host agent could not route to instance")

// HostAgentProxy forwards an invoke request to the Host Agent that owns a
// given instance — the REST API Service never dials a guest directly as of
// design doc 3.2; the Host Agent is the only thing that resolves
// instance_id to a live guest_ip:port, from its own in-memory registry
// (§4.3), and only at the moment it forwards.
type HostAgentProxy interface {
	Forward(ctx context.Context, hostAgentAddr, instanceID string, req *ProxyRequest) (*ProxyResponse, error)
}

type HTTPHostAgentProxy struct {
	client *http.Client
}

func NewHTTPHostAgentProxy(timeout time.Duration) *HTTPHostAgentProxy {
	return &HTTPHostAgentProxy{client: &http.Client{Timeout: timeout}}
}

func (p *HTTPHostAgentProxy) Forward(ctx context.Context, hostAgentAddr, instanceID string, req *ProxyRequest) (*ProxyResponse, error) {
	url := fmt.Sprintf("http://%s/vm/%s/proxy", hostAgentAddr, instanceID)
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, req.Body)
	if err != nil {
		return nil, fmt.Errorf("build host agent request: %w", err)
	}
	for k, vv := range req.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// Transport-level failure talking to the Host Agent itself (not a
		// routing failure it reported) — same fallback applies either way.
		return nil, fmt.Errorf("%w: host agent unreachable: %v", ErrHostAgentRoutingFailed, err)
	}
	defer resp.Body.Close()

	if resp.Header.Get(proxyErrorHeader) != "" {
		return nil, fmt.Errorf("%w: %s", ErrHostAgentRoutingFailed, resp.Header.Get(proxyErrorHeader))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read host agent response: %w", err)
	}
	header := resp.Header.Clone()
	for _, h := range hopByHopHeaders {
		header.Del(h)
	}
	return &ProxyResponse{StatusCode: resp.StatusCode, Header: header, Body: body}, nil
}

func isHopByHop(header string) bool {
	for _, h := range hopByHopHeaders {
		if header == h {
			return true
		}
	}
	return false
}
