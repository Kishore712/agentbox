package apiservice

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// NewRouter wires the REST API Service's public surface (§4.1). apiKey is
// the single static key checked on every request (§8: auth model still
// flagged as "not yet decided" in the design doc — a single shared key is
// the simplest option that unblocks building the rest of the service; swap
// for per-agent keys later without touching any other handler).
func NewRouter(svc *Service, apiKey string) http.Handler {
	h := &httpHandlers{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agents", h.createAgent)
	mux.HandleFunc("GET /agents/{id}", h.getAgent)
	mux.HandleFunc("DELETE /agents/{id}", h.deleteAgent)
	mux.HandleFunc("POST /agents/{id}/sessions", h.createSession)
	mux.HandleFunc("GET /agents/{id}/sessions/{sid}", h.getSession)
	mux.HandleFunc("DELETE /agents/{id}/sessions/{sid}", h.deleteSession)
	mux.HandleFunc("POST /agents/{id}/invocation", h.invoke)
	mux.HandleFunc("GET /agents/{id}/invocation", h.invoke)
	return authMiddleware(apiKey, mux)
}

func authMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || got != apiKey {
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
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

// mapControllerError translates the ControllerClient sentinel errors into
// the HTTP statuses §4.1 specifies. op identifies the calling handler for
// the server-side log line on the unclassified (500) path.
func mapControllerError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrWorkloadNotReady):
		writeError(w, http.StatusConflict, "agent is not ready")
	case errors.Is(err, ErrAtCapacity):
		writeError(w, http.StatusTooManyRequests, "max concurrent sessions reached")
	default:
		// Unclassified: could be a Controller-unreachable error carrying an
		// internal URL, so it's logged server-side, not sent to the client.
		slog.Error("request failed", "op", op, "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// --- Agent handlers ---

type createAgentRequest struct {
	AgentName              string   `json:"agent_name"`
	ImageRef               string   `json:"image_ref"`
	IdleTimeoutSeconds     int      `json:"idle_timeout_seconds"`
	EgressAllowlist        []string `json:"egress_allowlist"`
	VCPUs                  int      `json:"vcpus"`
	MemoryMiB              int      `json:"memory_mib"`
	MaxConcurrentInstances int      `json:"max_concurrent_instances"`
}

func (h *httpHandlers) createAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	wl, err := h.svc.CreateAgent(r.Context(), CreateWorkloadRequest{
		Name: req.AgentName, ImageRef: req.ImageRef, IdleTimeoutSeconds: req.IdleTimeoutSeconds,
		EgressAllowlist: req.EgressAllowlist, VCPUs: req.VCPUs, MemoryMiB: req.MemoryMiB,
		MaxConcurrentInstances: req.MaxConcurrentInstances,
	})
	if err != nil {
		mapControllerError(w, r, "create_agent", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"agent_id": wl.WorkloadID, "status": wl.Status})
}

func (h *httpHandlers) getAgent(w http.ResponseWriter, r *http.Request) {
	wl, err := h.svc.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		mapControllerError(w, r, "get_agent", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": wl.WorkloadID, "agent_name": wl.Name, "image_ref": wl.ImageRef, "status": wl.Status,
		"idle_timeout_seconds": wl.IdleTimeoutSeconds, "egress_allowlist": wl.EgressAllowlist,
		"vcpus": wl.VCPUs, "memory_mib": wl.MemoryMiB, "max_concurrent_instances": wl.MaxConcurrentInstances,
		"created_at": wl.CreatedAt,
	})
}

func (h *httpHandlers) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteAgent(r.Context(), r.PathValue("id")); err != nil {
		mapControllerError(w, r, "delete_agent", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// --- Session handlers ---

func sessionResultJSON(res *InstanceResult) map[string]any {
	m := map[string]any{"session_id": res.InstanceID, "state": res.State}
	if res.State == StateFailed {
		m["error"] = res.Error
		return m
	}
	return m
}

func (h *httpHandlers) createSession(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.CreateSession(r.Context(), r.PathValue("id"))
	if err != nil {
		mapControllerError(w, r, "create_session", err)
		return
	}
	writeJSON(w, http.StatusAccepted, sessionResultJSON(res))
}

func (h *httpHandlers) getSession(w http.ResponseWriter, r *http.Request) {
	inst, err := h.svc.GetSession(r.Context(), r.PathValue("sid"))
	if err != nil {
		mapControllerError(w, r, "get_session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": inst.InstanceID, "agent_id": inst.WorkloadID, "state": inst.State,
		"host_id": inst.HostID, "last_active": inst.LastActive,
	})
}

func (h *httpHandlers) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteSession(r.Context(), r.PathValue("sid")); err != nil {
		mapControllerError(w, r, "delete_session", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// --- Invocation ---

func (h *httpHandlers) invoke(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" && r.Method == http.MethodGet {
		writeError(w, http.StatusBadRequest, "session_id is required for GET")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	result, err := h.svc.Invoke(r.Context(), r.PathValue("id"), InvokeRequest{
		Method:    r.Method,
		SessionID: sessionID,
		Header:    r.Header,
		Body:      body,
	})
	if err != nil {
		var failed *SessionFailedError
		switch {
		case errors.Is(err, ErrSessionIDRequired):
			writeError(w, http.StatusBadRequest, "session_id is required for GET")
		case errors.As(err, &failed):
			writeError(w, http.StatusConflict, "session failed: "+failed.Reason)
		case errors.Is(err, ErrNotFound):
			writeError(w, http.StatusNotFound, "unknown session")
		case errors.Is(err, ErrWorkloadNotReady):
			writeError(w, http.StatusConflict, "agent is not ready")
		case errors.Is(err, ErrAtCapacity):
			writeError(w, http.StatusTooManyRequests, "max concurrent sessions reached")
		default:
			// Unclassified: typically a guest-proxy dial/timeout failure,
			// which can carry an internal guest IP — log it, don't forward it.
			slog.Error("request failed", "op", "invoke", "method", r.Method, "path", r.URL.Path, "error", err)
			writeError(w, http.StatusBadGateway, "upstream request failed")
		}
		return
	}

	for k, vv := range result.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if result.SessionID != "" {
		w.Header().Set("X-Session-Id", result.SessionID)
	}
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}
