package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPImageBuilderClient is the real implementation of the ImageBuilder
// interface, talking REST/JSON to the standalone Image Builder service
// (cmd/imagebuilder) instead of calling Build() in-process — split out into
// its own systemd-managed service because the mount/loop-device privileges
// it needs turned out to be genuinely unsafe to grant a container sharing
// the host's /dev. Same internal-only trust model as HTTPHostAgentClient:
// reachable only from the Controller.
type HTTPImageBuilderClient struct {
	baseURL string
	client  *http.Client
}

// NewHTTPImageBuilderClient's timeout must cover a full build — image pull,
// filesystem export, ext4 format, extract, init injection — which can run
// from several seconds to over a minute depending on image size. Same
// reasoning as apiservice's ControllerTimeout: too short a timeout here
// doesn't just fail the caller, it cancels the still-in-flight build
// underneath it (context cancellation cascades through net/http and any
// exec.CommandContext children the build is running).
func NewHTTPImageBuilderClient(baseURL string, timeout time.Duration) *HTTPImageBuilderClient {
	return &HTTPImageBuilderClient{baseURL: baseURL, client: &http.Client{Timeout: timeout}}
}

func (c *HTTPImageBuilderClient) Build(ctx context.Context, workloadID, imageRef string) (string, error) {
	body, err := json.Marshal(buildRequest{WorkloadID: workloadID, ImageRef: imageRef})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/build", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("image builder request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return "", &httpStatusError{status: resp.StatusCode, body: errBody.Error}
	}
	var out buildResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return out.RootfsRef, nil
}

// buildRequest/buildResponse mirror internal/imagebuilder/httpapi.go's wire
// types exactly — duplicated rather than imported to keep the Controller
// package free of a dependency on the Image Builder's internals now that
// they're separate services, the same boundary as HostAgentClient.
type buildRequest struct {
	WorkloadID string `json:"workload_id"`
	ImageRef   string `json:"image_ref"`
}

type buildResponse struct {
	RootfsRef string `json:"rootfs_ref"`
}
