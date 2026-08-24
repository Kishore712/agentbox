package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

// HTTPHostAgentClient is the real implementation of HostAgentClient (§4.2
// "B) Host Agent API"), talking REST/JSON over the internal network to
// whichever Host Agent owns a given instance. Only exercised once a real
// Host Agent exists to talk to — the Controller's own unit tests use the
// fakeHostAgent in service_test.go instead.
type HTTPHostAgentClient struct {
	client *http.Client
}

func NewHTTPHostAgentClient() *HTTPHostAgentClient {
	return &HTTPHostAgentClient{client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *HTTPHostAgentClient) BootVM(ctx context.Context, hostAddr string, req BootVMRequest) (VMEndpoint, error) {
	var ep VMEndpoint
	err := c.doJSON(ctx, http.MethodPost, hostAddr, "/vm", req, &ep)
	return ep, err
}

func (c *HTTPHostAgentClient) SuspendVM(ctx context.Context, hostAddr, instanceID string) error {
	return c.doJSON(ctx, http.MethodPost, hostAddr, "/vm/"+instanceID+"/suspend", nil, nil)
}

func (c *HTTPHostAgentClient) ResumeVM(ctx context.Context, hostAddr, instanceID string) (VMEndpoint, error) {
	var ep VMEndpoint
	err := c.doJSON(ctx, http.MethodPost, hostAddr, "/vm/"+instanceID+"/resume", nil, &ep)
	if err != nil {
		var herr *httpStatusError
		if isHTTPStatusError(err, &herr) && herr.status == http.StatusNotFound {
			return VMEndpoint{}, fmt.Errorf("%w: %s", ErrSnapshotMissing, herr.body)
		}
	}
	return ep, err
}

func (c *HTTPHostAgentClient) DeleteVM(ctx context.Context, hostAddr, instanceID string) error {
	return c.doJSON(ctx, http.MethodDelete, hostAddr, "/vm/"+instanceID, nil, nil)
}

func (c *HTTPHostAgentClient) HasRootfs(ctx context.Context, hostAddr, rootfsRef string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+hostAddr+"/golden-rootfs?path="+url.QueryEscape(rootfsRef), nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("host agent request failed: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("host agent returned unexpected status %d checking rootfs %q", resp.StatusCode, rootfsRef)
	}
}

// PushRootfs streams the local rootfs.ext4 file at rootfsRef to hostAddr.
// Only ever called after HasRootfs reports it's missing (§4.6) — this is
// the (potentially large) transfer that check exists to avoid repeating.
func (c *HTTPHostAgentClient) PushRootfs(ctx context.Context, hostAddr, rootfsRef string) error {
	f, err := os.Open(rootfsRef)
	if err != nil {
		return fmt.Errorf("open local rootfs %q: %w", rootfsRef, err)
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+hostAddr+"/golden-rootfs?path="+url.QueryEscape(rootfsRef), f)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("host agent request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return &httpStatusError{status: resp.StatusCode, body: errBody.Error}
	}
	return nil
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("host agent returned %d: %s", e.status, e.body)
}

func isHTTPStatusError(err error, target **httpStatusError) bool {
	if e, ok := err.(*httpStatusError); ok {
		*target = e
		return true
	}
	return false
}

func (c *HTTPHostAgentClient) doJSON(ctx context.Context, method, hostAddr, path string, body, out any) error {
	var reqBody bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = *bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+hostAddr+path, &reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("host agent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return &httpStatusError{status: resp.StatusCode, body: errBody.Error}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
