package hostagent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrSnapshotMissing is returned by ResumeVM when the instance's saved
// metadata or snapshot files can't be found — a definitively unrecoverable
// resume, distinct from a transient failure. The HTTP layer maps this to a
// 404, which the Controller's HTTPHostAgentClient in turn maps back to its
// own ErrSnapshotMissing sentinel (§4.2) — the two services share no Go
// types, only this status-code convention over HTTP.
var ErrSnapshotMissing = errors.New("snapshot or instance metadata missing")

// BootRequest mirrors the Controller's POST /vm body (§4.2 "B) Host Agent
// API"). Defined locally rather than imported from the controller package —
// the two services only ever talk over HTTP, never share Go types, matching
// the strict service boundary established for the whole platform.
type BootRequest struct {
	InstanceID      string
	RootfsRef       string // path to the workload's golden rootfs; already local per §4.6's placement-locality note
	VCPUs           int
	MemoryMiB       int
	EgressAllowlist []string
}

type VMEndpoint struct {
	GuestIP   string
	GuestPort int
}

type Config struct {
	KernelImagePath string // shared vmlinux, platform-owned (§4.6)
	GuestPort       int    // fixed platform convention (§4.3), e.g. 8080
	BootTimeout     time.Duration
}

// VMManager implements the exact boot/suspend/resume/delete sequences from
// §4.3, composed from HostOps (OS-level operations), a FirecrackerClient
// factory (bound to a specific socket per call), and a ReadinessChecker.
// This is the fully unit-testable core of the Host Agent — see
// manager_test.go, which exercises every flow against fakes with no real
// Linux/KVM/Firecracker present.
type VMManager struct {
	ops       HostOps
	fcFactory func(socketPath string) FirecrackerClient
	readiness ReadinessChecker
	cfg       Config
}

func NewVMManager(ops HostOps, fcFactory func(string) FirecrackerClient, readiness ReadinessChecker, cfg Config) *VMManager {
	return &VMManager{ops: ops, fcFactory: fcFactory, readiness: readiness, cfg: cfg}
}

// BootVM implements §4.3 "Flow — Boot a microVM", steps 1-7.
func (m *VMManager) BootVM(ctx context.Context, req BootRequest) (VMEndpoint, error) {
	rootfsPath, err := m.ops.CopyRootfs(ctx, req.RootfsRef, req.InstanceID) // step 1
	if err != nil {
		return VMEndpoint{}, fmt.Errorf("copy rootfs: %w", err)
	}
	homePath, err := m.ops.CreateHomeVolume(ctx, req.InstanceID) // step 2
	if err != nil {
		return VMEndpoint{}, fmt.Errorf("create home volume: %w", err)
	}
	net, err := m.ops.SetupNetwork(ctx, req.InstanceID, req.EgressAllowlist) // step 3
	if err != nil {
		return VMEndpoint{}, fmt.Errorf("setup network: %w", err)
	}
	if err := m.ops.SaveInstanceMetadata(ctx, req.InstanceID, InstanceMetadata{EgressAllowlist: req.EgressAllowlist}); err != nil {
		return VMEndpoint{}, fmt.Errorf("save instance metadata: %w", err)
	}

	sock, err := m.ops.StartFirecrackerProcess(ctx, req.InstanceID) // step 4
	if err != nil {
		return VMEndpoint{}, fmt.Errorf("start firecracker process: %w", err)
	}
	fc := m.fcFactory(sock)

	if err := m.configureAndStart(ctx, fc, net, rootfsPath, homePath, req.VCPUs, req.MemoryMiB); err != nil { // step 5
		return VMEndpoint{}, err
	}

	ep, err := m.waitReadyAndBuildEndpoint(ctx, net.GuestIP) // step 6-7
	if err != nil {
		return VMEndpoint{}, err
	}
	return ep, nil
}

// configureAndStart implements §4.3 step 5's ordered PUT sequence: boot
// source (with a kernel `ip=` argument so the guest gets a static network
// config with zero guest-side cooperation, §4.3), rootfs drive, home drive,
// network interface, machine config, then InstanceStart.
func (m *VMManager) configureAndStart(ctx context.Context, fc FirecrackerClient, net NetworkInfo, rootfsPath, homePath string, vcpus, memoryMiB int) error {
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:255.255.255.252::eth0:off", net.GuestIP, net.HostIP)
	if err := fc.SetBootSource(ctx, m.cfg.KernelImagePath, bootArgs); err != nil {
		return fmt.Errorf("set boot source: %w", err)
	}
	if err := fc.SetDrive(ctx, "rootfs", rootfsPath, true, false); err != nil {
		return fmt.Errorf("attach rootfs drive: %w", err)
	}
	if err := fc.SetDrive(ctx, "home", homePath, false, false); err != nil {
		return fmt.Errorf("attach home drive: %w", err)
	}
	if err := fc.SetNetworkInterface(ctx, "eth0", net.TapDevice); err != nil {
		return fmt.Errorf("attach network interface: %w", err)
	}
	if err := fc.SetMachineConfig(ctx, vcpus, memoryMiB); err != nil {
		return fmt.Errorf("set machine config: %w", err)
	}
	if err := fc.InstanceStart(ctx); err != nil {
		return fmt.Errorf("start instance: %w", err)
	}
	return nil
}

func (m *VMManager) waitReadyAndBuildEndpoint(ctx context.Context, guestIP string) (VMEndpoint, error) {
	guestAddr := fmt.Sprintf("%s:%d", guestIP, m.cfg.GuestPort)
	if err := m.readiness.WaitReady(ctx, guestAddr, m.cfg.BootTimeout); err != nil {
		return VMEndpoint{}, fmt.Errorf("wait for guest readiness: %w", err)
	}
	return VMEndpoint{GuestIP: guestIP, GuestPort: m.cfg.GuestPort}, nil
}

// SuspendVM implements §4.3 "Flow — Snapshot/suspend".
func (m *VMManager) SuspendVM(ctx context.Context, instanceID string) error {
	fc := m.fcFactory(m.ops.SocketPath(instanceID))
	if err := fc.Pause(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	snapshotPath, memFilePath := m.ops.SnapshotPaths(instanceID)
	if err := fc.CreateSnapshot(ctx, snapshotPath, memFilePath); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	if err := m.ops.StopFirecrackerProcess(ctx, instanceID); err != nil {
		return fmt.Errorf("stop firecracker process: %w", err)
	}
	if err := m.ops.TeardownNetwork(ctx, instanceID); err != nil {
		return fmt.Errorf("teardown network: %w", err)
	}
	return nil
}

// ResumeVM implements §4.3 "Flow — Restore/resume". Takes only instanceID,
// matching the Controller's minimal resume request (§4.2) — the egress
// allowlist needed to re-setup networking comes from the metadata BootVM
// saved, not from the caller.
func (m *VMManager) ResumeVM(ctx context.Context, instanceID string) (VMEndpoint, error) {
	meta, err := m.ops.LoadInstanceMetadata(ctx, instanceID)
	if err != nil {
		return VMEndpoint{}, fmt.Errorf("%w: load instance metadata: %v", ErrSnapshotMissing, err)
	}

	net, err := m.ops.SetupNetwork(ctx, instanceID, meta.EgressAllowlist) // may get a fresh IP
	if err != nil {
		return VMEndpoint{}, fmt.Errorf("setup network: %w", err)
	}
	sock, err := m.ops.StartFirecrackerProcess(ctx, instanceID)
	if err != nil {
		return VMEndpoint{}, fmt.Errorf("start firecracker process: %w", err)
	}
	fc := m.fcFactory(sock)

	// Simplification worth flagging: any LoadSnapshot failure here is
	// treated as ErrSnapshotMissing, even though in principle a Firecracker
	// API call could also fail transiently for unrelated reasons. The real
	// UnixSocketFirecrackerClient doesn't yet distinguish "snapshot file
	// not found on disk" from other failure modes — refining that split
	// needs testing against a real Firecracker process, out of reach on
	// this dev machine (Phase C: GCP validation).
	snapshotPath, memFilePath := m.ops.SnapshotPaths(instanceID)
	if err := fc.LoadSnapshot(ctx, snapshotPath, memFilePath, true); err != nil {
		return VMEndpoint{}, fmt.Errorf("%w: load snapshot: %v", ErrSnapshotMissing, err)
	}

	return m.waitReadyAndBuildEndpoint(ctx, net.GuestIP)
}

// DeleteVM implements §4.3 "Flow — Delete": best-effort process/network
// teardown (idempotent — fine if already gone), then the consequential
// step, deleting the instance's files.
func (m *VMManager) DeleteVM(ctx context.Context, instanceID string) error {
	_ = m.ops.StopFirecrackerProcess(ctx, instanceID)
	_ = m.ops.TeardownNetwork(ctx, instanceID)
	return m.ops.DeleteInstanceFiles(ctx, instanceID)
}
