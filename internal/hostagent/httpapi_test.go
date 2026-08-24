package hostagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func newTestHTTPRouter(ops HostOps, fc *fakeFirecrackerClient, readiness ReadinessChecker) *http.ServeMux {
	mgr := newTestManager(ops, fc, readiness)
	return NewRouter(mgr)
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHTTP_BootVM(t *testing.T) {
	mux := newTestHTTPRouter(newFakeHostOps(), &fakeFirecrackerClient{}, &fakeReadiness{})

	rec := doJSON(t, mux, "POST", "/vm", bootVMRequest{
		InstanceID: "wl_x:agent:abc", RootfsRef: "/data/workloads/wl_x/rootfs.ext4",
		VCPUs: 2, MemoryMiB: 512, EgressAllowlist: []string{"api.openai.com"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["guest_ip"] != "172.16.0.2" {
		t.Errorf("got %v", body)
	}
}

func TestHTTP_BootVM_Failure(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{instanceStartErr: errAny}
	mux := newTestHTTPRouter(ops, fc, &fakeReadiness{})

	rec := doJSON(t, mux, "POST", "/vm", bootVMRequest{InstanceID: "x"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
}

func TestHTTP_SuspendResumeDelete(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	mux := newTestHTTPRouter(ops, fc, &fakeReadiness{})

	rec := doJSON(t, mux, "POST", "/vm", bootVMRequest{InstanceID: "inst-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("boot: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, mux, "POST", "/vm/inst-1/suspend", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, mux, "POST", "/vm/inst-1/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, mux, "DELETE", "/vm/inst-1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !ops.deletedInstances["inst-1"] {
		t.Error("expected instance files deleted")
	}
}

func TestHTTP_ResumeVM_SnapshotMissingReturns404(t *testing.T) {
	mux := newTestHTTPRouter(newFakeHostOps(), &fakeFirecrackerClient{}, &fakeReadiness{})
	// Never booted, so no metadata exists — ResumeVM should fail with
	// ErrSnapshotMissing, which the handler must map to 404 (this is
	// exactly the status code the Controller's HTTPHostAgentClient
	// depends on to detect a genuinely unrecoverable resume, §4.2).
	rec := doJSON(t, mux, "POST", "/vm/never-booted/resume", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestHTTP_Proxy_ForwardsToRegisteredInstance(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	proxy := &fakeGuestProxy{forwardResponse: &ProxyResponse{StatusCode: http.StatusTeapot, Body: []byte("hello")}}
	mgr := newTestManagerWithProxy(ops, fc, &fakeReadiness{}, proxy)
	mux := NewRouter(mgr)

	rec := doJSON(t, mux, "POST", "/vm", bootVMRequest{InstanceID: "inst-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("boot: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, mux, "POST", "/vm/inst-1/proxy", nil)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("proxy: got %d, want the guest's passthrough status 418, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(proxyErrorHeader) != "" {
		t.Errorf("a real guest passthrough response must never carry %s", proxyErrorHeader)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want the guest's passthrough body", rec.Body.String())
	}
}

func TestHTTP_Proxy_UnknownInstanceReturns404WithMarkerHeader(t *testing.T) {
	mux := newTestHTTPRouter(newFakeHostOps(), &fakeFirecrackerClient{}, &fakeReadiness{})

	rec := doJSON(t, mux, "POST", "/vm/never-booted/proxy", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if rec.Header().Get(proxyErrorHeader) != "registry-miss" {
		t.Errorf("X-Agentbox-Proxy-Error = %q, want registry-miss — this is the signal the REST API Service's fallback relies on", rec.Header().Get(proxyErrorHeader))
	}
}

func TestHTTP_Proxy_GuestUnreachableReturns502WithMarkerHeader(t *testing.T) {
	ops := newFakeHostOps()
	fc := &fakeFirecrackerClient{}
	proxy := &fakeGuestProxy{forwardErr: errAny}
	mgr := newTestManagerWithProxy(ops, fc, &fakeReadiness{}, proxy)
	mux := NewRouter(mgr)

	rec := doJSON(t, mux, "POST", "/vm", bootVMRequest{InstanceID: "inst-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("boot: got %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, mux, "POST", "/vm/inst-1/proxy", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
	if rec.Header().Get(proxyErrorHeader) != "guest-unreachable" {
		t.Errorf("X-Agentbox-Proxy-Error = %q, want guest-unreachable", rec.Header().Get(proxyErrorHeader))
	}
}

func TestHTTP_GoldenRootfs_PushThenCheck(t *testing.T) {
	mux := newTestHTTPRouter(newFakeHostOps(), &fakeFirecrackerClient{}, &fakeReadiness{})
	path := "/data/workloads/wl_1/rootfs.ext4"

	// Not there yet.
	req := httptest.NewRequest("HEAD", "/golden-rootfs?path="+url.QueryEscape(path), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD before push: got %d, want 404", rec.Code)
	}

	// Push it.
	req = httptest.NewRequest("PUT", "/golden-rootfs?path="+url.QueryEscape(path), bytes.NewReader([]byte("rootfs bytes")))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: got %d, body=%s", rec.Code, rec.Body.String())
	}

	// Now it's there.
	req = httptest.NewRequest("HEAD", "/golden-rootfs?path="+url.QueryEscape(path), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD after push: got %d, want 200", rec.Code)
	}
}

func TestHTTP_GoldenRootfs_MissingPathParamIsBadRequest(t *testing.T) {
	mux := newTestHTTPRouter(newFakeHostOps(), &fakeFirecrackerClient{}, &fakeReadiness{})
	req := httptest.NewRequest("PUT", "/golden-rootfs", bytes.NewReader([]byte("x")))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

var errAny = &simpleErr{"simulated failure"}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }
