package hostagent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProxyRequest/ProxyResponse are the minimal shape needed to forward a
// request to a guest and relay its response back — identical in shape to
// what the REST API Service used to do directly against a guest before
// 3.2 moved this behind the Host Agent (design doc §4.1/§4.3). The two
// services never share Go types, so this is a deliberate duplicate of
// apiservice's version, not an import.
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

// GuestProxy is the actual last-hop HTTP forwarding to a specific
// guest_ip:guest_port — the mechanics, kept separate from VMManager.Proxy
// (which resolves instance_id → guest_ip:port via the live registry) so
// VMManager's own tests can keep using fakes with zero real HTTP involved.
type GuestProxy interface {
	Forward(ctx context.Context, guestIP string, guestPort int, req *ProxyRequest) (*ProxyResponse, error)
}

type HTTPGuestProxy struct {
	client *http.Client
}

func NewHTTPGuestProxy(timeout time.Duration) *HTTPGuestProxy {
	return &HTTPGuestProxy{client: &http.Client{Timeout: timeout}}
}

// Forward hits the guest's single fixed doorway ("/") — matching the
// platform's RPC-style convention (§4.1), not a path-preserving reverse
// proxy.
func (p *HTTPGuestProxy) Forward(ctx context.Context, guestIP string, guestPort int, req *ProxyRequest) (*ProxyResponse, error) {
	url := fmt.Sprintf("http://%s:%d/", guestIP, guestPort)
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, req.Body)
	if err != nil {
		return nil, fmt.Errorf("build guest request: %w", err)
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
		// Connection-level failure — distinct from the registry-miss case
		// (ErrInstanceNotRegistered in manager.go): the instance IS
		// registered here, but the guest itself didn't answer. The HTTP
		// layer maps this to 502, not 404 (design doc §4.3, "Flow —
		// Data-plane proxy" step 4).
		return nil, fmt.Errorf("guest unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read guest response: %w", err)
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
