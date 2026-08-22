package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"containerised-agents/internal/common"
)

// --- Fakes ---

type fakeHostAgent struct {
	mu             sync.Mutex
	bootErr        error
	suspendErr     error
	resumeErr      error
	deleteErr      error
	deleteFailures int // number of times DeleteVM should fail before succeeding
	deleteCalls    int
	nextGuestIP    string
	nextGuestPort  int
}

func newFakeHostAgent() *fakeHostAgent {
	return &fakeHostAgent{nextGuestIP: "172.16.0.2", nextGuestPort: 8080}
}

func (f *fakeHostAgent) BootVM(ctx context.Context, hostAddr string, req BootVMRequest) (VMEndpoint, error) {
	if f.bootErr != nil {
		return VMEndpoint{}, f.bootErr
	}
	return VMEndpoint{GuestIP: f.nextGuestIP, GuestPort: f.nextGuestPort}, nil
}

func (f *fakeHostAgent) SuspendVM(ctx context.Context, hostAddr, instanceID string) error {
	return f.suspendErr
}

func (f *fakeHostAgent) ResumeVM(ctx context.Context, hostAddr, instanceID string) (VMEndpoint, error) {
	if f.resumeErr != nil {
		return VMEndpoint{}, f.resumeErr
	}
	return VMEndpoint{GuestIP: f.nextGuestIP, GuestPort: f.nextGuestPort}, nil
}

func (f *fakeHostAgent) DeleteVM(ctx context.Context, hostAddr, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.deleteCalls <= f.deleteFailures {
		return errors.New("simulated host agent unreachable")
	}
	return f.deleteErr
}

type fakeImageBuilder struct {
	err       error
	rootfsRef string
	started   chan struct{}
	block     chan struct{} // if non-nil, Build waits for a send on this before returning
}

func newFakeImageBuilder() *fakeImageBuilder {
	return &fakeImageBuilder{rootfsRef: "/data/workloads/fake/rootfs.ext4", started: make(chan struct{}, 8)}
}

func (f *fakeImageBuilder) Build(ctx context.Context, workloadID, imageRef string) (string, error) {
	f.started <- struct{}{}
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return "", f.err
	}
	return f.rootfsRef, nil
}

// --- Test setup ---

func newTestService(t *testing.T, ha HostAgentClient, ib ImageBuilder) *Service {
	t.Helper()
	store := newTestStore(t) // real Redis, per store_test.go
	tokens := NewTokenIssuer([]byte("test-secret"))
	return NewService(store, ha, tokens, ib)
}

// createReadyWorkload registers a workload and blocks until its (fake)
// build completes and it reaches READY — avoids sleep-based flakiness in
// every test that needs a bootable workload.
func createReadyWorkload(t *testing.T, svc *Service, ib *fakeImageBuilder, maxConcurrent int) *common.Workload {
	t.Helper()
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, CreateWorkloadRequest{
		Name: "test-agent", ImageRef: "example/image:tag",
		IdleTimeoutSeconds: 300, VCPUs: 1, MemoryMiB: 256,
		MaxConcurrentInstances: maxConcurrent,
	})
	if err != nil {
		t.Fatalf("CreateWorkload: %v", err)
	}
	select {
	case <-ib.started:
	case <-time.After(2 * time.Second):
		t.Fatal("image build never started")
	}
	waitForWorkloadStatus(t, svc, w.WorkloadID, common.WorkloadReady)
	got, err := svc.GetWorkload(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func waitForWorkloadStatus(t *testing.T, svc *Service, workloadID string, want common.WorkloadStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w, err := svc.GetWorkload(context.Background(), workloadID)
		if err != nil {
			t.Fatal(err)
		}
		if w.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workload %s never reached status %s", workloadID, want)
}

func registerHealthyHost(t *testing.T, svc *Service, hostID string) {
	t.Helper()
	err := svc.store.UpsertHost(context.Background(), &common.Host{
		HostID: hostID, InternalAddr: hostID + ":9000", Status: common.HostHealthy,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- CreateWorkload ---

func TestCreateWorkload_ReachesReady(t *testing.T) {
	ib := newFakeImageBuilder()
	svc := newTestService(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)
	if w.Status != common.WorkloadReady {
		t.Errorf("status = %v, want READY", w.Status)
	}
	if w.RootfsRef != ib.rootfsRef {
		t.Errorf("rootfs_ref = %q, want %q", w.RootfsRef, ib.rootfsRef)
	}
}

func TestCreateWorkload_BuildFailure(t *testing.T) {
	ib := newFakeImageBuilder()
	ib.err = errors.New("docker pull failed")
	svc := newTestService(t, newFakeHostAgent(), ib)
	ctx := context.Background()

	w, err := svc.CreateWorkload(ctx, CreateWorkloadRequest{Name: "bad", ImageRef: "nope"})
	if err != nil {
		t.Fatalf("CreateWorkload should not itself fail: %v", err)
	}
	<-ib.started
	waitForWorkloadStatus(t, svc, w.WorkloadID, common.WorkloadFailed)
}

// --- CreateInstance ---

func TestCreateInstance_Success(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")

	res, err := svc.CreateInstance(context.Background(), w.WorkloadID)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if res.State != common.InstanceRunning {
		t.Fatalf("state = %v, want RUNNING (error=%s)", res.State, res.Error)
	}
	if res.GuestIP != "172.16.0.2" || res.GuestPort != 8080 {
		t.Errorf("endpoint = %s:%d, want 172.16.0.2:8080", res.GuestIP, res.GuestPort)
	}
	if res.RoutingToken == "" {
		t.Error("expected a non-empty routing token")
	}
	claims, err := svc.tokens.Verify(res.RoutingToken)
	if err != nil {
		t.Fatalf("issued token did not verify: %v", err)
	}
	if claims.InstanceID != res.InstanceID || claims.GuestIP != res.GuestIP {
		t.Errorf("token claims %+v don't match result %+v", claims, res)
	}

	host, err := svc.store.GetHost(context.Background(), "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if host.CapacityUsed != 1 {
		t.Errorf("host capacity_used = %d, want 1 after one running instance", host.CapacityUsed)
	}
}

func TestCreateInstance_WorkloadNotReady(t *testing.T) {
	ib := newFakeImageBuilder()
	ib.block = make(chan struct{}) // holds the build open so status stays PROVISIONING deterministically
	defer close(ib.block)
	svc := newTestService(t, newFakeHostAgent(), ib)
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, CreateWorkloadRequest{Name: "slow", ImageRef: "x"})
	if err != nil {
		t.Fatal(err)
	}
	<-ib.started // build has started and is now blocked; status is guaranteed PROVISIONING

	_, err = svc.CreateInstance(ctx, w.WorkloadID)
	if !errors.Is(err, ErrWorkloadNotReady) {
		t.Fatalf("got %v, want ErrWorkloadNotReady", err)
	}
}

func TestCreateInstance_UnknownWorkload(t *testing.T) {
	svc := newTestService(t, newFakeHostAgent(), newFakeImageBuilder())
	_, err := svc.CreateInstance(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCreateInstance_AtCapacity(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 1)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	if _, err := svc.CreateInstance(ctx, w.WorkloadID); err != nil {
		t.Fatalf("1st CreateInstance: %v", err)
	}
	_, err := svc.CreateInstance(ctx, w.WorkloadID)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("2nd CreateInstance over cap: got %v, want ErrAtCapacity", err)
	}
}

func TestCreateInstance_NoHealthyHost(t *testing.T) {
	ib := newFakeImageBuilder()
	svc := newTestService(t, newFakeHostAgent(), ib)
	w := createReadyWorkload(t, svc, ib, 10)
	// No host registered at all.

	res, err := svc.CreateInstance(context.Background(), w.WorkloadID)
	if err != nil {
		t.Fatalf("CreateInstance should return a FAILED result, not an error: %v", err)
	}
	if res.State != common.InstanceFailed {
		t.Errorf("state = %v, want FAILED", res.State)
	}
	if res.Error == "" {
		t.Error("expected a non-empty error reason")
	}
}

func TestCreateInstance_BootFailure(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	ha.bootErr = errors.New("firecracker boot timeout")
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")

	res, err := svc.CreateInstance(context.Background(), w.WorkloadID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.State != common.InstanceFailed {
		t.Errorf("state = %v, want FAILED", res.State)
	}

	inst, err := svc.GetInstance(context.Background(), res.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != common.InstanceFailed {
		t.Errorf("stored state = %v, want FAILED — record should be left for inspection, not vanished", inst.State)
	}
}

// --- SuspendInstance ---

func TestSuspendInstance_Success(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	res, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.SuspendInstance(ctx, res.InstanceID); err != nil {
		t.Fatalf("SuspendInstance: %v", err)
	}
	inst, err := svc.GetInstance(ctx, res.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != common.InstanceSuspended {
		t.Errorf("state = %v, want SUSPENDED", inst.State)
	}
	host, err := svc.store.GetHost(ctx, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if host.CapacityUsed != 0 {
		t.Errorf("host capacity_used = %d, want 0 after suspend", host.CapacityUsed)
	}
}

func TestSuspendInstance_NoOpIfNotRunning(t *testing.T) {
	svc := newTestService(t, newFakeHostAgent(), newFakeImageBuilder())
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_not_running", State: common.InstanceSuspended}
	if err := svc.store.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := svc.SuspendInstance(ctx, "inst_not_running"); err != nil {
		t.Fatalf("SuspendInstance on an already-SUSPENDED instance should no-op, got err: %v", err)
	}
}

func TestSuspendInstance_HostAgentFailureReverts(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	res, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	ha.suspendErr = errors.New("host agent unreachable")
	if err := svc.SuspendInstance(ctx, res.InstanceID); err == nil {
		t.Fatal("expected SuspendInstance to return the host agent error")
	}
	inst, err := svc.GetInstance(ctx, res.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != common.InstanceRunning {
		t.Errorf("state = %v, want reverted to RUNNING (not FAILED) after a suspend failure", inst.State)
	}
}

// --- ResumeInstance ---

func TestResumeInstance_Success(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SuspendInstance(ctx, created.InstanceID); err != nil {
		t.Fatal(err)
	}

	ha.nextGuestIP = "172.16.0.9" // simulate a fresh IP assignment on resume
	res, err := svc.ResumeInstance(ctx, created.InstanceID)
	if err != nil {
		t.Fatalf("ResumeInstance: %v", err)
	}
	if res.State != common.InstanceRunning {
		t.Fatalf("state = %v, want RUNNING (error=%s)", res.State, res.Error)
	}
	if res.GuestIP != "172.16.0.9" {
		t.Errorf("guest_ip = %s, want the refreshed 172.16.0.9", res.GuestIP)
	}
	host, err := svc.store.GetHost(ctx, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if host.CapacityUsed != 1 {
		t.Errorf("host capacity_used = %d, want 1 after resume", host.CapacityUsed)
	}
}

func TestResumeInstance_IdempotentWhenAlreadyRunning(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	// Instance is already RUNNING — resume should be a safe no-op that just
	// returns fresh routing info, per §4.2.
	res, err := svc.ResumeInstance(ctx, created.InstanceID)
	if err != nil {
		t.Fatalf("ResumeInstance on a RUNNING instance should not error: %v", err)
	}
	if res.State != common.InstanceRunning {
		t.Errorf("state = %v, want RUNNING", res.State)
	}
	if res.RoutingToken == "" {
		t.Error("expected a fresh routing token even on the idempotent path")
	}
}

func TestResumeInstance_UnknownInstance(t *testing.T) {
	svc := newTestService(t, newFakeHostAgent(), newFakeImageBuilder())
	_, err := svc.ResumeInstance(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestResumeInstance_SnapshotMissingFails(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SuspendInstance(ctx, created.InstanceID); err != nil {
		t.Fatal(err)
	}

	ha.resumeErr = ErrSnapshotMissing
	res, err := svc.ResumeInstance(ctx, created.InstanceID)
	if err != nil {
		t.Fatalf("unexpected error (should return a FAILED result): %v", err)
	}
	if res.State != common.InstanceFailed {
		t.Errorf("state = %v, want FAILED for a missing snapshot", res.State)
	}
}

func TestResumeInstance_TransientFailureRevertsToSuspended(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SuspendInstance(ctx, created.InstanceID); err != nil {
		t.Fatal(err)
	}

	ha.resumeErr = errors.New("network blip")
	if _, err := svc.ResumeInstance(ctx, created.InstanceID); err == nil {
		t.Fatal("expected ResumeInstance to surface the transient error")
	}
	inst, err := svc.GetInstance(ctx, created.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != common.InstanceSuspended {
		t.Errorf("state = %v, want reverted to SUSPENDED (not FAILED) after a transient resume failure — the snapshot is still on disk", inst.State)
	}
}

// --- DeleteInstance ---

func TestDeleteInstance_RemovesRecordAndAdjustsCapacity(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.runDeleteInstance(ctx, created.InstanceID); err != nil {
		t.Fatalf("runDeleteInstance: %v", err)
	}
	if _, err := svc.GetInstance(ctx, created.InstanceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("instance should be gone, got err=%v", err)
	}
	host, err := svc.store.GetHost(ctx, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if host.CapacityUsed != 0 {
		t.Errorf("host capacity_used = %d, want 0 after deleting a RUNNING instance", host.CapacityUsed)
	}
}

func TestDeleteInstance_RetriesOnTransientHostAgentFailure(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	ha.deleteFailures = 2 // fails twice, succeeds on the 3rd (final) attempt
	if err := svc.runDeleteInstance(ctx, created.InstanceID); err != nil {
		t.Fatalf("runDeleteInstance should succeed after retries: %v", err)
	}
	if _, err := svc.GetInstance(ctx, created.InstanceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("instance should be gone after retries succeed, got err=%v", err)
	}
}

func TestDeleteInstance_LeavesRecordOnPersistentFailure(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10)
	registerHealthyHost(t, svc, "host-1")
	ctx := context.Background()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	ha.deleteFailures = 999 // always fails
	if err := svc.runDeleteInstance(ctx, created.InstanceID); err == nil {
		t.Fatal("expected a persistent failure error")
	}
	inst, err := svc.GetInstance(ctx, created.InstanceID)
	if err != nil {
		t.Fatalf("instance record should still exist for manual cleanup: %v", err)
	}
	if inst.State != common.InstanceDeleting {
		t.Errorf("state = %v, want DELETING (left in place per §4.2)", inst.State)
	}
}

// --- Placement ---

func TestPickHealthyHost_RoundRobinsAndSkipsUnhealthy(t *testing.T) {
	svc := newTestService(t, newFakeHostAgent(), newFakeImageBuilder())
	ctx := context.Background()
	registerHealthyHost(t, svc, "host-a")
	registerHealthyHost(t, svc, "host-b")
	if err := svc.store.UpsertHost(ctx, &common.Host{HostID: "host-c", Status: common.HostUnhealthy}); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		h, err := svc.pickHealthyHost(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen[h.HostID]++
	}
	if seen["host-c"] != 0 {
		t.Errorf("unhealthy host-c should never be picked, got %d picks", seen["host-c"])
	}
	if seen["host-a"] == 0 || seen["host-b"] == 0 {
		t.Errorf("expected round-robin to spread across both healthy hosts, got %v", seen)
	}
}

func TestPickHealthyHost_NoneAvailable(t *testing.T) {
	svc := newTestService(t, newFakeHostAgent(), newFakeImageBuilder())
	_, err := svc.pickHealthyHost(context.Background())
	if !errors.Is(err, ErrNoHealthyHost) {
		t.Fatalf("got %v, want ErrNoHealthyHost", err)
	}
}
