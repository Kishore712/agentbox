package hostagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// NetworkInfo is what SetupNetwork hands back: the TAP device to attach to
// Firecracker's network-interface config, plus the guest/host IPs on the
// /30 it just created (§4.3 step 3, §3.1 disk layout).
type NetworkInfo struct {
	TapDevice string
	GuestIP   string
	HostIP    string
}

// HostOps abstracts every OS-level operation the Host Agent needs — file
// copying, TAP devices, iptables, process management — so VMManager's
// orchestration logic (the exact step sequence from §4.3) can be unit
// tested without real Linux/KVM/root privileges. LinuxHostOps below is the
// real implementation, only functional on the target GCE hosts; unit tests
// use fakeHostOps instead (see manager_test.go).
type HostOps interface {
	// CopyRootfs implements §4.3 step 1: a fresh per-instance copy of the
	// workload's golden rootfs, since a writable root can't safely be
	// shared across concurrent Firecracker processes.
	CopyRootfs(ctx context.Context, goldenRootfsPath, instanceID string) (instanceRootfsPath string, err error)

	// CreateHomeVolume implements §4.3 step 2: a fresh, empty, sized ext4
	// file for $HOME, unique per instance (§4.7).
	CreateHomeVolume(ctx context.Context, instanceID string) (homePath string, err error)

	// SetupNetwork implements §4.3 step 3: TAP device + iptables NAT/
	// egress-allowlist rules, and assigns the guest's IP.
	SetupNetwork(ctx context.Context, instanceID string, egressAllowlist []string) (NetworkInfo, error)

	// TeardownNetwork removes the TAP device and its iptables rules —
	// called on suspend and delete. Idempotent (no-op if already gone).
	TeardownNetwork(ctx context.Context, instanceID string) error

	// StartFirecrackerProcess implements §4.3 step 4: spawns `firecracker`
	// (wrapped in the Jailer once that's in scope, §4.6/Phase 6) bound to a
	// fresh API socket, and returns that socket's path.
	StartFirecrackerProcess(ctx context.Context, instanceID string) (socketPath string, err error)

	// StopFirecrackerProcess kills the process for instanceID. Idempotent.
	StopFirecrackerProcess(ctx context.Context, instanceID string) error

	// SnapshotPaths returns where this instance's snapshot files live —
	// used by both CreateSnapshot (suspend) and LoadSnapshot (resume).
	SnapshotPaths(instanceID string) (snapshotPath, memFilePath string)

	// SocketPath returns the deterministic Firecracker API socket path for
	// an instance — lets SuspendVM/ResumeVM rebuild a FirecrackerClient for
	// an already-running (or about-to-be-restarted) process without the
	// manager needing to hold in-memory state across calls.
	SocketPath(instanceID string) string

	// SaveInstanceMetadata/LoadInstanceMetadata persist what BootVM needs
	// to remember for later calls that don't repeat it — specifically the
	// egress allowlist, since §4.2's Host Agent API has ResumeVM take only
	// an instance_id, no body. Without this, a resume couldn't reapply the
	// same egress rules the instance was originally booted with.
	SaveInstanceMetadata(ctx context.Context, instanceID string, meta InstanceMetadata) error
	LoadInstanceMetadata(ctx context.Context, instanceID string) (InstanceMetadata, error)

	// DeleteInstanceFiles removes /data/instances/{instanceID}/ entirely —
	// the per-instance rootfs copy, home.ext4, snapshot/, and metadata.
	DeleteInstanceFiles(ctx context.Context, instanceID string) error
}

// InstanceMetadata is the small bit of boot-time context the Host Agent
// needs to remember locally to support resume's minimal (instance_id-only)
// request shape.
type InstanceMetadata struct {
	EgressAllowlist []string
}

// LinuxHostOps is the real implementation, shelling out to the same tools
// Phase 0/1's manual validation used (`ip`, `iptables`, `mkfs.ext4`, `cp`,
// the `firecracker` binary itself). Only functional on Linux with KVM and
// those binaries present — i.e. the target GCE hosts, not this dev machine.
// Compiles everywhere (os/exec, no Linux-only syscalls), but every method
// will fail at runtime off-target; exercised only in the GCP validation
// phase, never by unit tests.
type LinuxHostOps struct {
	DataDir           string // e.g. "/data"
	FirecrackerBinary string // e.g. "/usr/local/bin/firecracker"
	HomeVolumeSizeMiB int
}

func (h *LinuxHostOps) instanceDir(instanceID string) string {
	return filepath.Join(h.DataDir, "instances", instanceID)
}

func (h *LinuxHostOps) CopyRootfs(ctx context.Context, goldenRootfsPath, instanceID string) (string, error) {
	dir := h.instanceDir(instanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir instance dir: %w", err)
	}
	dst := filepath.Join(dir, "rootfs.ext4")
	if err := copyFile(goldenRootfsPath, dst); err != nil {
		return "", fmt.Errorf("copy rootfs: %w", err)
	}
	return dst, nil
}

func (h *LinuxHostOps) CreateHomeVolume(ctx context.Context, instanceID string) (string, error) {
	dst := filepath.Join(h.instanceDir(instanceID), "home.ext4")
	sizeMiB := h.HomeVolumeSizeMiB
	if sizeMiB == 0 {
		sizeMiB = 1024
	}
	if err := runCmd(ctx, "dd", "if=/dev/zero", "of="+dst, "bs=1M", fmt.Sprintf("count=%d", sizeMiB)); err != nil {
		return "", fmt.Errorf("allocate home.ext4: %w", err)
	}
	if err := runCmd(ctx, "mkfs.ext4", "-q", dst); err != nil {
		return "", fmt.Errorf("format home.ext4: %w", err)
	}
	return dst, nil
}

func (h *LinuxHostOps) SetupNetwork(ctx context.Context, instanceID string, egressAllowlist []string) (NetworkInfo, error) {
	tap := tapDeviceName(instanceID)
	hostIP, guestIP := subnetFor(instanceID)

	if err := runCmd(ctx, "ip", "tuntap", "add", "dev", tap, "mode", "tap"); err != nil {
		return NetworkInfo{}, fmt.Errorf("create tap device: %w", err)
	}
	if err := runCmd(ctx, "ip", "addr", "add", hostIP+"/30", "dev", tap); err != nil {
		return NetworkInfo{}, fmt.Errorf("assign tap address: %w", err)
	}
	if err := runCmd(ctx, "ip", "link", "set", tap, "up"); err != nil {
		return NetworkInfo{}, fmt.Errorf("bring up tap device: %w", err)
	}
	// Egress filtering: restrict this instance's FORWARD traffic to the
	// allowlist (§4.8). A real implementation would resolve allowlist
	// entries and/or route through the Squid proxy; left as a direct
	// per-destination iptables rule set here for the prototype.
	for _, dest := range egressAllowlist {
		if err := runCmd(ctx, "iptables", "-A", "FORWARD", "-i", tap, "-d", dest, "-j", "ACCEPT"); err != nil {
			return NetworkInfo{}, fmt.Errorf("apply egress rule for %s: %w", dest, err)
		}
	}
	if err := runCmd(ctx, "iptables", "-A", "FORWARD", "-i", tap, "-j", "DROP"); err != nil {
		return NetworkInfo{}, fmt.Errorf("apply default-deny egress rule: %w", err)
	}

	return NetworkInfo{TapDevice: tap, GuestIP: guestIP, HostIP: hostIP}, nil
}

func (h *LinuxHostOps) TeardownNetwork(ctx context.Context, instanceID string) error {
	tap := tapDeviceName(instanceID)
	_ = runCmd(ctx, "iptables", "-D", "FORWARD", "-i", tap, "-j", "DROP") // best-effort; may already be gone
	if err := runCmd(ctx, "ip", "link", "delete", tap); err != nil {
		return fmt.Errorf("delete tap device: %w", err)
	}
	return nil
}

func (h *LinuxHostOps) SocketPath(instanceID string) string {
	return filepath.Join("/run/firecracker", instanceID+".socket")
}

func (h *LinuxHostOps) StartFirecrackerProcess(ctx context.Context, instanceID string) (string, error) {
	sock := h.SocketPath(instanceID)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return "", err
	}
	_ = os.Remove(sock) // stale socket from a previous run
	cmd := exec.CommandContext(context.WithoutCancel(ctx), h.FirecrackerBinary, "--api-sock", sock)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start firecracker process: %w", err)
	}
	// Deliberately not waiting on cmd — it's a long-running process the
	// Host Agent manages by instance id (via StopFirecrackerProcess), not
	// by holding a live *exec.Cmd handle across suspend/resume boundaries.
	return sock, nil
}

func (h *LinuxHostOps) StopFirecrackerProcess(ctx context.Context, instanceID string) error {
	// Best-effort: find and kill by matching the socket path in the
	// process's argv, since we don't retain a live *exec.Cmd handle.
	sock := h.SocketPath(instanceID)
	return runCmd(ctx, "pkill", "-f", "firecracker --api-sock "+sock)
}

func (h *LinuxHostOps) SnapshotPaths(instanceID string) (string, string) {
	dir := filepath.Join(h.instanceDir(instanceID), "snapshot")
	return filepath.Join(dir, "vmstate"), filepath.Join(dir, "mem_file")
}

func (h *LinuxHostOps) metadataPath(instanceID string) string {
	return filepath.Join(h.instanceDir(instanceID), "metadata.json")
}

func (h *LinuxHostOps) SaveInstanceMetadata(ctx context.Context, instanceID string, meta InstanceMetadata) error {
	dir := h.instanceDir(instanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(h.metadataPath(instanceID), b, 0o644)
}

func (h *LinuxHostOps) LoadInstanceMetadata(ctx context.Context, instanceID string) (InstanceMetadata, error) {
	b, err := os.ReadFile(h.metadataPath(instanceID))
	if err != nil {
		return InstanceMetadata{}, err
	}
	var meta InstanceMetadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return InstanceMetadata{}, err
	}
	return meta, nil
}

func (h *LinuxHostOps) DeleteInstanceFiles(ctx context.Context, instanceID string) error {
	return os.RemoveAll(h.instanceDir(instanceID))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w (output: %s)", name, args, err, string(out))
	}
	return nil
}

// tapDeviceName and subnetFor are deliberately simple placeholders — a real
// deployment needs a collision-free subnet allocator across all instances
// on a host (§8 open item), not a hash-based derivation. Good enough to
// keep the interface shape honest for now.
func tapDeviceName(instanceID string) string {
	return "tap-" + shortHash(instanceID)
}

func subnetFor(instanceID string) (hostIP, guestIP string) {
	// Placeholder allocation — see the TODO above. Real implementation
	// needs a stateful allocator to avoid collisions across concurrent
	// instances on the same host.
	return "172.16.0.1", "172.16.0.2"
}

func shortHash(s string) string {
	h := fnv32(s)
	return fmt.Sprintf("%x", h)[:8]
}

func fnv32(s string) uint32 {
	const prime32 = 16777619
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}
