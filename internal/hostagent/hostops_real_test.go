package hostagent

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// Tier 2 of the testing pyramid (see internal/imagebuilder/builder_real_test.go
// for the full rationale): real `ip`/`iptables` against the real kernel
// networking stack, not fakes. Compiles everywhere, skips at test time
// wherever the environment can't support it.

func requireNetworkingEnvironment(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("real TAP/iptables operations need Linux — running on %s", runtime.GOOS)
	}
	requireHostCommand(t, "ip")
	requireHostCommand(t, "iptables")
	if os.Geteuid() != 0 {
		t.Skip("real TAP device / iptables operations need root (or CAP_NET_ADMIN) — skipping")
	}
}

func requireHostCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found in PATH — this test only runs on a properly provisioned Linux box, skipping here", name)
	}
}

// TestRealSetupTeardownNetwork_CreatesAndRemovesRealTapDevice verifies
// SetupNetwork/TeardownNetwork against the real kernel networking stack:
// the TAP device genuinely exists after setup (visible via `ip link show`)
// and is genuinely gone after teardown — not just "the function returned
// no error," which fakes already cover.
func TestRealSetupTeardownNetwork_CreatesAndRemovesRealTapDevice(t *testing.T) {
	requireNetworkingEnvironment(t)
	ctx := context.Background()

	subnets, err := NewSubnetAllocator("172.30.0.0/16", 4)
	if err != nil {
		t.Fatal(err)
	}
	ops := &LinuxHostOps{Subnets: subnets, SquidPort: DefaultSquidPort} // Squid nil: no ACL applied, fine for this test

	instanceID := "real-net-test-1"
	net, err := ops.SetupNetwork(ctx, instanceID, []string{"example.com"})
	if err != nil {
		t.Fatalf("SetupNetwork: %v", err)
	}
	t.Cleanup(func() { _ = ops.TeardownNetwork(context.Background(), instanceID) })

	if !tapDeviceExists(t, net.TapDevice) {
		t.Fatalf("TAP device %s does not actually exist after SetupNetwork", net.TapDevice)
	}
	if !tapHasAddress(t, net.TapDevice, net.HostIP) {
		t.Errorf("TAP device %s does not have the expected address %s", net.TapDevice, net.HostIP)
	}

	if err := ops.TeardownNetwork(ctx, instanceID); err != nil {
		t.Fatalf("TeardownNetwork: %v", err)
	}
	if tapDeviceExists(t, net.TapDevice) {
		t.Errorf("TAP device %s still exists after TeardownNetwork", net.TapDevice)
	}
}

// TestRealSetupNetwork_DistinctInstancesGetDistinctRealTapDevices boots two
// concurrent "instances" worth of networking and confirms they don't
// collide on the real host — the actual point of the SubnetAllocator,
// verified against the real kernel instead of an in-memory map.
func TestRealSetupNetwork_DistinctInstancesGetDistinctRealTapDevices(t *testing.T) {
	requireNetworkingEnvironment(t)
	ctx := context.Background()

	subnets, err := NewSubnetAllocator("172.31.0.0/16", 4)
	if err != nil {
		t.Fatal(err)
	}
	ops := &LinuxHostOps{Subnets: subnets, SquidPort: DefaultSquidPort}

	net1, err := ops.SetupNetwork(ctx, "real-net-test-a", nil)
	if err != nil {
		t.Fatalf("SetupNetwork (a): %v", err)
	}
	t.Cleanup(func() { _ = ops.TeardownNetwork(context.Background(), "real-net-test-a") })

	net2, err := ops.SetupNetwork(ctx, "real-net-test-b", nil)
	if err != nil {
		t.Fatalf("SetupNetwork (b): %v", err)
	}
	t.Cleanup(func() { _ = ops.TeardownNetwork(context.Background(), "real-net-test-b") })

	if net1.HostIP == net2.HostIP || net1.GuestIP == net2.GuestIP {
		t.Fatalf("two concurrent instances got colliding real subnets: %+v vs %+v", net1, net2)
	}
	if !tapDeviceExists(t, net1.TapDevice) || !tapDeviceExists(t, net2.TapDevice) {
		t.Fatal("both TAP devices should exist simultaneously")
	}
}

func tapDeviceExists(t *testing.T, name string) bool {
	t.Helper()
	err := exec.Command("ip", "link", "show", name).Run()
	return err == nil
}

func tapHasAddress(t *testing.T, name, addr string) bool {
	t.Helper()
	out, err := exec.Command("ip", "addr", "show", name).CombinedOutput()
	if err != nil {
		t.Fatalf("ip addr show %s: %v", name, err)
	}
	return strings.Contains(string(out), addr)
}
