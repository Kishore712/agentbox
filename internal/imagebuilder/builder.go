package imagebuilder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Config controls rootfs sizing (§4.6's "image size: auto-detect vs fixed
// platform max" open question — resolved here by doing both: detect the
// actual image size, apply a margin for runtime writes, then clamp to a
// platform maximum).
type Config struct {
	DataDir           string  // e.g. "/data" — golden rootfs lands at {DataDir}/workloads/{workloadID}/rootfs.ext4
	MinSizeMiB        int     // floor, regardless of how small the image is
	MaxSizeMiB        int     // platform cap, regardless of how large the image is
	SizeMarginPercent float64 // e.g. 50 = ext4 file is 1.5x the image's reported size, for runtime writes/package caches/etc.
}

func DefaultConfig() Config {
	return Config{DataDir: "/data", MinSizeMiB: 512, MaxSizeMiB: 4096, SizeMarginPercent: 50}
}

// Builder implements the controller.ImageBuilder interface — this is what
// gets wired into the Controller in place of the notImplementedImageBuilder
// stub. Composed from DockerOps and FilesystemOps so the orchestration
// (this file) is fully unit-testable against fakes, with the real
// Docker/Linux-shelling implementations exercised only where Docker and
// real mount privileges actually exist.
type Builder struct {
	docker DockerOps
	fs     FilesystemOps
	cfg    Config
}

func NewBuilder(docker DockerOps, fs FilesystemOps, cfg Config) *Builder {
	return &Builder{docker: docker, fs: fs, cfg: cfg}
}

// Build implements §4.6's full pipeline, steps 1-5, returning the golden
// rootfs path the Controller will store as the workload's rootfs_ref.
func (b *Builder) Build(ctx context.Context, workloadID, imageRef string) (string, error) {
	// Deliberately no direct filesystem access here (no os.MkdirAll etc.) —
	// every real filesystem touch goes through DockerOps/FilesystemOps, so
	// this orchestration stays testable against fakes with no real disk
	// access at all. Each real implementation is responsible for creating
	// its own parent directories as needed.
	workloadDir := filepath.Join(b.cfg.DataDir, "workloads", workloadID)

	// Step 1: pull.
	if err := b.docker.PullImage(ctx, imageRef); err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}

	entrypoint, err := b.docker.ImageEntrypoint(ctx, imageRef)
	if err != nil {
		return "", fmt.Errorf("resolve entrypoint: %w", err)
	}

	sizeMiB, err := b.sizeForImage(ctx, imageRef)
	if err != nil {
		return "", fmt.Errorf("determine rootfs size: %w", err)
	}

	// Step 2: export to a flat tarball.
	tarPath := filepath.Join(workloadDir, "image.tar")
	if err := b.docker.ExportImageFilesystem(ctx, imageRef, tarPath); err != nil {
		return "", fmt.Errorf("export image filesystem: %w", err)
	}
	defer os.Remove(tarPath)

	// Step 3: create + mount the ext4 image, extract the filesystem into it.
	rootfsPath := filepath.Join(workloadDir, "rootfs.ext4")
	if err := b.fs.CreateSizedExt4(ctx, rootfsPath, sizeMiB); err != nil {
		return "", fmt.Errorf("create ext4 image: %w", err)
	}
	mountPoint := filepath.Join(workloadDir, "mnt")
	if err := b.fs.MountExt4(ctx, rootfsPath, mountPoint); err != nil {
		return "", fmt.Errorf("mount ext4 image: %w", err)
	}
	// Best-effort unmount on every exit path from here on — the mount must
	// not be left dangling even if a later step fails.
	defer func() { _ = b.fs.UnmountExt4(context.WithoutCancel(ctx), mountPoint) }()

	if err := b.fs.ExtractTar(ctx, tarPath, mountPoint); err != nil {
		return "", fmt.Errorf("extract image filesystem: %w", err)
	}

	// Step 4: inject the init script.
	script, err := BuildInitScript(entrypoint)
	if err != nil {
		return "", fmt.Errorf("build init script: %w", err)
	}
	if err := b.fs.InjectInit(ctx, mountPoint, script); err != nil {
		return "", fmt.Errorf("inject init script: %w", err)
	}

	// Step 5: the golden rootfs is already stored at its final local path —
	// no separate distribution step for the single-host prototype (§4.6's
	// placement-locality note).
	return rootfsPath, nil
}

// sizeForImage resolves the rootfs size: detect the actual image size, add
// a margin for runtime writes, then clamp to [MinSizeMiB, MaxSizeMiB].
func (b *Builder) sizeForImage(ctx context.Context, imageRef string) (int, error) {
	imageSizeMiB, err := b.docker.ImageSizeMiB(ctx, imageRef)
	if err != nil {
		return 0, err
	}
	sized := int(float64(imageSizeMiB) * (1 + b.cfg.SizeMarginPercent/100))
	if sized < b.cfg.MinSizeMiB {
		return b.cfg.MinSizeMiB, nil
	}
	if sized > b.cfg.MaxSizeMiB {
		return b.cfg.MaxSizeMiB, nil
	}
	return sized, nil
}
