package hostagent

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
)

var testSocketCounter atomic.Int64

// newTestFirecrackerServer starts an HTTP server listening on a real Unix
// domain socket, and returns a client dialing it — exercises
// UnixSocketFirecrackerClient's actual transport, not just its request
// marshaling, without needing a real `firecracker` binary. Uses a short,
// counter-suffixed path directly under /tmp rather than t.TempDir(), which
// embeds the full (long) test name and blows past macOS's ~104-byte
// sun_path limit.
func newTestFirecrackerServer(t *testing.T, handler http.HandlerFunc) *UnixSocketFirecrackerClient {
	t.Helper()
	sockPath := fmt.Sprintf("/tmp/fc-test-%d-%d.socket", os.Getpid(), testSocketCounter.Add(1))
	_ = os.Remove(sockPath)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(sockPath)
	})
	return NewUnixSocketFirecrackerClient(sockPath)
}

func TestUnixSocketFirecrackerClient_SetBootSource(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	c := newTestFirecrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.SetBootSource(t.Context(), "/data/vmlinux", "console=ttyS0"); err != nil {
		t.Fatalf("SetBootSource: %v", err)
	}
	if gotPath != "/boot-source" {
		t.Errorf("path = %q, want /boot-source", gotPath)
	}
	if gotBody["kernel_image_path"] != "/data/vmlinux" || gotBody["boot_args"] != "console=ttyS0" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestUnixSocketFirecrackerClient_SetDrive(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := newTestFirecrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.SetDrive(t.Context(), "rootfs", "/data/instances/x/rootfs.ext4", true, false); err != nil {
		t.Fatalf("SetDrive: %v", err)
	}
	if gotPath != "/drives/rootfs" {
		t.Errorf("path = %q, want /drives/rootfs", gotPath)
	}
	if gotBody["is_root_device"] != true || gotBody["is_read_only"] != false {
		t.Errorf("body = %v", gotBody)
	}
}

func TestUnixSocketFirecrackerClient_InstanceStart(t *testing.T) {
	var gotBody map[string]string
	c := newTestFirecrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/actions" {
			t.Errorf("path = %q, want /actions", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.InstanceStart(t.Context()); err != nil {
		t.Fatalf("InstanceStart: %v", err)
	}
	if gotBody["action_type"] != "InstanceStart" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestUnixSocketFirecrackerClient_PauseUsesPATCH(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestFirecrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/vm" {
		t.Errorf("got %s %s, want PATCH /vm", gotMethod, gotPath)
	}
}

func TestUnixSocketFirecrackerClient_CreateAndLoadSnapshot(t *testing.T) {
	var paths []string
	var loadBody map[string]any
	c := newTestFirecrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/snapshot/load" {
			json.NewDecoder(r.Body).Decode(&loadBody)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.CreateSnapshot(t.Context(), "/snap/vmstate", "/snap/mem"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := c.LoadSnapshot(t.Context(), "/snap/vmstate", "/snap/mem", true); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/snapshot/create" || paths[1] != "/snapshot/load" {
		t.Errorf("paths = %v", paths)
	}
	if loadBody["resume_vm"] != true {
		t.Errorf("resume_vm = %v, want true", loadBody["resume_vm"])
	}
}

func TestUnixSocketFirecrackerClient_ErrorStatusPropagates(t *testing.T) {
	c := newTestFirecrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := c.SetMachineConfig(t.Context(), 2, 512)
	if err == nil {
		t.Fatal("expected an error on a 500 response")
	}
}
