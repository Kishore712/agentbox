package hostagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// NewRouter wires the Host Agent's API (§4.2 "B) Host Agent API"). Two
// distinct classes of caller as of 3.2, a real change in trust surface
// flagged in the design doc's open questions: the control-plane routes
// (/vm, suspend, resume, delete) are called only by the Controller; the
// data-plane route (/vm/{id}/proxy) is called only by the REST API
// Service, never the Controller.
func NewRouter(mgr *VMManager) *http.ServeMux {
	h := &httpHandlers{mgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /vm", h.bootVM)
	mux.HandleFunc("POST /vm/{id}/suspend", h.suspendVM)
	mux.HandleFunc("POST /vm/{id}/resume", h.resumeVM)
	mux.HandleFunc("DELETE /vm/{id}", h.deleteVM)
	mux.HandleFunc("/vm/{id}/proxy", h.proxy) // no method prefix: forwards GET/POST/etc. alike
	mux.HandleFunc("HEAD /golden-rootfs", h.hasRootfs)
	mux.HandleFunc("PUT /golden-rootfs", h.saveRootfs)
	return mux
}

type httpHandlers struct {
	mgr *VMManager
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeInternalError logs the real error server-side and returns a generic
// message to the caller — an unclassified failure here can carry host
// filesystem paths, TAP device names, or Firecracker socket errors that
// shouldn't leak to whoever's calling this endpoint (the Controller).
func writeInternalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	slog.Error("request failed", "op", op, "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

type bootVMRequest struct {
	InstanceID      string   `json:"instance_id"`
	RootfsRef       string   `json:"rootfs_ref"`
	VCPUs           int      `json:"vcpus"`
	MemoryMiB       int      `json:"memory_mib"`
	EgressAllowlist []string `json:"egress_allowlist"`
}

func (h *httpHandlers) bootVM(w http.ResponseWriter, r *http.Request) {
	var req bootVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ep, err := h.mgr.BootVM(r.Context(), BootRequest{
		InstanceID:      req.InstanceID,
		RootfsRef:       req.RootfsRef,
		VCPUs:           req.VCPUs,
		MemoryMiB:       req.MemoryMiB,
		EgressAllowlist: req.EgressAllowlist,
	})
	if err != nil {
		writeInternalError(w, r, "boot_vm", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guest_ip": ep.GuestIP, "guest_port": ep.GuestPort})
}

func (h *httpHandlers) suspendVM(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.SuspendVM(r.Context(), r.PathValue("id")); err != nil {
		writeInternalError(w, r, "suspend_vm", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *httpHandlers) resumeVM(w http.ResponseWriter, r *http.Request) {
	ep, err := h.mgr.ResumeVM(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrSnapshotMissing) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeInternalError(w, r, "resume_vm", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"guest_ip": ep.GuestIP, "guest_port": ep.GuestPort})
}

// hasRootfs and saveRootfs implement §4.6's placement-locality fix — the
// Controller calls these to check whether this host already has a
// workload's golden rootfs cached, and to push one if not (§4.2's
// CreateInstance flow). path is an exact filesystem path, not a workload
// ID — this host has no opinion about the golden-rootfs directory
// convention; it just needs the byte-for-byte same path string the
// Controller will later hand it in a BootVM's rootfs_ref (§4.3's
// CopyRootfs step reads that exact path).
func (h *httpHandlers) hasRootfs(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	ok, err := h.mgr.HasRootfs(r.Context(), path)
	if err != nil {
		writeInternalError(w, r, "has_rootfs", err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *httpHandlers) saveRootfs(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := h.mgr.SaveRootfs(r.Context(), path, r.Body); err != nil {
		writeInternalError(w, r, "save_rootfs", err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *httpHandlers) deleteVM(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.DeleteVM(r.Context(), r.PathValue("id")); err != nil {
		writeInternalError(w, r, "delete_vm", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// proxyErrorHeader marks a response as a Host-Agent-generated proxy
// failure, not a real response relayed from the guest app. Necessary
// because the guest's own app can legitimately return any status code —
// including 404 or 502 — as a normal response on the single fixed doorway
// (§4.1); without an out-of-band marker, the REST API Service couldn't
// tell "this instance needs a resume" apart from "the app you're running
// happens to return 404 for this input." Never set on a real passthrough
// response — only ever added by this handler itself, never copied from
// the guest.
const proxyErrorHeader = "X-Agentbox-Proxy-Error"

// proxy implements the data-plane forward (design doc §4.3, "Flow —
// Data-plane proxy"). Distinguishes ErrInstanceNotRegistered (404,
// registry-miss — the REST API Service should resume and retry) from any
// other failure (502, guest-unreachable — the instance is registered but
// the guest itself didn't answer; a resume may not help) — both carry
// proxyErrorHeader so the caller can tell either apart from a real guest
// response with the same status code.
func (h *httpHandlers) proxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	resp, err := h.mgr.Proxy(r.Context(), r.PathValue("id"), &ProxyRequest{
		Method: r.Method,
		Header: r.Header,
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		if errors.Is(err, ErrInstanceNotRegistered) {
			w.Header().Set(proxyErrorHeader, "registry-miss")
			writeError(w, http.StatusNotFound, "instance not registered on this host")
			return
		}
		slog.Error("data-plane proxy failed", "op", "proxy", "instance_id", r.PathValue("id"), "error", err)
		w.Header().Set(proxyErrorHeader, "guest-unreachable")
		writeError(w, http.StatusBadGateway, "guest unreachable")
		return
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
}
