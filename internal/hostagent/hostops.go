package hostagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// NetworkInfo is what SetupNetwork hands back: the TAP device to attach to
// Firecracker's network-interface config, the guest/host IPs on the /30 it
// just created (§4.3 step 3, §3.1 disk layout), and the Squid proxy address
// the guest's init script should configure HTTP_PROXY/HTTPS_PROXY to
// (§4.8) — empty if this HostOps implementation isn't running Squid.
type NetworkInfo struct {
	TapDevice      string
	GuestIP        string
	HostIP         string
	SquidProxyAddr string // e.g. "172.16.0.1:3128"; empty if egress proxying isn't configured
}

// HostOps abstracts every OS-level operation the Host Agent needs — file
// copying, TAP devices, iptables, process management — so VMManager's
// orchestration logic (the exact step sequence from §4.3) can be unit
// tested without real Linux/KVM/root privileges. LinuxHostOps below is the
// real implementation, only functional on the target GCE hosts; unit tests
// use fakeHostOps instead (see manager_test.go).
type HostOps interface {
	// PrepareKernel stages the platform-owned golden kernel for this
	// instance and returns the path value to pass to the Firecracker API's
	// boot-source call — the real host path when unjailed, or a
	// chroot-relative path when the Jailer is enabled (§4.6/Phase 6),
	// since a jailed firecracker process's own filesystem view is the
	// chroot root, not the real host filesystem.
	PrepareKernel(ctx context.Context, goldenKernelPath, instanceID string) (kernelPathForAPI string, err error)

	// CopyRootfs implements §4.3 step 1: a fresh per-instance copy of the
	// workload's golden rootfs, since a writable root can't safely be
	// shared across concurrent Firecracker processes. Returns the path
	// value to pass to the Firecracker API, same host-vs-jail-relative
	// distinction as PrepareKernel.
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

	// PrepareSnapshotDir ensures SnapshotPaths' parent directory exists.
	// Firecracker's own snapshot/create API writes directly to the paths
	// it's given — it doesn't create missing parent directories itself, and
	// nothing else in the boot/suspend flow creates a snapshot/
	// subdirectory ahead of time. Confirmed against a real suspend attempt:
	// without this, CreateSnapshot fails with a 400 from Firecracker before
	// ever touching the filesystem. Resume doesn't need this — it's only
	// reading files that must already exist, and their absence is the
	// expected ErrSnapshotMissing case.
	PrepareSnapshotDir(ctx context.Context, instanceID string) error

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

	// HasRootfs reports whether a golden rootfs already exists locally at
	// rootfsPath. The Controller calls this (via VMManager.HasRootfs, over
	// HTTP) before pushing one — §4.6's placement-locality fix: once
	// Controller and the Host Agent can be on different machines, a
	// workload's rootfs built by the Image Builder isn't automatically
	// visible on whichever host actually runs an instance of it. Checked
	// first so the (potentially large) transfer only happens once per
	// (workload, host) pair, not on every instance creation.
	HasRootfs(ctx context.Context, rootfsPath string) (bool, error)

	// SaveRootfs writes rootfs.ext4 bytes to rootfsPath, creating parent
	// directories as needed. Idempotent — overwrites if already present.
	SaveRootfs(ctx context.Context, rootfsPath string, data io.Reader) error
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
	Subnets           *SubnetAllocator // required — see NewLinuxHostOps
	Squid             *SquidManager    // nil disables egress proxying entirely (no ACLs applied, traffic still locked to the (unused) squid port and thus effectively blocked — see SetupNetwork)
	SquidPort         int              // 0 defaults to DefaultSquidPort
	Jailer            *JailerConfig    // nil (or Enabled: false) runs a bare firecracker process instead
}

// NewLinuxHostOps constructs a LinuxHostOps with its subnet pool
// initialized — the zero value is missing Subnets and would panic on first
// use, so this is the intended constructor. Squid is wired in separately
// (set the Squid field directly) since it's optional.
func NewLinuxHostOps(dataDir, firecrackerBinary string, homeVolumeSizeMiB int, subnetBaseCIDR string, subnetPoolSize int) (*LinuxHostOps, error) {
	subnets, err := NewSubnetAllocator(subnetBaseCIDR, subnetPoolSize)
	if err != nil {
		return nil, fmt.Errorf("init subnet allocator: %w", err)
	}
	return &LinuxHostOps{
		DataDir: dataDir, FirecrackerBinary: firecrackerBinary,
		HomeVolumeSizeMiB: homeVolumeSizeMiB, Subnets: subnets,
	}, nil
}

func (h *LinuxHostOps) jailerEnabled() bool {
	return h.Jailer != nil && h.Jailer.Enabled
}

// instanceRootDir is the host-visible directory holding everything for one
// instance. When the Jailer is enabled, this IS the chroot root itself
// (jailer's documented convention, see jailChrootRoot) — placing files
// there directly is how they become visible to the chrooted firecracker
// process, no separate copy-into-chroot step needed.
func (h *LinuxHostOps) instanceRootDir(instanceID string) string {
	if h.jailerEnabled() {
		return jailChrootRoot(h.Jailer.ChrootBaseDir, h.FirecrackerBinary, instanceID)
	}
	return filepath.Join(h.DataDir, "instances", instanceID)
}

// apiPath is the value to pass to the Firecracker API for a file named
// filename in this instance's root dir. Unjailed, Firecracker sees the
// real host filesystem, so this is the real absolute path. Jailed,
// firecracker's own "/" IS instanceRootDir (jailer chroots it there before
// exec), so the correct API value is the absolute path *from inside that
// view* — i.e. just "/" + filename, never the host-side path.
func (h *LinuxHostOps) apiPath(instanceID, filename string) string {
	if h.jailerEnabled() {
		return "/" + filename
	}
	return filepath.Join(h.instanceRootDir(instanceID), filename)
}

func (h *LinuxHostOps) PrepareKernel(ctx context.Context, goldenKernelPath, instanceID string) (string, error) {
	dir := h.instanceRootDir(instanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir instance root dir: %w", err)
	}
	if !h.jailerEnabled() {
		// Unjailed: firecracker reads the golden kernel directly, no need
		// to duplicate it per instance.
		return goldenKernelPath, nil
	}
	dst := filepath.Join(dir, "vmlinux")
	if err := copyFile(goldenKernelPath, dst); err != nil {
		return "", fmt.Errorf("stage kernel into chroot: %w", err)
	}
	return h.apiPath(instanceID, "vmlinux"), nil
}

func (h *LinuxHostOps) CopyRootfs(ctx context.Context, goldenRootfsPath, instanceID string) (string, error) {
	dir := h.instanceRootDir(instanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir instance root dir: %w", err)
	}
	dst := filepath.Join(dir, "rootfs.ext4")
	if err := copyFile(goldenRootfsPath, dst); err != nil {
		return "", fmt.Errorf("copy rootfs: %w", err)
	}
	return h.apiPath(instanceID, "rootfs.ext4"), nil
}

func (h *LinuxHostOps) CreateHomeVolume(ctx context.Context, instanceID string) (string, error) {
	dst := filepath.Join(h.instanceRootDir(instanceID), "home.ext4")
	sizeMiB := h.HomeVolumeSizeMiB
	if sizeMiB == 0 {
		sizeMiB = 1024
	}
	// truncate, not `dd if=/dev/zero`: mkfs.ext4 doesn't need the backing
	// bytes to actually be written — a sparse file of the right size is
	// enough, and skipping a real 1GiB(+) write is the difference between
	// this taking milliseconds and taking however long the host's disk
	// needs to sustain that write (pd-standard can make that genuinely
	// slow — this was directly responsible for a real CreateInstance
	// timeout during Tier 3 validation).
	if err := runCmd(ctx, "truncate", "-s", fmt.Sprintf("%dM", sizeMiB), dst); err != nil {
		return "", fmt.Errorf("allocate home.ext4: %w", err)
	}
	if err := runCmd(ctx, "mkfs.ext4", "-q", dst); err != nil {
		return "", fmt.Errorf("format home.ext4: %w", err)
	}
	return h.apiPath(instanceID, "home.ext4"), nil
}

func (h *LinuxHostOps) squidPort() int {
	if h.SquidPort != 0 {
		return h.SquidPort
	}
	return DefaultSquidPort
}

func (h *LinuxHostOps) SetupNetwork(ctx context.Context, instanceID string, egressAllowlist []string) (NetworkInfo, error) {
	tap := tapDeviceName(instanceID)
	hostIP, guestIP, err := h.Subnets.Allocate(instanceID)
	if err != nil {
		return NetworkInfo{}, fmt.Errorf("allocate subnet: %w", err)
	}

	if err := runCmd(ctx, "ip", "tuntap", "add", "dev", tap, "mode", "tap"); err != nil {
		return NetworkInfo{}, fmt.Errorf("create tap device: %w", err)
	}
	if err := runCmd(ctx, "ip", "addr", "add", hostIP+"/30", "dev", tap); err != nil {
		return NetworkInfo{}, fmt.Errorf("assign tap address: %w", err)
	}
	if err := runCmd(ctx, "ip", "link", "set", tap, "up"); err != nil {
		return NetworkInfo{}, fmt.Errorf("bring up tap device: %w", err)
	}

	// Egress filtering (§4.8): Squid is the sole enforcement point for the
	// allowlist, via per-instance ACLs (applied below). iptables' only job
	// is to prevent bypass — this TAP's forwarded traffic may reach ONLY
	// the host's Squid port, nothing else, regardless of what the guest
	// tries to connect to directly.
	port := h.squidPort()
	if err := runCmd(ctx, "iptables", "-A", "FORWARD", "-i", tap, "-d", hostIP, "-p", "tcp", "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT"); err != nil {
		return NetworkInfo{}, fmt.Errorf("allow squid-bound traffic: %w", err)
	}
	if err := runCmd(ctx, "iptables", "-A", "FORWARD", "-i", tap, "-j", "DROP"); err != nil {
		return NetworkInfo{}, fmt.Errorf("apply default-deny egress rule: %w", err)
	}

	if h.Squid != nil {
		if err := h.Squid.ApplyACL(ctx, instanceID, guestIP, egressAllowlist); err != nil {
			return NetworkInfo{}, fmt.Errorf("apply squid ACL: %w", err)
		}
	}

	return NetworkInfo{
		TapDevice: tap, GuestIP: guestIP, HostIP: hostIP,
		SquidProxyAddr: fmt.Sprintf("%s:%d", hostIP, port),
	}, nil
}

func (h *LinuxHostOps) TeardownNetwork(ctx context.Context, instanceID string) error {
	tap := tapDeviceName(instanceID)
	hostIP, _, _ := h.Subnets.Lookup(instanceID) // read before releasing, to remove the matching iptables rule below
	h.Subnets.Release(instanceID)

	if h.Squid != nil {
		if err := h.Squid.RemoveACL(ctx, instanceID); err != nil {
			// Best-effort — a Squid reload hiccup shouldn't block tearing
			// down the TAP device and reclaiming the subnet.
			_ = err
		}
	}
	port := h.squidPort()
	_ = runCmd(ctx, "iptables", "-D", "FORWARD", "-i", tap, "-d", hostIP, "-p", "tcp", "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT")
	_ = runCmd(ctx, "iptables", "-D", "FORWARD", "-i", tap, "-j", "DROP") // best-effort; may already be gone
	if err := runCmd(ctx, "ip", "link", "delete", tap); err != nil {
		return fmt.Errorf("delete tap device: %w", err)
	}
	return nil
}

// SocketPath is always the host-visible location — this is Host Agent's
// own bookkeeping (where it dials in from outside to talk to Firecracker's
// API), not a value passed to the Firecracker API itself, so it's never
// jail-relative even when jailing is enabled. Unified to a fixed filename
// inside instanceRootDir, whether jailed or not.
func (h *LinuxHostOps) SocketPath(instanceID string) string {
	return filepath.Join(h.instanceRootDir(instanceID), "api.sock")
}

func (h *LinuxHostOps) StartFirecrackerProcess(ctx context.Context, instanceID string) (string, error) {
	sock := h.SocketPath(instanceID)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return "", err
	}
	_ = os.Remove(sock) // stale socket from a previous run

	// The guest's console=ttyS0 boot argument (manager.go's configureAndStart)
	// only takes effect if something's listening on the other end — without
	// this, a guest that panics or never brings up its network is
	// indistinguishable from one that's still booting, since both just look
	// like nothing answering on the guest port. Firecracker writes serial
	// output straight to its own stdout/stderr with no extra API
	// configuration needed, so capturing those to a file per instance is
	// enough to see it.
	consoleLog, err := os.Create(filepath.Join(h.instanceRootDir(instanceID), "console.log"))
	if err != nil {
		return "", fmt.Errorf("create console log: %w", err)
	}

	if h.jailerEnabled() {
		// The jailer chroots the process before exec'ing it, so the
		// --api-sock value passed to firecracker (after the "--"
		// separator) must be relative to that view, not the host path —
		// see JailerConfig's caveat about this being unverified.
		name, args := buildJailerCommand(*h.Jailer, h.FirecrackerBinary, instanceID, "api.sock")
		cmd := exec.CommandContext(context.WithoutCancel(ctx), name, args...)
		cmd.Stdout, cmd.Stderr = consoleLog, consoleLog
		if err := cmd.Start(); err != nil {
			consoleLog.Close()
			return "", fmt.Errorf("start jailed firecracker process: %w", err)
		}
		go func() { _ = cmd.Wait(); consoleLog.Close() }()
		return sock, waitForSocket(ctx, sock)
	}

	cmd := exec.CommandContext(context.WithoutCancel(ctx), h.FirecrackerBinary, "--api-sock", sock)
	cmd.Stdout, cmd.Stderr = consoleLog, consoleLog
	if err := cmd.Start(); err != nil {
		consoleLog.Close()
		return "", fmt.Errorf("start firecracker process: %w", err)
	}
	// Deliberately not waiting on cmd for lifecycle purposes — it's a
	// long-running process the Host Agent manages by instance id (via
	// StopFirecrackerProcess), not by holding a live *exec.Cmd handle across
	// suspend/resume boundaries. This goroutine only exists to close
	// consoleLog once the process actually exits, so the fd doesn't leak.
	go func() { _ = cmd.Wait(); consoleLog.Close() }()
	return sock, waitForSocket(ctx, sock)
}

// waitForSocket polls for the API socket Firecracker creates on startup —
// cmd.Start() only confirms the process was exec'd, not that it's gotten far
// enough to bind its API socket, and the caller's very next step is an API
// call over that socket. Without this, that first call can race a
// still-starting process and fail with a plain "no such file or directory"
// that looks like a boot failure rather than the startup lag it actually is.
func waitForSocket(ctx context.Context, sock string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker API socket %s did not appear within 5s of starting the process", sock)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (h *LinuxHostOps) StopFirecrackerProcess(ctx context.Context, instanceID string) error {
	// Best-effort: find and kill by matching the instance id in the
	// process's argv, since we don't retain a live *exec.Cmd handle. Jailer
	// invocations carry --id <instanceID>; bare firecracker invocations
	// carry the (also instance-specific) socket path — either pattern
	// uniquely identifies the right process.
	if h.jailerEnabled() {
		return runCmd(ctx, "pkill", "-f", "--id "+instanceID)
	}
	return runCmd(ctx, "pkill", "-f", "firecracker --api-sock "+h.SocketPath(instanceID))
}

func (h *LinuxHostOps) SnapshotPaths(instanceID string) (string, string) {
	// Kept under a snapshot/ subdirectory (not flattened into
	// instanceRootDir directly) to stay consistent with the disk layout
	// documented in §3.1/§4.7, jailed or not.
	return h.apiPath(instanceID, "snapshot/vmstate"), h.apiPath(instanceID, "snapshot/mem_file")
}

func (h *LinuxHostOps) PrepareSnapshotDir(ctx context.Context, instanceID string) error {
	return os.MkdirAll(filepath.Join(h.instanceRootDir(instanceID), "snapshot"), 0o755)
}

func (h *LinuxHostOps) metadataPath(instanceID string) string {
	// Host Agent's own bookkeeping, like SocketPath — never jail-relative.
	return filepath.Join(h.instanceRootDir(instanceID), "metadata.json")
}

func (h *LinuxHostOps) SaveInstanceMetadata(ctx context.Context, instanceID string, meta InstanceMetadata) error {
	dir := h.instanceRootDir(instanceID)
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
	return os.RemoveAll(h.instanceRootDir(instanceID))
}

func (h *LinuxHostOps) HasRootfs(ctx context.Context, rootfsPath string) (bool, error) {
	_, err := os.Stat(rootfsPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat rootfs: %w", err)
}

func (h *LinuxHostOps) SaveRootfs(ctx context.Context, rootfsPath string, data io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0o755); err != nil {
		return fmt.Errorf("create rootfs parent dir: %w", err)
	}
	f, err := os.Create(rootfsPath)
	if err != nil {
		return fmt.Errorf("create rootfs file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, data); err != nil {
		return fmt.Errorf("write rootfs data: %w", err)
	}
	return nil
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

// tapDeviceName derives a short, deterministic device name from the
// instance ID. Collision-resistant enough for a prototype (32-bit hash
// space) — subnet allocation was the piece that needed a real stateful
// allocator (SubnetAllocator, subnet.go), since the old placeholder there
// returned the identical IP for every instance, a guaranteed collision
// rather than a low-probability one.
func tapDeviceName(instanceID string) string {
	return "tap-" + shortHash(instanceID)
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
