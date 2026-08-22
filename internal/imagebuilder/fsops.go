package imagebuilder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	if err := runCmd(ctx, "dd", "if=/dev/zero", "of="+path, "bs=1M", fmt.Sprintf("count=%d", sizeMiB)); err != nil {
		return err
	}
	return runCmd(ctx, "mkfs.ext4", "-q", path)
}

func (LinuxFilesystemOps) MountExt4(ctx context.Context, imagePath, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return err
	}
	return runCmd(ctx, "mount", "-o", "loop", imagePath, mountPoint)
}

func (LinuxFilesystemOps) UnmountExt4(ctx context.Context, mountPoint string) error {
	return runCmd(ctx, "umount", mountPoint)
}

func (LinuxFilesystemOps) ExtractTar(ctx context.Context, tarPath, destDir string) error {
	return runCmd(ctx, "tar", "-xf", tarPath, "-C", destDir)
}

func (LinuxFilesystemOps) InjectInit(ctx context.Context, mountedRootDir string, script string) error {
	initPath := filepath.Join(mountedRootDir, "sbin", "init")
	if err := os.MkdirAll(filepath.Dir(initPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(initPath, []byte(script), 0o755)
}
