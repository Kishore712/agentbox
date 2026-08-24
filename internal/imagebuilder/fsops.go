package imagebuilder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FilesystemOps abstracts the Linux-only ext4 packing operations (mount
// needs real privileges and a real Linux kernel) — not available on this
// dev machine, same class of constraint as Host Agent's HostOps. Interface
// so Builder's orchestration can be unit tested against a fake.
type FilesystemOps interface {
	// CreateSizedExt4 allocates and formats an empty ext4 file (§4.6 step 3).
	CreateSizedExt4(ctx context.Context, path string, sizeMiB int) error

	// MountExt4/UnmountExt4 bracket the extraction step.
	MountExt4(ctx context.Context, imagePath, mountPoint string) error
	UnmountExt4(ctx context.Context, mountPoint string) error

	// ExtractTar unpacks the exported Docker filesystem into the mounted
	// ext4 image (§4.6 step 3).
	ExtractTar(ctx context.Context, tarPath, destDir string) error

	// InjectInit writes the generated init script as /sbin/init inside the
	// mounted rootfs (§4.6 step 4).
	InjectInit(ctx context.Context, mountedRootDir string, script string) error
}

// LinuxFilesystemOps shells out to `dd`, `mkfs.ext4`, `mount`, `umount`,
// `tar`. Compiles everywhere, only functional on Linux with root/mount
// privileges — exercised only in the GCP validation phase.
type LinuxFilesystemOps struct{}

func (LinuxFilesystemOps) CreateSizedExt4(ctx context.Context, path string, sizeMiB int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// truncate, not `dd if=/dev/zero`: mkfs.ext4 doesn't need the backing
	// bytes actually written — a sparse file of the right size is enough,
	// and for a golden rootfs (often hundreds of MiB to a few GiB) a real
	// write is real latency on slower disks for no benefit. Same fix as
	// hostagent's CreateHomeVolume, same reason.
	if err := runCmd(ctx, "truncate", "-s", fmt.Sprintf("%dM", sizeMiB), path); err != nil {
		return err
	}
	return runCmd(ctx, "mkfs.ext4", "-q", path)
}

func (LinuxFilesystemOps) MountExt4(ctx context.Context, imagePath, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return err
	}
	// `mount -o loop` bundles "find a free loop device" and "attach + mount
	// it" into one call, and its free-device selection isn't safe against
	// concurrent use of the same loop-control (e.g. other loop users
	// elsewhere on a host whose /dev is shared into this container) — it can
	// report failure (exit 32, "already mounted or mount point busy") even
	// though the mount actually went through underneath it. `losetup -f
	// --show` is the hardened equivalent of that first step alone, so do the
	// attach and the mount as two explicit calls instead.
	loopDev, err := runCmdOutput(ctx, "losetup", "-f", "--show", imagePath)
	if err != nil {
		return fmt.Errorf("attach loop device: %w", err)
	}
	loopDev = strings.TrimSpace(loopDev)
	if err := runCmd(ctx, "mount", loopDev, mountPoint); err != nil {
		_ = runCmd(context.WithoutCancel(ctx), "losetup", "-d", loopDev)
		return err
	}
	return nil
}

func (LinuxFilesystemOps) UnmountExt4(ctx context.Context, mountPoint string) error {
	// Capture the backing loop device before unmounting (once unmounted,
	// nothing ties mountPoint back to it) so it can be explicitly detached —
	// `umount` alone doesn't guarantee autoclear runs synchronously, and a
	// lingering attachment is exactly what caused the mount race this
	// function's sibling (MountExt4) works around.
	loopDev, _ := runCmdOutput(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", mountPoint)
	loopDev = strings.TrimSpace(loopDev)
	if err := runCmd(ctx, "umount", mountPoint); err != nil {
		return err
	}
	if loopDev != "" {
		_ = runCmd(ctx, "losetup", "-d", loopDev)
	}
	return nil
}

func (LinuxFilesystemOps) ExtractTar(ctx context.Context, tarPath, destDir string) error {
	return runCmd(ctx, "tar", "-xf", tarPath, "-C", destDir)
}

func (LinuxFilesystemOps) InjectInit(ctx context.Context, mountedRootDir string, script string) error {
	initPath := filepath.Join(mountedRootDir, "sbin", "init")
	if err := os.MkdirAll(filepath.Dir(initPath), 0o755); err != nil {
		return err
	}
	// Base images commonly ship /sbin/init as a symlink (Alpine/BusyBox:
	// /sbin/init -> /bin/busybox) — os.WriteFile follows symlinks like any
	// open(2)-based write, and since that target is an *absolute* path, a
	// non-chrooted process resolves it against its own real root, not the
	// mounted rootfs. Confirmed against a real build: this silently escaped
	// the mount entirely and overwrote the host's own /bin/busybox instead
	// of the guest's. Removing whatever's at initPath first — symlink or
	// not — guarantees the write below creates a fresh regular file inside
	// the mount, never follows a pre-existing symlink out of it.
	if err := os.Remove(initPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing entry at %s before injecting init: %w", initPath, err)
	}
	return os.WriteFile(initPath, []byte(script), 0o755)
}
