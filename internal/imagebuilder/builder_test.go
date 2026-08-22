package imagebuilder

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// --- Fakes ---

type fakeDockerOps struct {
	mu sync.Mutex

	pullErr       error
	sizeMiB       int
	sizeErr       error
	entrypoint    []string
	entrypointErr error
	exportErr     error
	pulledImages  []string
	exportedPaths []string
}

func newFakeDockerOps() *fakeDockerOps {
	return &fakeDockerOps{sizeMiB: 200, entrypoint: []string{"python3", "app.py"}}
}

func (f *fakeDockerOps) PullImage(ctx context.Context, imageRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulledImages = append(f.pulledImages, imageRef)
	return f.pullErr
}
func (f *fakeDockerOps) ImageSizeMiB(ctx context.Context, imageRef string) (int, error) {
	return f.sizeMiB, f.sizeErr
}
func (f *fakeDockerOps) ImageEntrypoint(ctx context.Context, imageRef string) ([]string, error) {
	return f.entrypoint, f.entrypointErr
}
func (f *fakeDockerOps) ExportImageFilesystem(ctx context.Context, imageRef, destTarPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exportedPaths = append(f.exportedPaths, destTarPath)
	return f.exportErr
}

type fakeFilesystemOps struct {
	mu sync.Mutex

	createErr    error
	mountErr     error
	unmountErr   error
	extractErr   error
	injectErr    error
	createdSizes map[string]int
	mounted      []string
	unmounted    []string
	extracted    []string
	injectedInit map[string]string
}

func newFakeFilesystemOps() *fakeFilesystemOps {
	return &fakeFilesystemOps{createdSizes: map[string]int{}, injectedInit: map[string]string{}}
}

func (f *fakeFilesystemOps) CreateSizedExt4(ctx context.Context, path string, sizeMiB int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdSizes[path] = sizeMiB
	return f.createErr
}
func (f *fakeFilesystemOps) MountExt4(ctx context.Context, imagePath, mountPoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted = append(f.mounted, mountPoint)
	return f.mountErr
}
func (f *fakeFilesystemOps) UnmountExt4(ctx context.Context, mountPoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmounted = append(f.unmounted, mountPoint)
	return f.unmountErr
}
func (f *fakeFilesystemOps) ExtractTar(ctx context.Context, tarPath, destDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extracted = append(f.extracted, tarPath)
	return f.extractErr
}
func (f *fakeFilesystemOps) InjectInit(ctx context.Context, mountedRootDir string, script string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injectedInit[mountedRootDir] = script
	return f.injectErr
}

func newTestBuilder(docker DockerOps, fs FilesystemOps, dataDir string) *Builder {
	cfg := DefaultConfig()
	cfg.DataDir = dataDir
	return NewBuilder(docker, fs, cfg)
}

// --- Build: happy path and step ordering ---

func TestBuild_Success(t *testing.T) {
	docker := newFakeDockerOps()
	fs := newFakeFilesystemOps()
	b := newTestBuilder(docker, fs, "/data")

	rootfsRef, err := b.Build(context.Background(), "wl_1", "example/x:tag")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rootfsRef != "/data/workloads/wl_1/rootfs.ext4" {
		t.Errorf("rootfsRef = %q", rootfsRef)
	}
	if len(docker.pulledImages) != 1 || docker.pulledImages[0] != "example/x:tag" {
		t.Errorf("pulledImages = %v", docker.pulledImages)
	}
	if len(fs.mounted) != 1 || len(fs.unmounted) != 1 || fs.mounted[0] != fs.unmounted[0] {
		t.Errorf("expected exactly one matched mount/unmount pair, got mounted=%v unmounted=%v", fs.mounted, fs.unmounted)
	}
	if len(fs.extracted) != 1 {
		t.Errorf("expected exactly one tar extraction, got %v", fs.extracted)
	}
	script, ok := fs.injectedInit[fs.mounted[0]]
	if !ok {
		t.Fatal("expected an init script to be injected into the mounted root")
	}
	if !strings.Contains(script, "exec 'python3' 'app.py'") {
		t.Errorf("injected script doesn't contain the expected entrypoint: %s", script)
	}
}

func TestBuild_MountAlwaysUnmountedEvenOnLaterFailure(t *testing.T) {
	docker := newFakeDockerOps()
	fs := newFakeFilesystemOps()
	fs.extractErr = errors.New("corrupt tarball")
	b := newTestBuilder(docker, fs, "/data")

	_, err := b.Build(context.Background(), "wl_1", "example/x:tag")
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(fs.mounted) != 1 || len(fs.unmounted) != 1 {
		t.Fatalf("mount must be unwound even after a later step fails: mounted=%v unmounted=%v", fs.mounted, fs.unmounted)
	}
}

func TestBuild_PullFailure(t *testing.T) {
	docker := newFakeDockerOps()
	docker.pullErr = errors.New("registry unreachable")
	fs := newFakeFilesystemOps()
	b := newTestBuilder(docker, fs, "/data")

	_, err := b.Build(context.Background(), "wl_1", "example/x:tag")
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(fs.mounted) != 0 {
		t.Error("should not attempt any filesystem work if the pull failed")
	}
}

func TestBuild_NoEntrypointFails(t *testing.T) {
	docker := newFakeDockerOps()
	docker.entrypointErr = errors.New("image has neither ENTRYPOINT nor CMD")
	fs := newFakeFilesystemOps()
	b := newTestBuilder(docker, fs, "/data")

	_, err := b.Build(context.Background(), "wl_1", "example/x:tag")
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(fs.mounted) != 0 {
		t.Error("should resolve the entrypoint before doing any filesystem work")
	}
}

// --- sizeForImage: the resolved "auto-detect vs fixed max" decision ---

func TestSizeForImage_UsesDetectedSizePlusMargin(t *testing.T) {
	docker := newFakeDockerOps()
	docker.sizeMiB = 200
	fs := newFakeFilesystemOps()
	b := newTestBuilder(docker, fs, "/data") // default margin 50%, min 512, max 4096

	if _, err := b.Build(context.Background(), "wl_1", "example/x:tag"); err != nil {
		t.Fatal(err)
	}
	// 200 * 1.5 = 300, below MinSizeMiB (512) -> clamped up to 512.
	got := fs.createdSizes["/data/workloads/wl_1/rootfs.ext4"]
	if got != 512 {
		t.Errorf("size = %d, want 512 (clamped to the floor)", got)
	}
}

func TestSizeForImage_ClampedToMax(t *testing.T) {
	docker := newFakeDockerOps()
	docker.sizeMiB = 10000 // huge image
	fs := newFakeFilesystemOps()
	b := newTestBuilder(docker, fs, "/data")

	if _, err := b.Build(context.Background(), "wl_1", "example/x:tag"); err != nil {
		t.Fatal(err)
	}
	got := fs.createdSizes["/data/workloads/wl_1/rootfs.ext4"]
	if got != 4096 {
		t.Errorf("size = %d, want 4096 (clamped to the platform max)", got)
	}
}

func TestSizeForImage_WithinRangeUsesDetectedPlusMargin(t *testing.T) {
	docker := newFakeDockerOps()
	docker.sizeMiB = 1000 // 1000 * 1.5 = 1500, within [512, 4096]
	fs := newFakeFilesystemOps()
	b := newTestBuilder(docker, fs, "/data")

	if _, err := b.Build(context.Background(), "wl_1", "example/x:tag"); err != nil {
		t.Fatal(err)
	}
	got := fs.createdSizes["/data/workloads/wl_1/rootfs.ext4"]
	if got != 1500 {
		t.Errorf("size = %d, want 1500", got)
	}
}
