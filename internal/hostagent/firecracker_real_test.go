package hostagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRealFirecracker_BootSnapshotResume is the crown-jewel Tier 2 test:
// it automates exactly what this project's Phase 0 and Phase 1 validated
// manually, by hand, over SSH on a GCE VM — does a real Firecracker
// process actually boot a microVM on this host, and does snapshot/restore
// actually round-trip. Once this passes on a real box, that manual
// validation becomes a repeatable, CI-able test instead of a one-time
// exercise.
//
// Deliberately operates one level below VMManager, calling HostOps and
// FirecrackerClient directly — VMManager's own orchestration (step
// ordering, error handling) is already covered by manager_test.go's fakes;
// this file's job is proving the two real implementations underneath it
// actually work, which fakes can never do. It also skips attaching a home
// volume and skips VMManager's readiness-TCP-poll (Phase 0/1's test rootfs
// has no listening service, just a login prompt) — matching the original
// manual validation's scope exactly, not re-testing orchestration.
//
// Requires FC_TEST_KERNEL_PATH and FC_TEST_ROOTFS_PATH env vars pointing
// at a real vmlinux and rootfs.ext4 — see Firecracker's getting-started
// guide for how to obtain them (the same assets referenced in this
// project's own Phase 0 experiment). Not auto-downloaded here deliberately
// — Firecracker's asset URLs are known to shift between releases (noted
// during this project's own manual Phase 0 run), so a hardcoded URL here
// would be a maintenance trap; pointing at operator-provided local paths
// is more robust.
func requireFirecrackerEnvironment(t *testing.T) (kernelPath, rootfsPath string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("real Firecracker boot needs Linux + KVM — running on %s", runtime.GOOS)
	}
	kernelPath = os.Getenv("FC_TEST_KERNEL_PATH")
	rootfsPath = os.Getenv("FC_TEST_ROOTFS_PATH")
	if kernelPath == "" || rootfsPath == "" {
		t.Skip("FC_TEST_KERNEL_PATH and FC_TEST_ROOTFS_PATH must be set to a real vmlinux and rootfs.ext4 (see Firecracker's getting-started guide) — skipping")
	}
	if _, err := os.Stat(kernelPath); err != nil {
		t.Fatalf("FC_TEST_KERNEL_PATH %s: %v", kernelPath, err)
	}
	if _, err := os.Stat(rootfsPath); err != nil {
		t.Fatalf("FC_TEST_ROOTFS_PATH %s: %v", rootfsPath, err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present — nested virtualization not enabled on this host, skipping")
	}
	requireHostCommand(t, firecrackerBinaryName())
	requireHostCommand(t, "ip")
	requireHostCommand(t, "iptables")
	if os.Geteuid() != 0 {
		t.Skip("real Firecracker boot needs root (KVM access, TAP devices) — skipping")
	}
	return kernelPath, rootfsPath
}

func firecrackerBinaryName() string {
	if v := os.Getenv("FIRECRACKER_BINARY"); v != "" {
		return v
	}
	return "firecracker"
}

func TestRealFirecracker_BootSnapshotResume(t *testing.T) {
	kernelPath, goldenRootfs := requireFirecrackerEnvironment(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	subnets, err := NewSubnetAllocator("172.29.0.0/16", 4)
	if err != nil {
		t.Fatal(err)
	}
	ops := &LinuxHostOps{DataDir: dataDir, FirecrackerBinary: firecrackerBinaryName(), Subnets: subnets}
	instanceID := "real-fc-test-1"

	rootfsPath, err := ops.CopyRootfs(ctx, goldenRootfs, instanceID)
	if err != nil {
		t.Fatalf("CopyRootfs: %v", err)
	}
	net, err := ops.SetupNetwork(ctx, instanceID, nil)
	if err != nil {
		t.Fatalf("SetupNetwork: %v", err)
	}
	t.Cleanup(func() { _ = ops.TeardownNetwork(context.Background(), instanceID) })

	// --- Phase 0 equivalent: boot a real microVM ---
	sock, err := ops.StartFirecrackerProcess(ctx, instanceID)
	if err != nil {
		t.Fatalf("StartFirecrackerProcess: %v", err)
	}
	t.Cleanup(func() { _ = ops.StopFirecrackerProcess(context.Background(), instanceID) })
	waitForSocketFile(t, sock, 2*time.Second)

	fc := NewUnixSocketFirecrackerClient(sock)
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:255.255.255.252::eth0:off", net.GuestIP, net.HostIP)
	if err := fc.SetBootSource(ctx, kernelPath, bootArgs); err != nil {
		t.Fatalf("SetBootSource: %v", err)
	}
	if err := fc.SetDrive(ctx, "rootfs", rootfsPath, true, false); err != nil {
		t.Fatalf("SetDrive: %v", err)
	}
	if err := fc.SetNetworkInterface(ctx, "eth0", net.TapDevice); err != nil {
		t.Fatalf("SetNetworkInterface: %v", err)
	}
	if err := fc.SetMachineConfig(ctx, 1, 256); err != nil {
		t.Fatalf("SetMachineConfig: %v", err)
	}
	if err := fc.InstanceStart(ctx); err != nil {
		t.Fatalf("InstanceStart: %v", err)
	}
	t.Log("real microVM booted successfully")
	time.Sleep(500 * time.Millisecond) // let the guest actually finish booting before snapshotting

	// --- Phase 1 equivalent: snapshot, kill, restore in a fresh process ---
	snapshotPath, memFilePath := ops.SnapshotPaths(instanceID)
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fc.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := fc.CreateSnapshot(ctx, snapshotPath, memFilePath); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := ops.StopFirecrackerProcess(ctx, instanceID); err != nil {
		t.Fatalf("StopFirecrackerProcess: %v", err)
	}
	t.Log("snapshot created, original process stopped")

	sock2, err := ops.StartFirecrackerProcess(ctx, instanceID)
	if err != nil {
		t.Fatalf("StartFirecrackerProcess (resume): %v", err)
	}
	waitForSocketFile(t, sock2, 2*time.Second)
	fc2 := NewUnixSocketFirecrackerClient(sock2)
	if err := fc2.LoadSnapshot(ctx, snapshotPath, memFilePath, true); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	t.Log("snapshot loaded into a fresh process — resume round-trip succeeded")
}

func waitForSocketFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("firecracker API socket %s never appeared within %s", path, timeout)
}
