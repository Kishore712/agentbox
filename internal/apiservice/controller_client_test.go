package apiservice

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPControllerClient_CreateAndGetWorkload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/workloads":
			var req CreateWorkloadRequest
			json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(Workload{WorkloadID: "wl_1", Name: req.Name, Status: "PROVISIONING"})
		case r.Method == http.MethodGet && r.URL.Path == "/internal/workloads/wl_1":
			json.NewEncoder(w).Encode(Workload{WorkloadID: "wl_1", Name: "my-agent", Status: "READY"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewHTTPControllerClient(srv.URL, 15*time.Second)
	w, err := c.CreateWorkload(t.Context(), CreateWorkloadRequest{Name: "my-agent"})
	if err != nil {
		t.Fatalf("CreateWorkload: %v", err)
	}
	if w.WorkloadID != "wl_1" {
		t.Errorf("got %+v", w)
	}

	got, err := c.GetWorkload(t.Context(), "wl_1")
	if err != nil {
		t.Fatalf("GetWorkload: %v", err)
	}
	if got.Status != "READY" {
		t.Errorf("got %+v", got)
	}
}

func TestHTTPControllerClient_GetWorkload_404MapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown workload"})
	}))
	defer srv.Close()

	c := NewHTTPControllerClient(srv.URL, 15*time.Second)
	_, err := c.GetWorkload(t.Context(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestHTTPControllerClient_CreateInstance_MapsStatusCodes(t *testing.T) {
	tests := []struct {
		status  int
		wantErr error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusConflict, ErrWorkloadNotReady},
		{http.StatusTooManyRequests, ErrAtCapacity},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
			json.NewEncoder(w).Encode(map[string]string{"error": "x"})
		}))
		c := NewHTTPControllerClient(srv.URL, 15*time.Second)
		_, err := c.CreateInstance(t.Context(), "wl_1")
		if !errors.Is(err, tt.wantErr) {
			t.Errorf("status %d: got %v, want %v", tt.status, err, tt.wantErr)
		}
		srv.Close()
	}
}

func TestHTTPControllerClient_CreateInstance_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/workloads/wl_1/instances" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(InstanceResult{InstanceID: "inst-1", State: "RUNNING", HostAgentAddr: "10.0.1.5:9000"})
	}))
	defer srv.Close()

	c := NewHTTPControllerClient(srv.URL, 15*time.Second)
	res, err := c.CreateInstance(t.Context(), "wl_1")
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if res.InstanceID != "inst-1" || res.State != "RUNNING" {
		t.Errorf("got %+v", res)
	}
}

func TestHTTPControllerClient_Heartbeat_FireAndForget(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/instances/inst-1/heartbeat" {
			called <- struct{}{}
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := NewHTTPControllerClient(srv.URL, 15*time.Second)
	c.Heartbeat(t.Context(), "inst-1") // must not block

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat request never arrived")
	}
}
