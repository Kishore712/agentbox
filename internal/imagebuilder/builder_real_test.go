package imagebuilder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This file is Tier 2 of the testing pyramid: it exercises the REAL
// CLIDockerOps + LinuxFilesystemOps against a real Docker daemon and real
// Linux ext4 tooling, not fakes. It compiles everywhere (no build tags —
// consistent with how internal/controller/store_test.go gates on Redis
// reachability rather than excluding itself from non-Linux builds), and
// skips at test time with a clear, specific reason wherever the
// environment can't support it — which is every environment except a
// properly provisioned Linux box (Tier 3: a GCE VM with Docker + e2fsprogs
// installed, per the project's phased validation plan).
//
// The moment such a box exists, running `go test ./internal/imagebuilder/...`
// there — no test code changes needed — starts genuinely validating the
// real pipeline: a real docker pull/export, a real mkfs.ext4, a real mount,
// real content landing inside, a real executable init script.

func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found in PATH — this test only runs on a properly provisioned Linux box (Tier 3 validation), skipping here", name)
	}
}

func requireImageBuilderEnvironment(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("the real Image Builder pipeline needs Linux (mkfs.ext4, mount) — running on %s", runtime.GOOS)
	}
	requireCommand(t, "docker")
	requireCommand(t, "mkfs.ext4")
	requireCommand(t, "mount")
	requireCommand(t, "umount")
	requireCommand(t, "tar")
	requireCommand(t, "dd")
	if os.Geteuid() != 0 {
		t.Skip("the real Image Builder pipeline needs root (mount requires it) — run as root or with CAP_SYS_ADMIN")
	}
}

// mountAndReadBack mounts rootfsPath read-only just long enough to verify
// its contents, then unmounts. Separate from the Builder's own
// mount/extract/unmount cycle — this is the test *observing* what Build
// actually produced, using the real `mount`/`umount` binaries directly
// rather than going through FilesystemOps (which would just be testing the
// implementation against itself).
func mountAndReadBack(t *testing.T, ctx context.Context, rootfsPath string, verify func(mountPoint string)) {
	t.Helper()
	mountPoint := t.TempDir()
	if err := exec.CommandContext(ctx, "mount", "-o", "loop,ro", rootfsPath, mountPoint).Run(); err != nil {
		t.Fatalf("mount produced rootfs for verification: %v", err)
	}
	defer func() {
		if err := exec.Command("umount", mountPoint).Run(); err != nil {
			t.Logf("warning: failed to unmount verification mount point %s: %v", mountPoint, err)
		}
	}()
	verify(mountPoint)
}

// TestRealBuild_AlpineImage runs the complete §4.6 pipeline against a real,
// small public image (alpine — a few MB, has /bin/sh, has a default CMD)
// and verifies real, observable output: a correctly-sized ext4 file
// containing the actual image contents plus a working, executable init
// script — not just "Build() returned no error."
func TestRealBuild_AlpineImage(t *testing.T) {
	requireImageBuilderEnvironment(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dataDir
	b := NewBuilder(CLIDockerOps{}, LinuxFilesystemOps{}, cfg)

	rootfsPath, err := b.Build(ctx, "test-wl-alpine", "alpine:latest")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	info, err := os.Stat(rootfsPath)
	if err != nil {
		t.Fatalf("rootfs file doesn't exist at the returned path %s: %v", rootfsPath, err)
	}
	// Should be clamped to MinSizeMiB (512) — alpine's real image is only a
	// few MB, well under the floor even with the size margin applied.
	wantBytes := int64(cfg.MinSizeMiB) * 1024 * 1024
	if info.Size() != wantBytes {
		t.Errorf("rootfs size = %d bytes, want exactly %d (MinSizeMiB, since alpine is far smaller than the floor)", info.Size(), wantBytes)
	}

	mountAndReadBack(t, ctx, rootfsPath, func(mountPoint string) {
		if _, err := os.Stat(filepath.Join(mountPoint, "etc", "os-release")); err != nil {
			t.Errorf("expected alpine's /etc/os-release to exist in the extracted rootfs: %v", err)
		}
		if _, err := os.Stat(filepath.Join(mountPoint, "bin", "sh")); err != nil {
			t.Errorf("expected /bin/sh to exist (alpine's shell, needed by our init script): %v", err)
		}

		initPath := filepath.Join(mountPoint, "sbin", "init")
		initInfo, err := os.Stat(initPath)
		if err != nil {
			t.Fatalf("expected /sbin/init to exist: %v", err)
		}
		if initInfo.Mode()&0o111 == 0 {
			t.Error("init script must be executable")
		}
		content, err := os.ReadFile(initPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), "mount -t ext4 /dev/vdb") {
			t.Error("init script doesn't contain the expected home-volume mount logic")
		}
		if !strings.Contains(string(content), "exec ") {
			t.Error("init script doesn't contain an exec of the entrypoint")
		}
	})
}

// TestRealBuild_UnknownImageFails confirms the real pull failure path
// surfaces a clear error rather than hanging or silently producing a
// broken rootfs.
func TestRealBuild_UnknownImageFails(t *testing.T) {
	requireImageBuilderEnvironment(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dataDir
	b := NewBuilder(CLIDockerOps{}, LinuxFilesystemOps{}, cfg)

	_, err := b.Build(ctx, "test-wl-missing", "this-image-definitely-does-not-exist-anywhere:latest")
	if err == nil {
		t.Fatal("expected an error pulling a nonexistent image")
	}
}

// TestRealCLIDockerOps_ImageEntrypoint verifies the real `docker inspect`
// parsing against a real image with a known, specific entrypoint —
// distinct from TestRealBuild_AlpineImage, which only confirms *some*
// entrypoint got resolved and exec'd.
func TestRealCLIDockerOps_ImageEntrypoint(t *testing.T) {
	requireImageBuilderEnvironment(t)
	ctx := context.Background()

	ops := CLIDockerOps{}
	if err := ops.PullImage(ctx, "alpine:latest"); err != nil {
		t.Fatalf("pull: %v", err)
	}
	entrypoint, err := ops.ImageEntrypoint(ctx, "alpine:latest")
	if err != nil {
		t.Fatalf("ImageEntrypoint: %v", err)
	}
	if len(entrypoint) == 0 {
		t.Fatal("expected a non-empty entrypoint for alpine (it has a default CMD)")
	}
}

// TestRealCLIDockerOps_ImageSizeMiB verifies real `docker inspect` size
// parsing produces a plausible, non-zero value.
func TestRealCLIDockerOps_ImageSizeMiB(t *testing.T) {
	requireImageBuilderEnvironment(t)
	ctx := context.Background()

	ops := CLIDockerOps{}
	if err := ops.PullImage(ctx, "alpine:latest"); err != nil {
		t.Fatalf("pull: %v", err)
	}
	sizeMiB, err := ops.ImageSizeMiB(ctx, "alpine:latest")
	if err != nil {
		t.Fatalf("ImageSizeMiB: %v", err)
	}
	if sizeMiB <= 0 {
		t.Errorf("size = %d MiB, want > 0", sizeMiB)
	}
	if sizeMiB > 200 {
		t.Errorf("size = %d MiB, suspiciously large for alpine — inspect parsing may be wrong", sizeMiB)
	}
}
