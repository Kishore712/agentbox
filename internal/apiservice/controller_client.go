// Package apiservice implements the REST API Service (design doc §4.1):
// the only public-facing surface. Stateless — no Redis dependency, no
// direct access to workload:*/instance:*/host:* records. Every read and
// every invocation goes through the Controller's API (§4.4's hard service
// boundary).
package apiservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	ErrNotFound         = errors.New("not found")
	ErrWorkloadNotReady = errors.New("workload not ready")
	ErrAtCapacity       = errors.New("at capacity")
)

// Workload/Instance mirror the JSON shapes the Controller's HTTP API
// returns (§4.2). Defined locally rather than imported from the controller
// package — the two services only ever talk over HTTP, matching the strict
// boundary established for the whole platform.
type Workload struct {
	WorkloadID             string   `json:"workload_id"`
	Name                   string   `json:"name"`
	ImageRef               string   `json:"image_ref"`
	Status                 string   `json:"status"`
	IdleTimeoutSeconds     int      `json:"idle_timeout_seconds"`
	EgressAllowlist        []string `json:"egress_allowlist"`
	VCPUs                  int      `json:"vcpus"`
	MemoryMiB              int      `json:"memory_mib"`
	MaxConcurrentInstances int      `json:"max_concurrent_instances"`
	CreatedAt              int64    `json:"created_at"`
}

type Instance struct {
	InstanceID string `json:"instance_id"`
	WorkloadID string `json:"workload_id"`
	State      string `json:"state"`
	HostID     string `json:"host_id"`
	LastActive int64  `json:"last_active"`
	CreatedAt  int64  `json:"created_at"`
}

// InstanceResult mirrors the Controller's CreateInstance/ResumeInstance
// response body (§4.2) — either routing info (RUNNING) or an error reason
// (FAILED). host_agent_addr, not guest_ip/guest_port: this service never
// resolves an instance to a live guest address itself — the Host Agent
// does that, from its own live registry, at proxy time (§4.3).
type InstanceResult struct {
	InstanceID    string `json:"instance_id"`
	State         string `json:"state"`
	HostID        string `json:"host_id"`
	HostAgentAddr string `json:"host_agent_addr"`
	Error         string `json:"error"`
}

const (
	StateRunning = "RUNNING"
	StateFailed  = "FAILED"
)

type CreateWorkloadRequest struct {
	Name                   string   `json:"name"`
	ImageRef               string   `json:"image_ref"`
	IdleTimeoutSeconds     int      `json:"idle_timeout_seconds"`
	EgressAllowlist        []string `json:"egress_allowlist"`
	VCPUs                  int      `json:"vcpus"`
	MemoryMiB              int      `json:"memory_mib"`
	MaxConcurrentInstances int      `json:"max_concurrent_instances"`
}

// ControllerClient is the REST API Service's only path to workload/instance
// data (§4.1's service boundary note). Interface so the HTTP handlers can
// be unit tested against a fake, without a real Controller present.
type ControllerClient interface {
	CreateWorkload(ctx context.Context, req CreateWorkloadRequest) (*Workload, error)
	GetWorkload(ctx context.Context, workloadID string) (*Workload, error)
	DeleteWorkload(ctx context.Context, workloadID string) error

	CreateInstance(ctx context.Context, workloadID string) (*InstanceResult, error)
	GetInstance(ctx context.Context, instanceID string) (*Instance, error)
	ResumeInstance(ctx context.Context, instanceID string) (*InstanceResult, error)
	Heartbeat(ctx context.Context, instanceID string) // fire-and-forget; errors are logged, not returned
	DeleteInstance(ctx context.Context, instanceID string) error
}

// HTTPControllerClient is the real implementation.
type HTTPControllerClient struct {
	baseURL string
	client  *http.Client
}

// timeout must be generous enough to cover a cold CreateInstance: the
// Controller may need to push a workload's golden rootfs to a Host Agent
// that's never seen it before (§4.6) before BootVM even starts, and a slow
// disk on the Host Agent's end (e.g. pd-standard) can make the volume-setup
// steps inside that legitimately take several seconds. A too-tight timeout
// here doesn't just fail this call — canceling it cancels the Host Agent's
// still-in-flight work too (its request context is tied to this
// connection), which is a worse failure than just waiting longer.
func NewHTTPControllerClient(baseURL string, timeout time.Duration) *HTTPControllerClient {
	return &HTTPControllerClient{baseURL: baseURL, client: &http.Client{Timeout: timeout}}
}

type controllerAPIError struct {
	status int
	body   map[string]string
}

func (e *controllerAPIError) Error() string {
	return fmt.Sprintf("controller returned %d: %v", e.status, e.body["error"])
}

func (c *HTTPControllerClient) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	var buf bytes.Buffer
	if reqBody != nil {
		if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, &buf)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("controller request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		apiErr := &controllerAPIError{status: resp.StatusCode, body: body}
		switch resp.StatusCode {
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusConflict:
			return ErrWorkloadNotReady
		case http.StatusTooManyRequests:
			return ErrAtCapacity
		default:
			return apiErr
		}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *HTTPControllerClient) CreateWorkload(ctx context.Context, req CreateWorkloadRequest) (*Workload, error) {
	var w Workload
	if err := c.doJSON(ctx, http.MethodPost, "/internal/workloads", req, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (c *HTTPControllerClient) GetWorkload(ctx context.Context, workloadID string) (*Workload, error) {
	var w Workload
	if err := c.doJSON(ctx, http.MethodGet, "/internal/workloads/"+workloadID, nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (c *HTTPControllerClient) DeleteWorkload(ctx context.Context, workloadID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/internal/workloads/"+workloadID, nil, nil)
}

func (c *HTTPControllerClient) CreateInstance(ctx context.Context, workloadID string) (*InstanceResult, error) {
	var res InstanceResult
	if err := c.doJSON(ctx, http.MethodPost, "/internal/workloads/"+workloadID+"/instances", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *HTTPControllerClient) GetInstance(ctx context.Context, instanceID string) (*Instance, error) {
	var inst Instance
	if err := c.doJSON(ctx, http.MethodGet, "/internal/instances/"+instanceID, nil, &inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

func (c *HTTPControllerClient) ResumeInstance(ctx context.Context, instanceID string) (*InstanceResult, error) {
	var res InstanceResult
	if err := c.doJSON(ctx, http.MethodPost, "/internal/instances/"+instanceID+"/resume", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Heartbeat is fire-and-forget by design (§4.2) — never blocks the caller,
// errors are worth knowing about but not worth failing the client's
// response over.
func (c *HTTPControllerClient) Heartbeat(ctx context.Context, instanceID string) {
	go func() {
		_ = c.doJSON(context.Background(), http.MethodPost, "/internal/instances/"+instanceID+"/heartbeat", nil, nil)
	}()
}

func (c *HTTPControllerClient) DeleteInstance(ctx context.Context, instanceID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/internal/instances/"+instanceID, nil, nil)
}
