package hostagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Fakes ---

type fakeHostOps struct {
	mu sync.Mutex

	prepareKernelErr   error
	copyRootfsErr      error
	createHomeErr      error
	setupNetworkErr    error
	startProcessErr    error
	stopProcessErr     error
	saveMetadataErr    error
	loadMetadataErr    error
	deleteFilesErr     error
	teardownNetErr     error
	nextGuestIP        string
	nextHostIP         string
	nextSquidProxyAddr string
	metadata           map[string]InstanceMetadata
	deletedInstances   map[string]bool
	stoppedProcesses   map[string]bool
	torndownNetworks   map[string]bool
	hasRootfsErr       error
	saveRootfsErr      error
	savedRootfs        map[string][]byte
}

func newFakeHostOps() *fakeHostOps {
	return &fakeHostOps{
		nextGuestIP:      "172.16.0.2",
		nextHostIP:       "172.16.0.1",
		metadata:         map[string]InstanceMetadata{},
		deletedInstances: map[string]bool{},
		stoppedProcesses: map[string]bool{},
		torndownNetworks: map[string]bool{},
		savedRootfs:      map[string][]byte{},
	}
}

func (f *fakeHostOps) PrepareKernel(ctx context.Context, goldenKernelPath, instanceID string) (string, error) {
	if f.prepareKernelErr != nil {
		return "", f.prepareKernelErr
	}
	return goldenKernelPath, nil
}

func (f *fakeHostOps) CopyRootfs(ctx context.Context, goldenRootfsPath, instanceID string) (string, error) {
	if f.copyRootfsErr != nil {
		return "", f.copyRootfsErr
	}
	return "/data/instances/" + instanceID + "/rootfs.ext4", nil
}

func (f *fakeHostOps) CreateHomeVolume(ctx context.Context, instanceID string) (string, error) {
	if f.createHomeErr != nil {
		return "", f.createHomeErr
	}
	return "/data/instances/" + instanceID + "/home.ext4", nil
}

func (f *fakeHostOps) SetupNetwork(ctx context.Context, instanceID string, egressAllowlist []string) (NetworkInfo, error) {
	if f.setupNetworkErr != nil {
		return NetworkInfo{}, f.setupNetworkErr
	}
	return NetworkInfo{
		TapDevice: "tap-" + instanceID, GuestIP: f.nextGuestIP, HostIP: f.nextHostIP,
		SquidProxyAddr: f.nextSquidProxyAddr,
	}, nil
}

func (f *fakeHostOps) TeardownNetwork(ctx context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torndownNetworks[instanceID] = true
	return f.teardownNetErr
}

func (f *fakeHostOps) StartFirecrackerProcess(ctx context.Context, instanceID string) (string, error) {
	if f.startProcessErr != nil {
		return "", f.startProcessErr
	}
	return "/run/firecracker/" + instanceID + ".socket", nil
}

func (f *fakeHostOps) StopFirecrackerProcess(ctx context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stoppedProcesses[instanceID] = true
	return f.stopProcessErr
}

func (f *fakeHostOps) SnapshotPaths(instanceID string) (string, string) {
	base := "/data/instances/" + instanceID + "/snapshot/"
	return base + "vmstate", base + "mem_file"
}

func (f *fakeHostOps) PrepareSnapshotDir(ctx context.Context, instanceID string) error {
	return nil
}

func (f *fakeHostOps) SocketPath(instanceID string) string {
	return "/run/firecracker/" + instanceID + ".socket"
}

func (f *fakeHostOps) SaveInstanceMetadata(ctx context.Context, instanceID string, meta InstanceMetadata) error {
	if f.saveMetadataErr != nil {
		return f.saveMetadataErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metadata[instanceID] = meta
	return nil
}

func (f *fakeHostOps) LoadInstanceMetadata(ctx context.Context, instanceID string) (InstanceMetadata, error) {
	if f.loadMetadataErr != nil {
		return InstanceMetadata{}, f.loadMetadataErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	meta, ok := f.metadata[instanceID]
	if !ok {
		return InstanceMetadata{}, errors.New("no metadata found")
	}
	return meta, nil
}

func (f *fakeHostOps) DeleteInstanceFiles(ctx context.Context, instanceID string) error {
	if f.deleteFilesErr != nil {
		return f.deleteFilesErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedInstances[instanceID] = true
	return nil
}

func (f *fakeHostOps) HasRootfs(ctx context.Context, rootfsPath string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hasRootfsErr != nil {
		return false, f.hasRootfsErr
	}
	_, ok := f.savedRootfs[rootfsPath]
	return ok, nil
}

func (f *fakeHostOps) SaveRootfs(ctx context.Context, rootfsPath string, data io.Reader) error {
	if f.saveRootfsErr != nil {
		return f.saveRootfsErr
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.savedRootfs[rootfsPath] = b
	return nil
}

// fakeFirecrackerClient records calls and can be configured to fail at any step.
type fakeFirecrackerClient struct {
	mu sync.Mutex

	setBootSourceErr  error
	setDriveErr       error
	setNetIfaceErr    error
	setMachineCfgErr  error
	instanceStartErr  error
	pauseErr          error
	createSnapshotErr error
	loadSnapshotErr   error

	bootSourcePath string
	bootArgs       string
	drivesSet      []string
	instanceUp     bool
	paused         bool
	snapshotted    bool
	snapshotLoad   bool
}

func (f *fakeFirecrackerClient) SetBootSource(ctx context.Context, kernelImagePath, bootArgs string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootSourcePath = kernelImagePath
	f.bootArgs = bootArgs
	return f.setBootSourceErr
}
func (f *fakeFirecrackerClient) SetDrive(ctx context.Context, driveID, pathOnHost string, isRootDevice, isReadOnly bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drivesSet = append(f.drivesSet, driveID)
	return f.setDriveErr
}
func (f *fakeFirecrackerClient) SetNetworkInterface(ctx context.Context, ifaceID, hostDevName string) error {
	return f.setNetIfaceErr
}
func (f *fakeFirecrackerClient) SetMachineConfig(ctx context.Context, vcpuCount, memSizeMiB int) error {
	return f.setMachineCfgErr
}
func (f *fakeFirecrackerClient) InstanceStart(ctx context.Context) error {
	if f.instanceStartErr != nil {
		return f.instanceStartErr
	}
	f.mu.Lock()
	f.instanceUp = true
	f.mu.Unlock()
	return nil
}
func (f *fakeFirecrackerClient) Pause(ctx context.Context) error {
	if f.pauseErr != nil {
		return f.pauseErr
	}
	f.paused = true
	return nil
}
func (f *fakeFirecrackerClient) CreateSnapshot(ctx context.Context, snapshotPath, memFilePath string) error {
	if f.createSnapshotErr != nil {
		return f.createSnapshotErr
	}
	f.snapshotted = true
	return nil
}
func (f *fakeFirecrackerClient) LoadSnapshot(ctx context.Context, snapshotPath, memFilePath string, resumeVM bool) error {
	if f.loadSnapshotErr != nil {
		return f.loadSnapshotErr
	}
	f.snapshotLoad = true
	return nil
}

type fakeReadiness struct {
	err error
}

func (f *fakeReadiness) WaitReady(ctx context.Context, addr string, timeout time.Duration) error {
	return f.err
}

// fakeGuestProxy records the last Forward call and returns a canned
// response/error — the manager tests care about registry resolution
// (does Proxy find the right guest_ip:port), not real HTTP forwarding,
// which is guestproxy_test.go's job.
type fakeGuestProxy struct {
	mu              sync.Mutex
	lastGuestIP     string
	lastGuestPort   int
	forwardErr      error
	forwardResponse *ProxyResponse
}

func (f *fakeGuestProxy) Forward(ctx context.Context, guestIP string, guestPort int, req *ProxyRequest) (*ProxyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastGuestIP, f.lastGuestPort = guestIP, guestPort
	if f.forwardErr != nil {
		return nil, f.forwardErr
	}
	if f.forwardResponse != nil {
		return f.forwardResponse, nil
	}
	return &ProxyResponse{StatusCode: 200}, nil
}

func newTestManager(ops HostOps, fc *fakeFirecrackerClient, readiness ReadinessChecker) *VMManager {
	return newTestManagerWithProxy(ops, fc, readiness, &fakeGuestProxy{})
}

func newTestManagerWithProxy(ops HostOps, fc *fakeFirecrackerClient, readiness ReadinessChecker, proxy GuestProxy) *VMManager {
	return NewVMManager(ops, func(string) FirecrackerClient { return fc }, readiness, Config{
		KernelImagePath: "/data/vmlinux",
		GuestPort:       8080,
		BootTimeout:     time.Second,
	}, proxy)
}

// --- BootVM ---

func TestBootVM_Success(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})

	ep, err := m.BootVM(context.Background(), BootRequest{
		InstanceID: "wl_x:agent:abc", RootfsRef: "/data/workloads/wl_x/rootfs.ext4",
		VCPUs: 2, MemoryMiB: 512, EgressAllowlist: []string{"api.openai.com"},
	})
	if err != nil {
		t.Fatalf("BootVM: %v", err)
	}
	if ep.GuestIP != "172.16.0.2" || ep.GuestPort != 8080 {
		t.Errorf("got %+v", ep)
	}
	if !fc.instanceUp {
		t.Error("InstanceStart should have been called")
	}
	if len(fc.drivesSet) != 2 || fc.drivesSet[0] != "rootfs" || fc.drivesSet[1] != "home" {
		t.Errorf("drives set = %v, want [rootfs home] in order", fc.drivesSet)
	}
	wantArgs := "console=ttyS0 reboot=k panic=1 pci=off ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off"
	if fc.bootArgs != wantArgs {
		t.Errorf("boot_args = %q, want %q", fc.bootArgs, wantArgs)
	}
	meta, err := ops.LoadInstanceMetadata(context.Background(), "wl_x:agent:abc")
	if err != nil {
		t.Fatalf("expected metadata to be saved: %v", err)
	}
	if len(meta.EgressAllowlist) != 1 || meta.EgressAllowlist[0] != "api.openai.com" {
		t.Errorf("saved metadata = %+v", meta)
	}
}

func TestBootVM_IncludesSquidProxyInBootArgsWhenPresent(t *testing.T) {
	ops := newFakeHostOps()
	ops.nextSquidProxyAddr = "172.16.0.1:3128"
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "wl_x:agent:abc"}); err != nil {
		t.Fatalf("BootVM: %v", err)
	}
	if !strings.Contains(fc.bootArgs, "platform.squid_proxy=172.16.0.1:3128") {
		t.Errorf("boot_args = %q, want it to include the squid proxy address", fc.bootArgs)
	}
}

func TestBootVM_OmitsSquidProxyArgWhenAbsent(t *testing.T) {
	ops := newFakeHostOps() // nextSquidProxyAddr left at its zero value ""
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "wl_x:agent:abc"}); err != nil {
		t.Fatalf("BootVM: %v", err)
	}
	if strings.Contains(fc.bootArgs, "squid_proxy") {
		t.Errorf("boot_args = %q, should not mention squid_proxy when HostOps didn't set one", fc.bootArgs)
	}
}

func TestBootVM_UsesPrepareKernelResultNotRawConfigPath(t *testing.T) {
	ops := &fakeHostOpsWithKernelOverride{fakeHostOps: newFakeHostOps(), kernelPath: "/jail/root/wl_x/vmlinux"}
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{}) // Config.KernelImagePath = "/data/vmlinux"

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "wl_x:agent:abc"}); err != nil {
		t.Fatalf("BootVM: %v", err)
	}
	if fc.bootSourcePath != "/jail/root/wl_x/vmlinux" {
		t.Errorf("boot source path = %q, want the value PrepareKernel returned, not the raw Config.KernelImagePath", fc.bootSourcePath)
	}
}

func TestBootVM_PrepareKernelFailure(t *testing.T) {
	ops := newFakeHostOps()
	ops.prepareKernelErr = errors.New("chroot not writable")
	m := newTestManager(ops, &fakeFirecrackerClient{}, &fakeReadiness{})
	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "x"}); err == nil {
		t.Fatal("expected an error")
	}
}

// fakeHostOpsWithKernelOverride lets a test control PrepareKernel's return
// value independently of the other fakeHostOps fields.
type fakeHostOpsWithKernelOverride struct {
	*fakeHostOps
	kernelPath string
}

func (f *fakeHostOpsWithKernelOverride) PrepareKernel(ctx context.Context, goldenKernelPath, instanceID string) (string, error) {
	return f.kernelPath, nil
}

func TestBootVM_RootfsCopyFailure(t *testing.T) {
	ops := newFakeHostOps()
	ops.copyRootfsErr = errors.New("disk full")
	m := newTestManager(ops, &fakeFirecrackerClient{}, &fakeReadiness{})
	_, err := m.BootVM(context.Background(), BootRequest{InstanceID: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestBootVM_InstanceStartFailure(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{instanceStartErr: errors.New("firecracker rejected InstanceStart")}
	m := newTestManager(ops, fc, &fakeReadiness{})
	_, err := m.BootVM(context.Background(), BootRequest{InstanceID: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestBootVM_ReadinessTimeout(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{err: errors.New("connection refused")})
	_, err := m.BootVM(context.Background(), BootRequest{InstanceID: "x"})
	if err == nil {
		t.Fatal("expected an error when the guest never becomes ready")
	}
}

// --- SuspendVM ---

func TestSuspendVM_Success(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})

	if err := m.SuspendVM(context.Background(), "inst-1"); err != nil {
		t.Fatalf("SuspendVM: %v", err)
	}
	if !fc.paused {
		t.Error("expected Pause to be called")
	}
	if !fc.snapshotted {
		t.Error("expected CreateSnapshot to be called")
	}
	if !ops.stoppedProcesses["inst-1"] {
		t.Error("expected the firecracker process to be stopped")
	}
	if !ops.torndownNetworks["inst-1"] {
		t.Error("expected the network to be torn down")
	}
}

func TestSuspendVM_PauseFailureStopsShort(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{pauseErr: errors.New("firecracker unreachable")}
	m := newTestManager(ops, fc, &fakeReadiness{})

	if err := m.SuspendVM(context.Background(), "inst-1"); err == nil {
		t.Fatal("expected an error")
	}
	if fc.snapshotted {
		t.Error("should not attempt CreateSnapshot after a failed Pause")
	}
	if ops.stoppedProcesses["inst-1"] {
		t.Error("should not stop the process if pause/snapshot never completed")
	}
}

// --- ResumeVM ---

func TestResumeVM_Success(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})

	// Boot first so metadata exists, matching real usage.
	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1", EgressAllowlist: []string{"pypi.org"}}); err != nil {
		t.Fatal(err)
	}

	ops.nextGuestIP = "172.16.0.9" // simulate a fresh IP on resume
	ep, err := m.ResumeVM(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("ResumeVM: %v", err)
	}
	if ep.GuestIP != "172.16.0.9" {
		t.Errorf("guest_ip = %s, want the refreshed 172.16.0.9", ep.GuestIP)
	}
	if !fc.snapshotLoad {
		t.Error("expected LoadSnapshot to be called")
	}
}

func TestResumeVM_MissingMetadataFails(t *testing.T) {
	ops := newFakeHostOps() // never booted, no metadata saved
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})

	_, err := m.ResumeVM(context.Background(), "never-booted")
	if !errors.Is(err, ErrSnapshotMissing) {
		t.Fatalf("got %v, want ErrSnapshotMissing", err)
	}
}

func TestResumeVM_LoadSnapshotFailure(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})
	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}

	fc.loadSnapshotErr = errors.New("snapshot file not found")
	_, err := m.ResumeVM(context.Background(), "inst-1")
	if !errors.Is(err, ErrSnapshotMissing) {
		t.Fatalf("got %v, want ErrSnapshotMissing", err)
	}
}

// --- DeleteVM ---

func TestDeleteVM_Success(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})
	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteVM(context.Background(), "inst-1"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if !ops.deletedInstances["inst-1"] {
		t.Error("expected instance files to be deleted")
	}
}

func TestDeleteVM_IgnoresStopAndTeardownErrorsButNotDeleteError(t *testing.T) {
	ops := newFakeHostOps()
	ops.stopProcessErr = errors.New("process already gone")
	ops.teardownNetErr = errors.New("network already gone")
	m := newTestManager(ops, &fakeFirecrackerClient{}, &fakeReadiness{})

	if err := m.DeleteVM(context.Background(), "inst-1"); err != nil {
		t.Fatalf("stop/teardown errors should be tolerated (idempotent best-effort), got: %v", err)
	}

	ops.deleteFilesErr = errors.New("disk error")
	if err := m.DeleteVM(context.Background(), "inst-2"); err == nil {
		t.Fatal("a real DeleteInstanceFiles failure should surface as an error")
	}
}

// --- Golden rootfs check-and-push (design doc §4.6, placement locality) ---

func TestHasRootfs_TrueAfterSave(t *testing.T) {
	ops := newFakeHostOps()
	m := newTestManager(ops, &fakeFirecrackerClient{}, &fakeReadiness{})

	if err := m.SaveRootfs(context.Background(), "/data/workloads/wl_1/rootfs.ext4", strings.NewReader("bytes")); err != nil {
		t.Fatal(err)
	}
	has, err := m.HasRootfs(context.Background(), "/data/workloads/wl_1/rootfs.ext4")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected HasRootfs to report true after SaveRootfs")
	}
}

func TestHasRootfs_FalseForUnknownPath(t *testing.T) {
	m := newTestManager(newFakeHostOps(), &fakeFirecrackerClient{}, &fakeReadiness{})
	has, err := m.HasRootfs(context.Background(), "/data/workloads/never-pushed/rootfs.ext4")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected HasRootfs to report false for a path never saved")
	}
}

// --- Live instance registry + Proxy (design doc §4.3) ---

func TestProxy_ResolvesInstanceIDToCurrentGuestEndpoint(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	proxy := &fakeGuestProxy{}
	m := newTestManagerWithProxy(ops, fc, &fakeReadiness{}, proxy)

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Proxy(context.Background(), "inst-1", &ProxyRequest{Method: "GET"}); err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if proxy.lastGuestIP != "172.16.0.2" || proxy.lastGuestPort != 8080 {
		t.Errorf("proxy forwarded to %s:%d, want the registered 172.16.0.2:8080", proxy.lastGuestIP, proxy.lastGuestPort)
	}
}

func TestProxy_UnknownInstanceReturnsErrInstanceNotRegistered(t *testing.T) {
	m := newTestManager(newFakeHostOps(), &fakeFirecrackerClient{}, &fakeReadiness{})
	_, err := m.Proxy(context.Background(), "never-booted", &ProxyRequest{Method: "GET"})
	if !errors.Is(err, ErrInstanceNotRegistered) {
		t.Fatalf("got %v, want ErrInstanceNotRegistered", err)
	}
}

func TestProxy_AfterSuspendReturnsErrInstanceNotRegistered(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	m := newTestManager(ops, fc, &fakeReadiness{})

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SuspendVM(context.Background(), "inst-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Proxy(context.Background(), "inst-1", &ProxyRequest{Method: "GET"}); !errors.Is(err, ErrInstanceNotRegistered) {
		t.Fatalf("got %v, want ErrInstanceNotRegistered — suspend must deregister the instance", err)
	}
}

// TestProxy_SurvivesFailedSuspendPause is the concurrency-correctness case
// the design doc calls out explicitly: a Pause failure leaves the instance
// running and routable (matching the Controller's own revert-to-RUNNING
// behavior on a failed suspend call, §4.2) — deregistering here would make
// a perfectly healthy instance unreachable for no reason.
func TestProxy_SurvivesFailedSuspendPause(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{pauseErr: errors.New("firecracker unreachable")}
	m := newTestManager(ops, fc, &fakeReadiness{})

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SuspendVM(context.Background(), "inst-1"); err == nil {
		t.Fatal("expected SuspendVM to fail")
	}
	if _, err := m.Proxy(context.Background(), "inst-1", &ProxyRequest{Method: "GET"}); err != nil {
		t.Fatalf("instance should still be registered after a failed Pause, got: %v", err)
	}
}

func TestProxy_AfterResumeUsesRefreshedEndpoint(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	proxy := &fakeGuestProxy{}
	m := newTestManagerWithProxy(ops, fc, &fakeReadiness{}, proxy)

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SuspendVM(context.Background(), "inst-1"); err != nil {
		t.Fatal(err)
	}
	ops.nextGuestIP = "172.16.0.9" // simulate a fresh IP on resume
	if _, err := m.ResumeVM(context.Background(), "inst-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Proxy(context.Background(), "inst-1", &ProxyRequest{Method: "GET"}); err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if proxy.lastGuestIP != "172.16.0.9" {
		t.Errorf("proxy forwarded to %s, want the refreshed 172.16.0.9 — a stale registry entry would misroute", proxy.lastGuestIP)
	}
}

func TestProxy_AfterDeleteReturnsErrInstanceNotRegistered(t *testing.T) {
	ops := newFakeHostOps()
	m := newTestManager(ops, &fakeFirecrackerClient{}, &fakeReadiness{})

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteVM(context.Background(), "inst-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Proxy(context.Background(), "inst-1", &ProxyRequest{Method: "GET"}); !errors.Is(err, ErrInstanceNotRegistered) {
		t.Fatalf("got %v, want ErrInstanceNotRegistered", err)
	}
}

func TestProxy_ForwardErrorPropagates(t *testing.T) {
	ops := newFakeHostOps()
	proxy := &fakeGuestProxy{forwardErr: errors.New("connection refused")}
	m := newTestManagerWithProxy(ops, &fakeFirecrackerClient{}, &fakeReadiness{}, proxy)

	if _, err := m.BootVM(context.Background(), BootRequest{InstanceID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Proxy(context.Background(), "inst-1", &ProxyRequest{Method: "GET"})
	if err == nil || errors.Is(err, ErrInstanceNotRegistered) {
		t.Fatalf("got %v, want the raw guest-unreachable error, distinct from ErrInstanceNotRegistered", err)
	}
}
