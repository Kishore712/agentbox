// Package imagebuilder implements the Image Builder (design doc §4.6): the
// Docker image → Firecracker-compatible golden rootfs pipeline, triggered
// once per workload on registration, never per instance.
package imagebuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DockerOps abstracts the Docker-specific operations — needs a real Docker
// daemon, not available on this dev machine. Interface so Builder's
// orchestration can be unit tested against a fake; CLIDockerOps is the real
// implementation, exercised only in the GCP validation phase.
type DockerOps interface {
	// PullImage fetches the customer's image (§4.6 step 1).
	PullImage(ctx context.Context, imageRef string) error

	// ImageSizeMiB inspects the image's size, used to size the ext4 file —
	// resolves the "auto-detect vs fixed platform max" open question from
	// early design discussion by doing both: detect, then clamp to a
	// platform max (see Builder.sizeForImage).
	ImageSizeMiB(ctx context.Context, imageRef string) (int, error)

	// ImageEntrypoint returns the exec argv the guest's init should run —
	// Entrypoint+Cmd per Docker's own semantics (§4.6 step 4's "start the
	// customer's entrypoint").
	ImageEntrypoint(ctx context.Context, imageRef string) ([]string, error)

	// ExportImageFilesystem implements §4.6 step 2: docker create -> export
	// -> flat tarball, container cleaned up after.
	ExportImageFilesystem(ctx context.Context, imageRef, destTarPath string) error
}

// CLIDockerOps shells out to the `docker` CLI. Compiles everywhere, only
// functional where Docker is actually installed and running — not this
// dev machine (confirmed: no `docker` binary present).
type CLIDockerOps struct{}

func (CLIDockerOps) PullImage(ctx context.Context, imageRef string) error {
	return runCmd(ctx, "docker", "pull", imageRef)
}

func (CLIDockerOps) ImageSizeMiB(ctx context.Context, imageRef string) (int, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format={{.Size}}", imageRef).Output()
	if err != nil {
		return 0, fmt.Errorf("docker inspect size: %w", err)
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse image size: %w", err)
	}
	return int(bytes / (1024 * 1024)), nil
}

func (CLIDockerOps) ImageEntrypoint(ctx context.Context, imageRef string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format={{json .Config.Entrypoint}}|{{json .Config.Cmd}}", imageRef).Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect entrypoint: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("unexpected docker inspect output: %q", out)
	}
	var entrypoint, cmd []string
	if err := json.Unmarshal([]byte(parts[0]), &entrypoint); err != nil {
		return nil, fmt.Errorf("parse entrypoint: %w", err)
	}
	if err := json.Unmarshal([]byte(parts[1]), &cmd); err != nil {
		return nil, fmt.Errorf("parse cmd: %w", err)
	}
	// Docker semantics: Entrypoint+Cmd if both set, Cmd alone if no
	// entrypoint, Entrypoint alone if no cmd.
	argv := append(append([]string{}, entrypoint...), cmd...)
	if len(argv) == 0 {
		return nil, fmt.Errorf("image %s has neither ENTRYPOINT nor CMD", imageRef)
	}
	return argv, nil
}

func (CLIDockerOps) ExportImageFilesystem(ctx context.Context, imageRef, destTarPath string) error {
	out, err := exec.CommandContext(ctx, "docker", "create", imageRef).Output()
	if err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	containerID := strings.TrimSpace(string(out))
	defer func() { _ = runCmd(context.WithoutCancel(ctx), "docker", "rm", containerID) }()

	if err := os.MkdirAll(filepath.Dir(destTarPath), 0o755); err != nil {
		return fmt.Errorf("create destination dir: %w", err)
	}
	f, err := os.Create(destTarPath)
	if err != nil {
		return fmt.Errorf("create tar file: %w", err)
	}
	defer f.Close()

	cmd := exec.CommandContext(ctx, "docker", "export", containerID)
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker export: %w", err)
	}
	return nil
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w (output: %s)", name, args, err, string(out))
	}
	return nil
}
