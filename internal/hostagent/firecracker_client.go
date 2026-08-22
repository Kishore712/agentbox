// Package hostagent implements the Host Agent (design doc §4.3): the Go
// binary running as a systemd service on each KVM-enabled GCE host, the
// only component that actually touches Firecracker.
package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// FirecrackerClient talks to a single Firecracker process's API, over its
// Unix domain socket (§2 "Firecracker" facts, §4.3 step 5). One instance
// per running microVM.
type FirecrackerClient interface {
	SetBootSource(ctx context.Context, kernelImagePath, bootArgs string) error
	SetDrive(ctx context.Context, driveID, pathOnHost string, isRootDevice, isReadOnly bool) error
	SetNetworkInterface(ctx context.Context, ifaceID, hostDevName string) error
	SetMachineConfig(ctx context.Context, vcpuCount, memSizeMiB int) error
	InstanceStart(ctx context.Context) error
	Pause(ctx context.Context) error
	CreateSnapshot(ctx context.Context, snapshotPath, memFilePath string) error
	LoadSnapshot(ctx context.Context, snapshotPath, memFilePath string, resumeVM bool) error
}

// UnixSocketFirecrackerClient is the real implementation — an HTTP client
// dialing a Unix domain socket, exactly the mechanism Phase 0/1's manual
// curl commands used. Only functional on Linux with a real `firecracker`
// process listening on socketPath; unit tests use a fake FirecrackerClient
// instead (see manager_test.go).
type UnixSocketFirecrackerClient struct {
	http *http.Client
}

func NewUnixSocketFirecrackerClient(socketPath string) *UnixSocketFirecrackerClient {
	return &UnixSocketFirecrackerClient{
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (c *UnixSocketFirecrackerClient) put(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: unexpected status %d", path, resp.StatusCode)
	}
	return nil
}

func (c *UnixSocketFirecrackerClient) patch(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "http://firecracker"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("PATCH %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PATCH %s: unexpected status %d", path, resp.StatusCode)
	}
	return nil
}

func (c *UnixSocketFirecrackerClient) SetBootSource(ctx context.Context, kernelImagePath, bootArgs string) error {
	return c.put(ctx, "/boot-source", map[string]string{
		"kernel_image_path": kernelImagePath,
		"boot_args":         bootArgs,
	})
}

func (c *UnixSocketFirecrackerClient) SetDrive(ctx context.Context, driveID, pathOnHost string, isRootDevice, isReadOnly bool) error {
	return c.put(ctx, "/drives/"+driveID, map[string]any{
		"drive_id":       driveID,
		"path_on_host":   pathOnHost,
		"is_root_device": isRootDevice,
		"is_read_only":   isReadOnly,
	})
}

func (c *UnixSocketFirecrackerClient) SetNetworkInterface(ctx context.Context, ifaceID, hostDevName string) error {
	return c.put(ctx, "/network-interfaces/"+ifaceID, map[string]string{
		"iface_id":      ifaceID,
		"host_dev_name": hostDevName,
	})
}

func (c *UnixSocketFirecrackerClient) SetMachineConfig(ctx context.Context, vcpuCount, memSizeMiB int) error {
	return c.put(ctx, "/machine-config", map[string]int{
		"vcpu_count":   vcpuCount,
		"mem_size_mib": memSizeMiB,
	})
}

func (c *UnixSocketFirecrackerClient) InstanceStart(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "InstanceStart"})
}

func (c *UnixSocketFirecrackerClient) Pause(ctx context.Context) error {
	return c.patch(ctx, "/vm", map[string]string{"state": "Paused"})
}

func (c *UnixSocketFirecrackerClient) CreateSnapshot(ctx context.Context, snapshotPath, memFilePath string) error {
	return c.put(ctx, "/snapshot/create", map[string]string{
		"snapshot_path": snapshotPath,
		"mem_file_path": memFilePath,
	})
}

func (c *UnixSocketFirecrackerClient) LoadSnapshot(ctx context.Context, snapshotPath, memFilePath string, resumeVM bool) error {
	return c.put(ctx, "/snapshot/load", map[string]any{
		"snapshot_path": snapshotPath,
		"mem_file_path": memFilePath,
		"resume_vm":     resumeVM,
	})
}
