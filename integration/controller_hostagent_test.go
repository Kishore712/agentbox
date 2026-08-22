// Package integration drives the actual HTTP boundary between independently
// built services to prove their wire contracts match — the second phase of
// the implementation plan (component unit tests -> integration -> full GCP
// validation). Real Redis, real HTTP servers for both the Controller and
// the Host Agent; only the pieces that genuinely require Linux/KVM/a real
// `firecracker` binary are stubbed (VM operations and the Firecracker API
// itself) — everything else in the request path is exercised for real.
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"containerised-agents/internal/common"
	"containerised-agents/internal/controller"
	"containerised-agents/internal/hostagent"
)

// --- Stubs for the Linux/KVM-only pieces ---

type stubImageBuilder struct{ rootfsRef string }

func (s *stubImageBuilder) Build(ctx context.Context, workloadID, imageRef string) (string, error) {
	return s.rootfsRef, nil
}

type stubHostOps struct {
	metadata map[string]hostagent.InstanceMetadata
}

func newStubHostOps() *stubHostOps {
	return &stubHostOps{metadata: map[string]hostagent.InstanceMetadata{}}
}

func (s *stubHostOps) PrepareKernel(ctx context.Context, goldenKernelPath, instanceID string) (string, error) {
	return goldenKernelPath, nil
}
func (s *stubHostOps) CopyRootfs(ctx context.Context, goldenRootfsPath, instanceID string) (string, error) {
	return "/data/instances/" + instanceID + "/rootfs.ext4", nil
}
func (s *stubHostOps) CreateHomeVolume(ctx context.Context, instanceID string) (string, error) {
	return "/data/instances/" + instanceID + "/home.ext4", nil
}
func (s *stubHostOps) SetupNetwork(ctx context.Context, instanceID string, egressAllowlist []string) (hostagent.NetworkInfo, error) {
	return hostagent.NetworkInfo{TapDevice: "tap-" + instanceID, GuestIP: "172.16.0.2", HostIP: "172.16.0.1"}, nil
}
func (s *stubHostOps) TeardownNetwork(ctx context.Context, instanceID string) error { return nil }
func (s *stubHostOps) StartFirecrackerProcess(ctx context.Context, instanceID string) (string, error) {
	return "/run/firecracker/" + instanceID + ".socket", nil
}
func (s *stubHostOps) StopFirecrackerProcess(ctx context.Context, instanceID string) error {
	return nil
}
func (s *stubHostOps) SnapshotPaths(instanceID string) (string, string) {
	return "/data/instances/" + instanceID + "/snapshot/vmstate", "/data/instances/" + instanceID + "/snapshot/mem_file"
}
func (s *stubHostOps) SocketPath(instanceID string) string {
	return "/run/firecracker/" + instanceID + ".socket"
}
func (s *stubHostOps) SaveInstanceMetadata(ctx context.Context, instanceID string, meta hostagent.InstanceMetadata) error {
	s.metadata[instanceID] = meta
	return nil
}
func (s *stubHostOps) LoadInstanceMetadata(ctx context.Context, instanceID string) (hostagent.InstanceMetadata, error) {
	meta, ok := s.metadata[instanceID]
	if !ok {
		return hostagent.InstanceMetadata{}, hostagent.ErrSnapshotMissing
	}
	return meta, nil
}
func (s *stubHostOps) DeleteInstanceFiles(ctx context.Context, instanceID string) error {
	delete(s.metadata, instanceID)
	return nil
}

type stubFirecrackerClient struct{}

func (stubFirecrackerClient) SetBootSource(ctx context.Context, kernelImagePath, bootArgs string) error {
	return nil
}
func (stubFirecrackerClient) SetDrive(ctx context.Context, driveID, pathOnHost string, isRootDevice, isReadOnly bool) error {
	return nil
}
func (stubFirecrackerClient) SetNetworkInterface(ctx context.Context, ifaceID, hostDevName string) error {
	return nil
}
func (stubFirecrackerClient) SetMachineConfig(ctx context.Context, vcpuCount, memSizeMiB int) error {
	return nil
}
func (stubFirecrackerClient) InstanceStart(ctx context.Context) error { return nil }
func (stubFirecrackerClient) Pause(ctx context.Context) error         { return nil }
func (stubFirecrackerClient) CreateSnapshot(ctx context.Context, snapshotPath, memFilePath string) error {
	return nil
}
func (stubFirecrackerClient) LoadSnapshot(ctx context.Context, snapshotPath, memFilePath string, resumeVM bool) error {
	return nil
}

type stubReadiness struct{}

func (stubReadiness) WaitReady(ctx context.Context, addr string, timeout time.Duration) error {
	return nil
}

// --- Test harness ---

type harness struct {
	t       *testing.T
	ctrlURL string
	store   *controller.Store
	ib      *stubImageBuilder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 14})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("local redis not reachable on localhost:6379: %v", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.FlushDB(ctx); rdb.Close() })

	// Host Agent: real HTTP server, stubbed VM operations.
	mgr := hostagent.NewVMManager(
		newStubHostOps(),
		func(string) hostagent.FirecrackerClient { return stubFirecrackerClient{} },
		stubReadiness{},
		hostagent.Config{KernelImagePath: "/data/vmlinux", GuestPort: 8080, BootTimeout: time.Second},
	)
	haServer := httptest.NewServer(hostagent.NewRouter(mgr))
	t.Cleanup(haServer.Close)
	haAddr := strings.TrimPrefix(haServer.URL, "http://")

	// Controller: real HTTP server, real Redis, real HTTPHostAgentClient
	// pointed at the Host Agent server above.
	store := controller.NewStore(rdb)
	tokens := controller.NewTokenIssuer([]byte("integration-test-secret"))
	ha := controller.NewHTTPHostAgentClient()
	ib := &stubImageBuilder{rootfsRef: "/data/workloads/integration/rootfs.ext4"}
	svc := controller.NewService(store, ha, tokens, ib)
	ctrlServer := httptest.NewServer(controller.NewRouter(svc))
	t.Cleanup(ctrlServer.Close)

	if err := store.UpsertHost(ctx, &common.Host{
		HostID: "host-1", InternalAddr: haAddr, Status: common.HostHealthy,
	}); err != nil {
		t.Fatal(err)
	}

	return &harness{t: t, ctrlURL: ctrlServer.URL, store: store, ib: ib}
}

func (h *harness) postJSON(path string, body any) map[string]any {
	h.t.Helper()
	return h.doJSON(http.MethodPost, path, body)
}

func (h *harness) getJSON(path string) map[string]any {
	h.t.Helper()
	return h.doJSON(http.MethodGet, path, nil)
}

func (h *harness) doJSON(method, path string, body any) map[string]any {
	h.t.Helper()
	var reqBody []byte
	if body != nil {
		var err error
		reqBody, err = json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, h.ctrlURL+path, strings.NewReader(string(reqBody)))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		h.t.Fatalf("%s %s: decode response: %v", method, path, err)
	}
	m["_status"] = float64(resp.StatusCode) // match how json.Decode types every other numeric field
	return m
}

func (h *harness) waitForWorkloadReady(workloadID string) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body := h.getJSON("/internal/workloads/" + workloadID)
		if body["status"] == "READY" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("workload %s never reached READY", workloadID)
}

// --- The integration test itself ---

func TestIntegration_FullInstanceLifecycle(t *testing.T) {
	h := newHarness(t)

	// 1. Register the workload (control plane only, no compute).
	created := h.postJSON("/internal/workloads", map[string]any{
		"name": "integration-agent", "image_ref": "example/image:tag",
		"idle_timeout_seconds": 300, "vcpus": 1, "memory_mib": 256,
		"max_concurrent_instances": 5,
	})
	if created["_status"] != float64(http.StatusCreated) {
		t.Fatalf("create workload: got status %v, body=%v", created["_status"], created)
	}
	workloadID, _ := created["workload_id"].(string)
	if workloadID == "" {
		t.Fatalf("expected workload_id in response: %v", created)
	}
	h.waitForWorkloadReady(workloadID)

	// 2. Create an instance — this is the real cross-service call: the
	// Controller's CreateInstance flow calls out over HTTP to the Host
	// Agent's POST /vm and gets back a real (stubbed) VM endpoint.
	inst := h.postJSON("/internal/workloads/"+workloadID+"/instances", nil)
	if inst["_status"] != float64(http.StatusCreated) {
		t.Fatalf("create instance: got status %v, body=%v", inst["_status"], inst)
	}
	if inst["state"] != "RUNNING" {
		t.Fatalf("state = %v, want RUNNING (error=%v)", inst["state"], inst["error"])
	}
	instanceID, _ := inst["instance_id"].(string)
	if instanceID == "" || inst["routing_token"] == "" || inst["guest_ip"] != "172.16.0.2" {
		t.Fatalf("unexpected create-instance response: %v", inst)
	}

	// 3. Suspend. §4.2: never called by the REST API Service in practice —
	// the Controller's internal idle-reaper loop calls Service.
	// SuspendInstance directly, in-process. The HTTP endpoint exists for
	// ops tooling and is what this test drives, but either path exercises
	// the same real HTTP call from Controller to the Host Agent's
	// POST /vm/{id}/suspend.
	suspendResp := h.postJSON("/internal/instances/"+instanceID+"/suspend", nil)
	if suspendResp["_status"] != float64(http.StatusAccepted) {
		t.Fatalf("suspend: got status %v, body=%v", suspendResp["_status"], suspendResp)
	}
	time.Sleep(50 * time.Millisecond) // suspend is async; give it a moment
	status := h.getJSON("/internal/instances/" + instanceID)
	if status["state"] != "SUSPENDED" {
		t.Fatalf("state after suspend = %v, want SUSPENDED", status["state"])
	}

	// 4. Resume — real HTTP call from the Controller to the Host Agent's
	// POST /vm/{id}/resume, exercising the metadata-driven re-setup path
	// (§4.3: resume takes no body, so egress allowlist must have been
	// persisted at boot time and reloaded here).
	resumed := h.postJSON("/internal/instances/"+instanceID+"/resume", nil)
	if resumed["_status"] != float64(http.StatusOK) {
		t.Fatalf("resume: got status %v, body=%v", resumed["_status"], resumed)
	}
	if resumed["state"] != "RUNNING" {
		t.Fatalf("state after resume = %v, want RUNNING (error=%v)", resumed["state"], resumed["error"])
	}

	// 5. Delete — fire-and-forget; poll until the record is actually gone.
	del := h.doJSON(http.MethodDelete, "/internal/instances/"+instanceID, nil)
	if del["_status"] != float64(http.StatusAccepted) {
		t.Fatalf("delete: got status %v, body=%v", del["_status"], del)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		final := h.getJSON("/internal/instances/" + instanceID)
		if final["_status"] == float64(http.StatusNotFound) {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("instance was never actually deleted")
}
