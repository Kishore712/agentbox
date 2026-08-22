package apiservice

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProxyRequest/ProxyResponse are the minimal shape needed to forward a
// client's invoke request to a guest and relay its response back
// (§4.1: "full request/response passthrough — method, headers minus
// hop-by-hop, body").
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

// GuestProxy forwards an invoke request directly to a guest microVM — the
// data-plane path that never touches the Controller (§3.1/§4.1). Interface
// so invoke logic can be unit tested against a fake guest, without a real
// microVM. Every invocation hits a single fixed path on the guest ("/"),
// matching the design's RPC-style single doorway rather than a
// path-preserving reverse proxy.
type GuestProxy interface {
	Forward(ctx context.Context, guestIP string, guestPort int, req *ProxyRequest) (*ProxyResponse, error)
}

type HTTPGuestProxy struct {
	client *http.Client
}

func NewHTTPGuestProxy(timeout time.Duration) *HTTPGuestProxy {
	return &HTTPGuestProxy{client: &http.Client{Timeout: timeout}}
}

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
		// Connection-level failure — this is the signal callers use to
		// distinguish "guest unreachable" (fall back to resume) from a
		// normal error response the guest app itself returned.
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
