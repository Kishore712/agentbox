package apiservice

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testAPIKey = "test-key-123"

func newTestRouter(ctrl ControllerClient, proxy HostAgentProxy) http.Handler {
	svc := NewService(ctrl, proxy, 0)
	return NewRouter(svc, testAPIKey)
}

func doReq(t *testing.T, h http.Handler, method, path, apiKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTP_Auth_MissingKeyRejected(t *testing.T) {
	h := newTestRouter(newFakeControllerClient(), &fakeHostAgentProxy{})
	rec := doReq(t, h, "GET", "/agents/agt_1", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestHTTP_Auth_WrongKeyRejected(t *testing.T) {
	h := newTestRouter(newFakeControllerClient(), &fakeHostAgentProxy{})
	rec := doReq(t, h, "GET", "/agents/agt_1", "wrong-key", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestHTTP_CreateAndGetAgent(t *testing.T) {
	ctrl := newFakeControllerClient()
	h := newTestRouter(ctrl, &fakeHostAgentProxy{})

	rec := doReq(t, h, "POST", "/agents", testAPIKey, createAgentRequest{
		AgentName: "my-agent", ImageRef: "example/x:tag", IdleTimeoutSeconds: 300,
		VCPUs: 1, MemoryMiB: 256, MaxConcurrentInstances: 10,
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	agentID, _ := created["agent_id"].(string)
	if agentID == "" {
		t.Fatalf("expected agent_id in response: %v", created)
	}

	rec = doReq(t, h, "GET", "/agents/"+agentID, testAPIKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["agent_name"] != "my-agent" {
		t.Errorf("got %v", got)
	}
}

func TestHTTP_GetAgent_NotFound(t *testing.T) {
	h := newTestRouter(newFakeControllerClient(), &fakeHostAgentProxy{})
	rec := doReq(t, h, "GET", "/agents/nonexistent", testAPIKey, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestHTTP_DeleteAgent(t *testing.T) {
	h := newTestRouter(newFakeControllerClient(), &fakeHostAgentProxy{})
	rec := doReq(t, h, "DELETE", "/agents/agt_1", testAPIKey, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
}

func TestHTTP_CreateSession(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.5:9000"}
	h := newTestRouter(ctrl, &fakeHostAgentProxy{})

	rec := doReq(t, h, "POST", "/agents/agt_1/sessions", testAPIKey, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["session_id"] != "inst-1" || body["state"] != "RUNNING" {
		t.Errorf("got %v", body)
	}
	// The explicit-create response is a status envelope, not a routing
	// grant — no routing_token should leak into it (that's only ever
	// delivered via invocation response headers).
	if _, present := body["routing_token"]; present {
		t.Error("POST /agents/{id}/sessions must not include routing_token")
	}
}

func TestHTTP_GetSession(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.instances["inst-1"] = &Instance{InstanceID: "inst-1", WorkloadID: "agt_1", State: "RUNNING", HostID: "host-1", LastActive: 123}
	h := newTestRouter(ctrl, &fakeHostAgentProxy{})

	rec := doReq(t, h, "GET", "/agents/agt_1/sessions/inst-1", testAPIKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["session_id"] != "inst-1" || body["agent_id"] != "agt_1" {
		t.Errorf("got %v", body)
	}
}

func TestHTTP_DeleteSession(t *testing.T) {
	h := newTestRouter(newFakeControllerClient(), &fakeHostAgentProxy{})
	rec := doReq(t, h, "DELETE", "/agents/agt_1/sessions/inst-1", testAPIKey, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
}

func TestHTTP_Invoke_ImplicitCreate(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.createInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.5:9000"}
	proxy := &fakeHostAgentProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("app response")}}
	h := newTestRouter(ctrl, proxy)

	rec := doReq(t, h, "POST", "/agents/agt_1/invocation", testAPIKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "app response" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("X-Session-Id") != "inst-1" {
		t.Errorf("X-Session-Id = %q, want inst-1", rec.Header().Get("X-Session-Id"))
	}
	if _, present := rec.Header()["X-Routing-Token"]; present {
		t.Error("no routing token should ever be sent to the client — see the design doc's 3.1→3.2 changelog")
	}
}

func TestHTTP_Invoke_GETWithoutSessionID(t *testing.T) {
	h := newTestRouter(newFakeControllerClient(), &fakeHostAgentProxy{})
	rec := doReq(t, h, "GET", "/agents/agt_1/invocation", testAPIKey, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestHTTP_Invoke_SuspendedSessionNeverSurfacesAsErrorToClient(t *testing.T) {
	// The whole point of the resume-fallback design: a client hitting a
	// suspended session sees ONE (slower) successful response, never a
	// 503 — this test is the end-to-end proof of that HTTP-layer contract.
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateRunning, HostID: "host-1", HostAgentAddr: "10.0.1.9:9000"}
	proxy := &fakeHostAgentProxy{response: &ProxyResponse{StatusCode: 200, Header: http.Header{}, Body: []byte("woke up")}}
	h := newTestRouter(ctrl, proxy)

	req := httptest.NewRequest("POST", "/agents/agt_1/invocation?session_id=inst-1", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	// Fresh Service, so the routing cache is empty — simulates a client
	// returning after the session was suspended (cache miss forces a
	// Controller.ResumeInstance round trip).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("client must see a normal 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "woke up" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestHTTP_Invoke_SessionFailedReturns409(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceRes = &InstanceResult{InstanceID: "inst-1", State: StateFailed, Error: "unrecoverable"}
	h := newTestRouter(ctrl, &fakeHostAgentProxy{})

	rec := doReq(t, h, "POST", "/agents/agt_1/invocation?session_id=inst-1", testAPIKey, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", rec.Code)
	}
}

func TestHTTP_Invoke_UnknownSessionReturns404(t *testing.T) {
	ctrl := newFakeControllerClient()
	ctrl.resumeInstanceErr = ErrNotFound
	h := newTestRouter(ctrl, &fakeHostAgentProxy{})

	rec := doReq(t, h, "POST", "/agents/agt_1/invocation?session_id=nonexistent", testAPIKey, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}
