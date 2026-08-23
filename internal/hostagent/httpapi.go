package hostagent

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// NewRouter wires the Host Agent's API (§4.2 "B) Host Agent API") — called
// by the Controller only, never by the REST API Service.
func NewRouter(mgr *VMManager) *http.ServeMux {
	h := &httpHandlers{mgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /vm", h.bootVM)
	mux.HandleFunc("POST /vm/{id}/suspend", h.suspendVM)
	mux.HandleFunc("POST /vm/{id}/resume", h.resumeVM)
	mux.HandleFunc("DELETE /vm/{id}", h.deleteVM)
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

func (h *httpHandlers) deleteVM(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.DeleteVM(r.Context(), r.PathValue("id")); err != nil {
		writeInternalError(w, r, "delete_vm", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
