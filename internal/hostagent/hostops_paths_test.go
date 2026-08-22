package hostagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise LinuxHostOps' path-resolution and file-placement
// logic for real — MkdirAll and file copy aren't Linux-specific, unlike
// the shelled-out dd/mkfs.ext4/mount/ip/iptables calls elsewhere in this
// file, so this genuinely verifies jailed-vs-unjailed behavior against a
// real filesystem, not just mocks.

func newTestOps(t *testing.T, jailed bool) (*LinuxHostOps, string) {
	t.Helper()
	dataDir := t.TempDir()
	subnets, err := NewSubnetAllocator("172.16.0.0/16", 10)
	if err != nil {
		t.Fatal(err)
	}
	ops := &LinuxHostOps{DataDir: dataDir, FirecrackerBinary: "/usr/local/bin/firecracker", Subnets: subnets}
	if jailed {
		chrootBase := t.TempDir()
		ops.Jailer = &JailerConfig{Enabled: true, JailerBinary: "/usr/local/bin/jailer", ChrootBaseDir: chrootBase, UID: 1000, GID: 1000}
		return ops, chrootBase
	}
	return ops, dataDir
}

func TestInstanceRootDir_Unjailed(t *testing.T) {
	ops, dataDir := newTestOps(t, false)
	got := ops.instanceRootDir("inst-1")
	want := filepath.Join(dataDir, "instances", "inst-1")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestInstanceRootDir_Jailed(t *testing.T) {
	ops, chrootBase := newTestOps(t, true)
	got := ops.instanceRootDir("inst-1")
	want := filepath.Join(chrootBase, "firecracker", "inst-1", "root")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApiPath_UnjailedReturnsRealHostPath(t *testing.T) {
	ops, _ := newTestOps(t, false)
	got := ops.apiPath("inst-1", "rootfs.ext4")
	want := filepath.Join(ops.instanceRootDir("inst-1"), "rootfs.ext4")
	if got != want {
		t.Errorf("got %q, want the real host path %q", got, want)
	}
}

func TestApiPath_JailedReturnsChrootRelativePath(t *testing.T) {
	ops, _ := newTestOps(t, true)
	got := ops.apiPath("inst-1", "rootfs.ext4")
	if got != "/rootfs.ext4" {
		t.Errorf("got %q, want /rootfs.ext4 — Firecracker's own view of / IS the chroot root, so this must not be the host path", got)
	}
}

func TestCopyRootfs_Unjailed_PlacesFileAtHostPathAndReturnsIt(t *testing.T) {
	ops, dataDir := newTestOps(t, false)
	golden := filepath.Join(t.TempDir(), "golden-rootfs.ext4")
	if err := os.WriteFile(golden, []byte("fake ext4 content"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ops.CopyRootfs(context.Background(), golden, "inst-1")
	if err != nil {
		t.Fatalf("CopyRootfs: %v", err)
	}
	wantPath := filepath.Join(dataDir, "instances", "inst-1", "rootfs.ext4")
	if got != wantPath {
		t.Errorf("returned path = %q, want %q", got, wantPath)
	}
	content, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("file wasn't actually placed at the returned path: %v", err)
	}
	if string(content) != "fake ext4 content" {
		t.Errorf("copied content = %q", content)
	}
}

func TestCopyRootfs_Jailed_PlacesFileInChrootReturnsChrootRelativePath(t *testing.T) {
	ops, chrootBase := newTestOps(t, true)
	golden := filepath.Join(t.TempDir(), "golden-rootfs.ext4")
	if err := os.WriteFile(golden, []byte("fake ext4 content"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ops.CopyRootfs(context.Background(), golden, "inst-1")
	if err != nil {
		t.Fatalf("CopyRootfs: %v", err)
	}
	if got != "/rootfs.ext4" {
		t.Errorf("returned path = %q, want the chroot-relative /rootfs.ext4", got)
	}
	// The file must physically exist inside the chroot's host-visible
	// location, even though the API-facing path is chroot-relative.
	hostPath := filepath.Join(chrootBase, "firecracker", "inst-1", "root", "rootfs.ext4")
	content, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("file wasn't placed inside the chroot at %s: %v", hostPath, err)
	}
	if string(content) != "fake ext4 content" {
		t.Errorf("copied content = %q", content)
	}
}

func TestPrepareKernel_Unjailed_ReturnsGoldenPathDirectly(t *testing.T) {
	ops, _ := newTestOps(t, false)
	got, err := ops.PrepareKernel(context.Background(), "/data/vmlinux", "inst-1")
	if err != nil {
		t.Fatalf("PrepareKernel: %v", err)
	}
	if got != "/data/vmlinux" {
		t.Errorf("got %q, want the golden path unchanged — unjailed firecracker reads it directly, no per-instance copy needed", got)
	}
}

func TestPrepareKernel_Jailed_CopiesIntoChroot(t *testing.T) {
	ops, chrootBase := newTestOps(t, true)
	golden := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(golden, []byte("fake kernel"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ops.PrepareKernel(context.Background(), golden, "inst-1")
	if err != nil {
		t.Fatalf("PrepareKernel: %v", err)
	}
	if got != "/vmlinux" {
		t.Errorf("got %q, want the chroot-relative /vmlinux", got)
	}
	hostPath := filepath.Join(chrootBase, "firecracker", "inst-1", "root", "vmlinux")
	content, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("kernel wasn't copied into the chroot at %s: %v", hostPath, err)
	}
	if string(content) != "fake kernel" {
		t.Errorf("copied content = %q", content)
	}
}

func TestSocketPath_AlwaysHostVisibleEvenWhenJailed(t *testing.T) {
	unjailed, dataDir := newTestOps(t, false)
	jailed, chrootBase := newTestOps(t, true)

	gotUnjailed := unjailed.SocketPath("inst-1")
	wantUnjailed := filepath.Join(dataDir, "instances", "inst-1", "api.sock")
	if gotUnjailed != wantUnjailed {
		t.Errorf("unjailed SocketPath = %q, want %q", gotUnjailed, wantUnjailed)
	}

	gotJailed := jailed.SocketPath("inst-1")
	wantJailed := filepath.Join(chrootBase, "firecracker", "inst-1", "root", "api.sock")
	if gotJailed != wantJailed {
		t.Errorf("jailed SocketPath = %q, want %q (must be host-visible, not chroot-relative — Host Agent dials in from outside)", gotJailed, wantJailed)
	}
}

func TestSnapshotPaths_KeepsSnapshotSubdirectoryBothModes(t *testing.T) {
	unjailed, _ := newTestOps(t, false)
	vmstate, memFile := unjailed.SnapshotPaths("inst-1")
	if filepath.Base(filepath.Dir(vmstate)) != "snapshot" || filepath.Base(filepath.Dir(memFile)) != "snapshot" {
		t.Errorf("unjailed snapshot paths should stay under a snapshot/ subdir (§3.1/§4.7), got %q, %q", vmstate, memFile)
	}

	jailed, _ := newTestOps(t, true)
	jVmstate, jMemFile := jailed.SnapshotPaths("inst-1")
	if jVmstate != "/snapshot/vmstate" || jMemFile != "/snapshot/mem_file" {
		t.Errorf("jailed snapshot paths = (%q, %q), want (/snapshot/vmstate, /snapshot/mem_file)", jVmstate, jMemFile)
	}
}
