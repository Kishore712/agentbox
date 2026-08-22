package controller

import (
	"testing"
	"time"

	"containerised-agents/internal/common"
)

func TestIdleReaper_Tick_SuspendsDueInstances(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	registerHealthyHost(t, svc, "host-1")
	ctx := t.Context()

	// idle_timeout_seconds = 0 on the workload means every instance is due
	// the moment TouchActivity runs (which CreateInstance already does).
	w, err := svc.CreateWorkload(ctx, CreateWorkloadRequest{
		Name: "idle-test", ImageRef: "example/image:tag",
		IdleTimeoutSeconds: 0, VCPUs: 1, MemoryMiB: 256, MaxConcurrentInstances: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-ib.started
	waitForWorkloadStatus(t, svc, w.WorkloadID, common.WorkloadReady)

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if created.State != common.InstanceRunning {
		t.Fatalf("state = %v, want RUNNING (error=%s)", created.State, created.Error)
	}

	// Give the ZSET score (based on time.Now()) a moment to fall behind "now".
	time.Sleep(1100 * time.Millisecond)

	reaper := NewIdleReaper(svc, time.Hour) // interval irrelevant, we drive Tick directly
	reaper.Tick(ctx)

	inst, err := svc.GetInstance(ctx, created.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != common.InstanceSuspended {
		t.Errorf("state after tick = %v, want SUSPENDED", inst.State)
	}
}

func TestIdleReaper_Tick_LeavesFreshInstancesAlone(t *testing.T) {
	ib := newFakeImageBuilder()
	ha := newFakeHostAgent()
	svc := newTestService(t, ha, ib)
	w := createReadyWorkload(t, svc, ib, 10) // default idle_timeout_seconds = 300
	registerHealthyHost(t, svc, "host-1")
	ctx := t.Context()

	created, err := svc.CreateInstance(ctx, w.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}

	reaper := NewIdleReaper(svc, time.Hour)
	reaper.Tick(ctx)

	inst, err := svc.GetInstance(ctx, created.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != common.InstanceRunning {
		t.Errorf("state = %v, want still RUNNING — idle_timeout_seconds=300 has not elapsed", inst.State)
	}
}
