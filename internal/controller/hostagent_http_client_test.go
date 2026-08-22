package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPHostAgentClient_BootVM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/vm" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req BootVMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.InstanceID != "wl_x:agent:abc" {
			t.Errorf("instance_id = %q, want wl_x:agent:abc", req.InstanceID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(VMEndpoint{GuestIP: "172.16.0.2", GuestPort: 8080})
	}))
	defer srv.Close()

	c := NewHTTPHostAgentClient()
	ep, err := c.BootVM(t.Context(), strings.TrimPrefix(srv.URL, "http://"), BootVMRequest{
		InstanceID: "wl_x:agent:abc", RootfsRef: "/data/x/rootfs.ext4", VCPUs: 2, MemoryMiB: 512,
	})
	if err != nil {
		t.Fatalf("BootVM: %v", err)
	}
	if ep.GuestIP != "172.16.0.2" || ep.GuestPort != 8080 {
		t.Errorf("got %+v", ep)
	}
}

func TestHTTPHostAgentClient_BootVM_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "firecracker boot timeout"})
	}))
	defer srv.Close()

	c := NewHTTPHostAgentClient()
	_, err := c.BootVM(t.Context(), strings.TrimPrefix(srv.URL, "http://"), BootVMRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "firecracker boot timeout") {
		t.Errorf("error = %v, want it to include the host agent's error body", err)
	}
}

func TestHTTPHostAgentClient_SuspendAndDelete(t *testing.T) {
	var gotSuspend, gotDelete bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/vm/inst-1/suspend":
			gotSuspend = true
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{})
		case r.Method == http.MethodDelete && r.URL.Path == "/vm/inst-1":
			gotDelete = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewHTTPHostAgentClient()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := c.SuspendVM(t.Context(), addr, "inst-1"); err != nil {
		t.Fatalf("SuspendVM: %v", err)
	}
	if err := c.DeleteVM(t.Context(), addr, "inst-1"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if !gotSuspend || !gotDelete {
		t.Errorf("gotSuspend=%v gotDelete=%v", gotSuspend, gotDelete)
	}
}

func TestHTTPHostAgentClient_ResumeVM_SnapshotMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "snapshot not found on disk"})
	}))
	defer srv.Close()

	c := NewHTTPHostAgentClient()
	_, err := c.ResumeVM(t.Context(), strings.TrimPrefix(srv.URL, "http://"), "inst-1")
	if !errors.Is(err, ErrSnapshotMissing) {
		t.Fatalf("got %v, want ErrSnapshotMissing (a 404 from the Host Agent must map to this sentinel per §4.2)", err)
	}
}

func TestHTTPHostAgentClient_ResumeVM_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(VMEndpoint{GuestIP: "172.16.0.9", GuestPort: 8080})
	}))
	defer srv.Close()

	c := NewHTTPHostAgentClient()
	ep, err := c.ResumeVM(t.Context(), strings.TrimPrefix(srv.URL, "http://"), "inst-1")
	if err != nil {
		t.Fatalf("ResumeVM: %v", err)
	}
	if ep.GuestIP != "172.16.0.9" {
		t.Errorf("got %+v", ep)
	}
}
