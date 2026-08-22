package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"agentbox/internal/common"
)

// newTestStore connects to a local Redis (started via `brew services start
// redis` / `redis-server` on localhost:6379) and flushes a dedicated test DB
// before and after each test so tests never see each other's keys.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 15})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("local redis not reachable on localhost:6379 (start it with `brew services start redis`): %v", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush test db: %v", err)
	}
	t.Cleanup(func() {
		_ = rdb.FlushDB(ctx).Err()
		_ = rdb.Close()
	})
	return NewStore(rdb)
}

func TestWorkloadCreateGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	w := &common.Workload{
		WorkloadID:             "wl_test1",
		Name:                   "my-agent",
		ImageRef:               "us-docker.pkg.dev/proj/repo/image:tag",
		Status:                 common.WorkloadProvisioning,
		RootfsRef:              "/data/workloads/wl_test1/rootfs.ext4",
		IdleTimeoutSeconds:     300,
		EgressAllowlist:        []string{"api.openai.com", "pypi.org"},
		VCPUs:                  2,
		MemoryMiB:              512,
		MaxConcurrentInstances: 10,
		CreatedAt:              1724000000,
	}
	if err := s.CreateWorkload(ctx, w); err != nil {
		t.Fatalf("CreateWorkload: %v", err)
	}

	got, err := s.GetWorkload(ctx, "wl_test1")
	if err != nil {
		t.Fatalf("GetWorkload: %v", err)
	}
	if got.Name != w.Name || got.ImageRef != w.ImageRef || got.Status != w.Status {
		t.Errorf("got %+v, want fields matching %+v", got, w)
	}
	if len(got.EgressAllowlist) != 2 || got.EgressAllowlist[0] != "api.openai.com" {
		t.Errorf("egress_allowlist round-trip failed: %v", got.EgressAllowlist)
	}
	if got.MaxConcurrentInstances != 10 || got.VCPUs != 2 || got.MemoryMiB != 512 {
		t.Errorf("numeric field round-trip failed: %+v", got)
	}

	// Rootfs ref should never leak into JSON responses — verified at the
	// type level via the `json:"-"` tag, but sanity check it round-trips
	// internally since the Host Agent needs the real value.
	if got.RootfsRef != w.RootfsRef {
		t.Errorf("rootfs_ref = %q, want %q", got.RootfsRef, w.RootfsRef)
	}
}

func TestGetWorkloadNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetWorkload(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got err = %v, want ErrNotFound", err)
	}
}

func TestSetWorkloadStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	w := &common.Workload{WorkloadID: "wl_test2", Status: common.WorkloadProvisioning}
	if err := s.CreateWorkload(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkloadStatus(ctx, "wl_test2", common.WorkloadReady); err != nil {
		t.Fatalf("SetWorkloadStatus: %v", err)
	}
	got, err := s.GetWorkload(ctx, "wl_test2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != common.WorkloadReady {
		t.Errorf("status = %v, want READY", got.Status)
	}

	if err := s.SetWorkloadStatus(ctx, "nonexistent", common.WorkloadReady); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetWorkloadStatus on missing workload: got %v, want ErrNotFound", err)
	}
}

func TestDeleteWorkloadRemovesRecordAndSet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	w := &common.Workload{WorkloadID: "wl_test3", Status: common.WorkloadReady}
	if err := s.CreateWorkload(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveInstanceSlot(ctx, "wl_test3", "inst_a", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkload(ctx, "wl_test3"); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	if _, err := s.GetWorkload(ctx, "wl_test3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("workload record should be gone, got err=%v", err)
	}
	ids, err := s.WorkloadInstanceIDs(ctx, "wl_test3")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("workload_instances set should be empty after delete, got %v", ids)
	}
}

// --- Concurrency cap: the Lua-scripted atomic reserve ---

func TestReserveInstanceSlot_UnderCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ReserveInstanceSlot(ctx, "wl_cap", "inst_1", 2); err != nil {
		t.Fatalf("1st reserve: %v", err)
	}
	if err := s.ReserveInstanceSlot(ctx, "wl_cap", "inst_2", 2); err != nil {
		t.Fatalf("2nd reserve: %v", err)
	}
}

func TestReserveInstanceSlot_AtCapacity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ReserveInstanceSlot(ctx, "wl_cap2", "inst_1", 1); err != nil {
		t.Fatalf("1st reserve: %v", err)
	}
	err := s.ReserveInstanceSlot(ctx, "wl_cap2", "inst_2", 1)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("2nd reserve over cap: got %v, want ErrAtCapacity", err)
	}
	ids, _ := s.WorkloadInstanceIDs(ctx, "wl_cap2")
	if len(ids) != 1 {
		t.Errorf("rejected reservation should not have been added to the set, got %v", ids)
	}
}

// TestReserveInstanceSlot_ConcurrentRace fires N goroutines at a cap of 1 to
// verify the Lua script actually closes the check-then-act race rather than
// just narrowing it — this is the whole point of using EVAL instead of a
// plain SCARD-then-SADD from Go.
func TestReserveInstanceSlot_ConcurrentRace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const cap = 3
	const attempts = 30

	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			results <- s.ReserveInstanceSlot(ctx, "wl_race", instID(i), cap)
		}(i)
	}

	successes := 0
	for i := 0; i < attempts; i++ {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrAtCapacity) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != cap {
		t.Fatalf("successes = %d, want exactly %d (cap) — Lua script did not close the race", successes, cap)
	}
	ids, err := s.WorkloadInstanceIDs(ctx, "wl_race")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != cap {
		t.Fatalf("workload_instances set has %d members, want %d", len(ids), cap)
	}
}

func instID(i int) string {
	return "inst_race_" + string(rune('a'+i))
}

// --- Instance CRUD + CAS state machine ---

func TestPutGetInstance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{
		InstanceID: "wl_x:agent:abc123",
		WorkloadID: "wl_x",
		State:      common.InstanceCreating,
		HostID:     "host-vm-1",
		LastActive: 1724000000,
		GuestIP:    "172.16.0.2",
		GuestPort:  8080,
		CreatedAt:  1724000000,
	}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatalf("PutInstance: %v", err)
	}
	got, err := s.GetInstance(ctx, inst.InstanceID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.State != common.InstanceCreating || got.GuestPort != 8080 || got.HostID != "host-vm-1" {
		t.Errorf("got %+v, want fields matching %+v", got, inst)
	}
}

func TestGetInstanceNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetInstance(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCAS_Success(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_cas1", State: common.InstanceCreating}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}

	swapped, actual, err := s.CAS(ctx, "inst_cas1", common.InstanceCreating, common.InstanceRunning)
	if err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if !swapped {
		t.Fatalf("expected swap to succeed, actual state reported as %v", actual)
	}
	if actual != common.InstanceRunning {
		t.Errorf("actual = %v, want RUNNING", actual)
	}

	got, err := s.GetInstance(ctx, "inst_cas1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != common.InstanceRunning {
		t.Errorf("state in store = %v, want RUNNING", got.State)
	}
}

func TestCAS_WrongExpectedState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_cas2", State: common.InstanceRunning}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}

	// Try to CAS as if it were SUSPENDED -> RESUMING, but it's actually RUNNING.
	swapped, actual, err := s.CAS(ctx, "inst_cas2", common.InstanceSuspended, common.InstanceResuming)
	if err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if swapped {
		t.Fatalf("CAS should not have swapped — instance was RUNNING, not SUSPENDED")
	}
	if actual != common.InstanceRunning {
		t.Errorf("actual = %v, want RUNNING (the real current state)", actual)
	}

	got, err := s.GetInstance(ctx, "inst_cas2")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != common.InstanceRunning {
		t.Errorf("state should be untouched by a failed CAS, got %v", got.State)
	}
}

func TestCAS_NonexistentInstance(t *testing.T) {
	s := newTestStore(t)
	swapped, actual, err := s.CAS(context.Background(), "does-not-exist", common.InstanceRunning, common.InstanceSuspending)
	if err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if swapped {
		t.Fatal("CAS should not swap a nonexistent instance")
	}
	if actual != "" {
		t.Errorf("actual = %q, want empty string for a nonexistent instance", actual)
	}
}

// TestCAS_ConcurrentDoubleResume simulates the exact race the design doc
// calls out in §4.2: two concurrent invokes both see SUSPENDED and both try
// to CAS to RESUMING. Exactly one must win.
func TestCAS_ConcurrentDoubleResume(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_race_resume", State: common.InstanceSuspended}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}

	const attempts = 20
	results := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			swapped, _, err := s.CAS(ctx, "inst_race_resume", common.InstanceSuspended, common.InstanceResuming)
			if err != nil {
				t.Error(err)
			}
			results <- swapped
		}()
	}
	wins := 0
	for i := 0; i < attempts; i++ {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one concurrent CAS should win the SUSPENDED->RESUMING race, got %d winners", wins)
	}
}

func TestDeleteInstance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_del", WorkloadID: "wl_del", State: common.InstanceRunning}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveInstanceSlot(ctx, "wl_del", "inst_del", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchActivity(ctx, "inst_del", 300); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteInstance(ctx, "wl_del", "inst_del"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	if _, err := s.GetInstance(ctx, "inst_del"); !errors.Is(err, ErrNotFound) {
		t.Errorf("instance record should be gone, got err=%v", err)
	}
	ids, _ := s.WorkloadInstanceIDs(ctx, "wl_del")
	if len(ids) != 0 {
		t.Errorf("should be removed from workload_instances set, got %v", ids)
	}
	due, err := s.DueInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range due {
		if id == "inst_del" {
			t.Error("deleted instance should be removed from instances_due ZSET")
		}
	}
}

// --- Idle reaper: TouchActivity + DueInstances ---

func TestTouchActivity_MakesInstanceDueAfterTimeout(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_idle1", State: common.InstanceRunning}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}

	// idle_timeout_seconds = 0 means it's due immediately (score = now).
	if err := s.TouchActivity(ctx, "inst_idle1", 0); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}

	// Give the ZSET score (based on time.Now()) a moment to be <= a fresh "now".
	time.Sleep(1100 * time.Millisecond)

	due, err := s.DueInstances(ctx)
	if err != nil {
		t.Fatalf("DueInstances: %v", err)
	}
	found := false
	for _, id := range due {
		if id == "inst_idle1" {
			found = true
		}
	}
	if !found {
		t.Errorf("instance with idle_timeout=0 should be due, DueInstances returned %v", due)
	}
}

func TestTouchActivity_NotDueBeforeTimeout(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_idle2", State: common.InstanceRunning}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchActivity(ctx, "inst_idle2", 300); err != nil { // due in 5 minutes
		t.Fatal(err)
	}

	due, err := s.DueInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range due {
		if id == "inst_idle2" {
			t.Error("instance with a 300s idle timeout just touched should not be due yet")
		}
	}
}

func TestClearDue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	inst := &common.Instance{InstanceID: "inst_idle3", State: common.InstanceRunning}
	if err := s.PutInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchActivity(ctx, "inst_idle3", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearDue(ctx, "inst_idle3"); err != nil {
		t.Fatalf("ClearDue: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	due, err := s.DueInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range due {
		if id == "inst_idle3" {
			t.Error("instance should not be due after ClearDue (simulates a suspend)")
		}
	}
}

// --- Host registry ---

func TestHostUpsertGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	h := &common.Host{
		HostID:        "host-vm-1",
		InternalAddr:  "10.0.1.5:9000",
		Status:        common.HostHealthy,
		LastHeartbeat: 1724000000,
		CapacityUsed:  0,
	}
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}
	got, err := s.GetHost(ctx, "host-vm-1")
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if got.InternalAddr != h.InternalAddr || got.Status != common.HostHealthy {
		t.Errorf("got %+v, want fields matching %+v", got, h)
	}
}

func TestAdjustHostCapacity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	h := &common.Host{HostID: "host-vm-2", Status: common.HostHealthy, CapacityUsed: 0}
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := s.AdjustHostCapacity(ctx, "host-vm-2", 1); err != nil {
		t.Fatalf("AdjustHostCapacity +1: %v", err)
	}
	if err := s.AdjustHostCapacity(ctx, "host-vm-2", 1); err != nil {
		t.Fatalf("AdjustHostCapacity +1: %v", err)
	}
	got, err := s.GetHost(ctx, "host-vm-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.CapacityUsed != 2 {
		t.Errorf("capacity_used = %d, want 2", got.CapacityUsed)
	}
	if err := s.AdjustHostCapacity(ctx, "host-vm-2", -1); err != nil {
		t.Fatalf("AdjustHostCapacity -1: %v", err)
	}
	got, err = s.GetHost(ctx, "host-vm-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.CapacityUsed != 1 {
		t.Errorf("capacity_used = %d, want 1", got.CapacityUsed)
	}
}
