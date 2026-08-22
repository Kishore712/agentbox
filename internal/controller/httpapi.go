package controller

import (
	"encoding/json"
	"errors"
	"net/http"
)

// NewRouter wires the Controller's "A) Control-plane API" (§4.2) — the REST
// API Service's only access to workload/instance data. Uses the standard
// library's 1.22+ ServeMux method+path routing; no external router needed
// for this surface.
func NewRouter(svc *Service) *http.ServeMux {
	h := &httpHandlers{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/workloads", h.createWorkload)
	mux.HandleFunc("GET /internal/workloads/{id}", h.getWorkload)
	mux.HandleFunc("DELETE /internal/workloads/{id}", h.deleteWorkload)
	mux.HandleFunc("POST /internal/workloads/{id}/instances", h.createInstance)
	mux.HandleFunc("GET /internal/instances/{id}", h.getInstance)
	mux.HandleFunc("POST /internal/instances/{id}/resume", h.resumeInstance)
	mux.HandleFunc("POST /internal/instances/{id}/suspend", h.suspendInstance)
	mux.HandleFunc("POST /internal/instances/{id}/heartbeat", h.heartbeat)
	mux.HandleFunc("DELETE /internal/instances/{id}", h.deleteInstance)
	return mux
}

type httpHandlers struct {
	svc *Service
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- Workload handlers ---

type createWorkloadRequest struct {
	Name                   string   `json:"name"`
	ImageRef               string   `json:"image_ref"`
	IdleTimeoutSeconds     int      `json:"idle_timeout_seconds"`
	EgressAllowlist        []string `json:"egress_allowlist"`
	VCPUs                  int      `json:"vcpus"`
	MemoryMiB              int      `json:"memory_mib"`
	MaxConcurrentInstances int      `json:"max_concurrent_instances"`
}

func (h *httpHandlers) createWorkload(w http.ResponseWriter, r *http.Request) {
	var req createWorkloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	wl, err := h.svc.CreateWorkload(r.Context(), CreateWorkloadRequest{
		Name:                   req.Name,
		ImageRef:               req.ImageRef,
		IdleTimeoutSeconds:     req.IdleTimeoutSeconds,
		EgressAllowlist:        req.EgressAllowlist,
		VCPUs:                  req.VCPUs,
		MemoryMiB:              req.MemoryMiB,
		MaxConcurrentInstances: req.MaxConcurrentInstances,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"workload_id": wl.WorkloadID,
		"status":      wl.Status,
		"created_at":  wl.CreatedAt,
	})
}

func (h *httpHandlers) getWorkload(w http.ResponseWriter, r *http.Request) {
	wl, err := h.svc.GetWorkload(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown workload")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workload_id":              wl.WorkloadID,
		"name":                     wl.Name,
		"image_ref":                wl.ImageRef,
		"status":                   wl.Status,
		"idle_timeout_seconds":     wl.IdleTimeoutSeconds,
		"egress_allowlist":         wl.EgressAllowlist,
		"vcpus":                    wl.VCPUs,
		"memory_mib":               wl.MemoryMiB,
		"max_concurrent_instances": wl.MaxConcurrentInstances,
		"created_at":               wl.CreatedAt,
	})
}

func (h *httpHandlers) deleteWorkload(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteWorkload(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// --- Instance handlers ---

// instanceResultJSON renders an InstanceResult per §4.2's convention: a
// FAILED result still carries instance_id/state/error, never routing info.
func instanceResultJSON(res *InstanceResult) map[string]any {
	m := map[string]any{"instance_id": res.InstanceID, "state": res.State}
	if res.Error != "" {
		m["error"] = res.Error
		return m
	}
	m["host_id"] = res.HostID
	m["guest_ip"] = res.GuestIP
	m["guest_port"] = res.GuestPort
	m["routing_token"] = res.RoutingToken
	m["token_exp"] = res.TokenExp
	return m
}

func (h *httpHandlers) createInstance(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.CreateInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			writeError(w, http.StatusNotFound, "unknown workload")
		case errors.Is(err, ErrWorkloadNotReady):
			writeError(w, http.StatusConflict, "workload is not READY")
		case errors.Is(err, ErrAtCapacity):
			writeError(w, http.StatusTooManyRequests, "max_concurrent_instances reached")
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, instanceResultJSON(res))
}

func (h *httpHandlers) getInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := h.svc.GetInstance(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown instance")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id": inst.InstanceID,
		"workload_id": inst.WorkloadID,
		"state":       inst.State,
		"host_id":     inst.HostID,
		"last_active": inst.LastActive,
		"created_at":  inst.CreatedAt,
	})
}

func (h *httpHandlers) resumeInstance(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.ResumeInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown instance")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instanceResultJSON(res))
}

func (h *httpHandlers) suspendInstance(w http.ResponseWriter, r *http.Request) {
	// Note: never called by the REST API Service in practice — the
	// Controller's own idle-reaper loop calls Service.SuspendInstance
	// directly, in-process (§4.2). Exposed over HTTP anyway for
	// completeness/ops tooling.
	if err := h.svc.SuspendInstance(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "suspending"})
}

func (h *httpHandlers) heartbeat(w http.ResponseWriter, r *http.Request) {
	err := h.svc.Heartbeat(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown instance")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *httpHandlers) deleteInstance(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteInstance(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}
