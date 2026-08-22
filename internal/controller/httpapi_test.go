package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"containerised-agents/internal/common"
)

func newTestRouter(t *testing.T, ha HostAgentClient, ib ImageBuilder) (*http.ServeMux, *Service) {
	t.Helper()
	svc := newTestService(t, ha, ib)
	return NewRouter(svc), svc
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestHTTP_CreateAndGetWorkload(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)

	rec := doJSON(t, mux, "POST", "/internal/workloads", createWorkloadRequest{
		Name: "my-agent", ImageRef: "example/image:tag", IdleTimeoutSeconds: 300,
		EgressAllowlist: []string{"api.openai.com"}, VCPUs: 2, MemoryMiB: 512, MaxConcurrentInstances: 10,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /internal/workloads: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	workloadID, _ := body["workload_id"].(string)
	if workloadID == "" {
		t.Fatalf("expected a workload_id in response, got %v", body)
	}
	if body["status"] != "PROVISIONING" {
		t.Errorf("status = %v, want PROVISIONING", body["status"])
	}

	<-ib.started
	waitForWorkloadStatus(t, svc, workloadID, common.WorkloadReady)

	rec = doJSON(t, mux, "GET", "/internal/workloads/"+workloadID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /internal/workloads/{id}: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body = decodeJSON(t, rec)
	if body["name"] != "my-agent" || body["status"] != "READY" {
		t.Errorf("got %v", body)
	}
	if _, present := body["rootfs_ref"]; present {
		t.Error("rootfs_ref must never be serialized in an API response (§4.1 fix: no leaking internal implementation details)")
	}
}

func TestHTTP_GetWorkload_NotFound(t *testing.T) {
	mux, _ := newTestRouter(t, newFakeHostAgent(), newFakeImageBuilder())
	rec := doJSON(t, mux, "GET", "/internal/workloads/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestHTTP_DeleteWorkload(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)

	rec := doJSON(t, mux, "DELETE", "/internal/workloads/"+w.WorkloadID, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["status"] != "deleting" {
		t.Errorf("got %v", body)
	}
}

func TestHTTP_CreateInstance_FullContract(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")

	rec := doJSON(t, mux, "POST", "/internal/workloads/"+w.WorkloadID+"/instances", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["state"] != "RUNNING" {
		t.Fatalf("state = %v, want RUNNING", body["state"])
	}
	for _, field := range []string{"instance_id", "host_id", "guest_ip", "guest_port", "routing_token", "token_exp"} {
		if _, ok := body[field]; !ok {
			t.Errorf("response missing field %q: %v", field, body)
		}
	}
}

func TestHTTP_CreateInstance_AtCapacityReturns429(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 1)
	registerHealthyHost(t, svc, "host-1")

	rec := doJSON(t, mux, "POST", "/internal/workloads/"+w.WorkloadID+"/instances", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("1st create: got %d", rec.Code)
	}
	rec = doJSON(t, mux, "POST", "/internal/workloads/"+w.WorkloadID+"/instances", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("2nd create over cap: got %d, want 429", rec.Code)
	}
}

func TestHTTP_CreateInstance_WorkloadNotReadyReturns409(t *testing.T) {
	ib := newFakeImageBuilder()
	ib.block = make(chan struct{}) // holds the build open so status stays PROVISIONING deterministically
	defer close(ib.block)
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	ctx := t.Context()
	w, err := svc.CreateWorkload(ctx, CreateWorkloadRequest{Name: "slow", ImageRef: "x", MaxConcurrentInstances: 10})
	if err != nil {
		t.Fatal(err)
	}
	<-ib.started // build has started and is now blocked on ib.block; status is guaranteed PROVISIONING

	rec := doJSON(t, mux, "POST", "/internal/workloads/"+w.WorkloadID+"/instances", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 (workload still PROVISIONING), body=%s", rec.Code, rec.Body.String())
	}
}

func TestHTTP_CreateInstance_UnknownWorkloadReturns404(t *testing.T) {
	mux, _ := newTestRouter(t, newFakeHostAgent(), newFakeImageBuilder())
	rec := doJSON(t, mux, "POST", "/internal/workloads/nonexistent/instances", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestHTTP_GetInstance(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	created, err := svc.CreateInstance(t.Context(), w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, mux, "GET", "/internal/instances/"+created.InstanceID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["state"] != "RUNNING" || body["workload_id"] != w.WorkloadID {
		t.Errorf("got %v", body)
	}
	// GetInstance must not carry routing info — that's the /access-equivalent
	// path (CreateInstance/ResumeInstance/heartbeat), not the status read.
	if _, present := body["routing_token"]; present {
		t.Error("GET instance status must not include a routing_token")
	}
}

func TestHTTP_ResumeInstance_Success(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	mux, svc := newTestRouter(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	created, err := svc.CreateInstance(t.Context(), w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SuspendInstance(t.Context(), created.InstanceID); err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, mux, "POST", "/internal/instances/"+created.InstanceID+"/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["state"] != "RUNNING" {
		t.Errorf("state = %v, want RUNNING", body["state"])
	}
}

func TestHTTP_Heartbeat(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	created, err := svc.CreateInstance(t.Context(), w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, mux, "POST", "/internal/instances/"+created.InstanceID+"/heartbeat", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
}

func TestHTTP_Heartbeat_UnknownInstance(t *testing.T) {
	mux, _ := newTestRouter(t, newFakeHostAgent(), newFakeImageBuilder())
	rec := doJSON(t, mux, "POST", "/internal/instances/nonexistent/heartbeat", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestHTTP_SuspendInstance(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	created, err := svc.CreateInstance(t.Context(), w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, mux, "POST", "/internal/instances/"+created.InstanceID+"/suspend", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
}

func TestHTTP_DeleteInstance(t *testing.T) {
	ib := newFakeImageBuilder()
	mux, svc := newTestRouter(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	created, err := svc.CreateInstance(t.Context(), w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, mux, "DELETE", "/internal/instances/"+created.InstanceID, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}
}
