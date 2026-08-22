package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"agentbox/internal/apiservice"
	"agentbox/internal/common"
	"agentbox/internal/controller"
	"agentbox/internal/hostagent"
)

const fullStackAPIKey = "full-stack-test-key"
const fullStackTokenSecret = "full-stack-test-secret"

// fullStackHarness wires all three real services together over real HTTP —
// REST API Service -> Controller -> Host Agent — with only the pieces that
// genuinely require Linux/KVM/a real guest app stubbed out: VM operations,
// the Firecracker API itself, and the guest application the client's
// request ultimately reaches. Everything else (auth, routing-token
// issuance/verification, the resume-on-suspend fallback, all three
// services' actual HTTP contracts) is exercised for real.
type fullStackHarness struct {
	t       *testing.T
	apiURL  string
	ctrlURL string
	guest   *httptest.Server
	guestFn func(w http.ResponseWriter, r *http.Request)
}

func newFullStackHarness(t *testing.T) *fullStackHarness {
	t.Helper()
	ctx := context.Background()

	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 13})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("local redis not reachable on localhost:6379: %v", err)
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.FlushDB(ctx); rdb.Close() })

	// A stand-in for the customer's actual application inside the guest.
	h := &fullStackHarness{t: t}
	h.guestFn = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from guest")
	}
	h.guest = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.guestFn(w, r) }))
	t.Cleanup(h.guest.Close)
	guestAddr := strings.TrimPrefix(h.guest.URL, "http://")
	guestParts := strings.Split(guestAddr, ":")

	// Host Agent: real HTTP server, stubbed VM ops that report the actual
	// guest test-server's address as the "booted" endpoint, so the API
	// Service's proxy calls land on a real HTTP server.
	ops := &fullStackStubHostOps{guestIP: guestParts[0], stubHostOps: newStubHostOps()}
	mgr := hostagent.NewVMManager(
		ops,
		func(string) hostagent.FirecrackerClient { return stubFirecrackerClient{} },
		stubReadiness{},
		hostagent.Config{KernelImagePath: "/data/vmlinux", GuestPort: mustAtoi(t, guestParts[1]), BootTimeout: time.Second},
	)
	haServer := httptest.NewServer(hostagent.NewRouter(mgr))
	t.Cleanup(haServer.Close)
	haAddr := strings.TrimPrefix(haServer.URL, "http://")

	// Controller: real HTTP server, real Redis, real HTTPHostAgentClient.
	store := controller.NewStore(rdb)
	tokens := controller.NewTokenIssuer([]byte(fullStackTokenSecret))
	ha := controller.NewHTTPHostAgentClient()
	ib := &stubImageBuilder{rootfsRef: "/data/workloads/full-stack/rootfs.ext4"}
	ctrlSvc := controller.NewService(store, ha, tokens, ib)
	ctrlServer := httptest.NewServer(controller.NewRouter(ctrlSvc))
	t.Cleanup(ctrlServer.Close)

	if err := store.UpsertHost(ctx, &common.Host{
		HostID: "host-1", InternalAddr: haAddr, Status: common.HostHealthy,
	}); err != nil {
		t.Fatal(err)
	}

	// REST API Service: real HTTP server, real HTTPControllerClient, real
	// token verification against the same shared secret as the Controller.
	apiCtrl := apiservice.NewHTTPControllerClient(ctrlServer.URL)
	apiTokens := apiservice.NewTokenVerifier([]byte(fullStackTokenSecret))
	apiProxy := apiservice.NewHTTPGuestProxy(5 * time.Second)
	apiSvc := apiservice.NewService(apiCtrl, apiTokens, apiProxy)
	apiServer := httptest.NewServer(apiservice.NewRouter(apiSvc, fullStackAPIKey))
	t.Cleanup(apiServer.Close)

	h.apiURL = apiServer.URL
	h.ctrlURL = ctrlServer.URL
	return h
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a port number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// fullStackStubHostOps overrides SetupNetwork to report the real guest test
// server's IP, so proxied requests actually land somewhere.
type fullStackStubHostOps struct {
	*stubHostOps
	guestIP string
}

func (f *fullStackStubHostOps) SetupNetwork(ctx context.Context, instanceID string, egressAllowlist []string) (hostagent.NetworkInfo, error) {
	return hostagent.NetworkInfo{TapDevice: "tap-" + instanceID, GuestIP: f.guestIP, HostIP: "127.0.0.1"}, nil
}

func (h *fullStackHarness) req(method, path string, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.apiURL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullStackAPIKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (h *fullStackHarness) postJSON(path, body string, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.apiURL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullStackAPIKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func TestFullStack_CustomerNeverSeesA503(t *testing.T) {
	h := newFullStackHarness(t)

	// 1. Register the agent.
	resp := h.postJSON("/agents", `{"agent_name":"e2e-agent","image_ref":"example/x:tag","idle_timeout_seconds":300,"vcpus":1,"memory_mib":256,"max_concurrent_instances":5}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create agent: got %d", resp.StatusCode)
	}
	var created map[string]any
	mustDecode(t, resp, &created)
	agentID, _ := created["agent_id"].(string)
	if agentID == "" {
		t.Fatalf("expected agent_id: %v", created)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r := h.req(http.MethodGet, "/agents/"+agentID, nil)
		var body map[string]any
		mustDecode(t, r, &body)
		if body["status"] == "READY" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 2. First invoke — cold start, implicit session creation, through the
	// full stack: API Service -> Controller -> Host Agent -> guest.
	resp = h.postJSON("/agents/"+agentID+"/invocation", "", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("cold invoke: got %d, body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from guest" {
		t.Fatalf("cold invoke body = %q", body)
	}
	sessionID := resp.Header.Get("X-Session-Id")
	token := resp.Header.Get("X-Routing-Token")
	if sessionID == "" || token == "" {
		t.Fatalf("expected X-Session-Id and X-Routing-Token headers, got session=%q token=%q", sessionID, token)
	}

	// 3. Warm invoke — token-routed, should not touch the Controller for
	// routing at all.
	resp = h.req(http.MethodGet, "/agents/"+agentID+"/invocation?session_id="+sessionID, map[string]string{"X-Routing-Token": token})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("warm invoke: got %d, body=%s", resp.StatusCode, b)
	}

	// 4. Suspend the session, via the Controller's ops-tooling endpoint
	// directly. In production there's no public HTTP path for this — the
	// Controller's internal idle-reaper loop calls Service.SuspendInstance
	// in-process (§4.2) — but hitting the same endpoint here exercises the
	// identical Controller->Host Agent suspend call that loop would trigger.
	suspendResp, err := http.Post(h.ctrlURL+"/internal/instances/"+sessionID+"/suspend", "application/json", nil)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if suspendResp.StatusCode != http.StatusAccepted {
		t.Fatalf("suspend: got %d", suspendResp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	// 5. Invoke again with the now-stale token. This is the core promise:
	// the client must see a normal 200, never a 503, even though the
	// session was suspended in between.
	resp = h.postJSON("/agents/"+agentID+"/invocation?session_id="+sessionID, "", map[string]string{"X-Routing-Token": token})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("invoke on suspended session: got %d (want 200 — the client must never see a 503), body=%s", resp.StatusCode, b)
	}
	body, _ = io.ReadAll(resp.Body)
	if string(body) != "hello from guest" {
		t.Fatalf("post-resume body = %q", body)
	}

	// 6. Delete the session.
	req, _ := http.NewRequest(http.MethodDelete, h.apiURL+"/agents/"+agentID+"/sessions/"+sessionID, nil)
	req.Header.Set("Authorization", "Bearer "+fullStackAPIKey)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete session: got %d", delResp.StatusCode)
	}
}

func mustDecode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
