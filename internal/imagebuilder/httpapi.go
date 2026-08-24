package imagebuilder

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// NewRouter wires the Image Builder's API: a single synchronous endpoint the
// Controller calls in place of the in-process Build() call it used to make
// when the Image Builder ran inside the Controller's own process. Split out
// as its own systemd-managed service because Build() needs real mount/loop
// privileges (§4.6) that turned out to be genuinely unsafe to grant a
// container sharing the host's /dev — see controller.Dockerfile's history
// for why. Internal-only: reachable solely from the Controller, same trust
// model as the Host Agent's control-plane routes.
func NewRouter(b *Builder) *http.ServeMux {
	h := &httpHandlers{b: b}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /build", h.build)
	return mux
}

type httpHandlers struct {
	b *Builder
}

type buildRequest struct {
	WorkloadID string `json:"workload_id"`
	ImageRef   string `json:"image_ref"`
}

type buildResponse struct {
	RootfsRef string `json:"rootfs_ref"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// build runs the full §4.6 pipeline synchronously and blocks until it's
// done — a real build (pull, export, format, extract, inject) can take from
// several seconds to over a minute depending on image size, so the caller
// (Controller's runImageBuild, already running in its own goroutine per
// workload) is expected to use a client timeout generous enough to cover
// that, the same reasoning as apiservice's ControllerTimeout.
func (h *httpHandlers) build(w http.ResponseWriter, r *http.Request) {
	var req buildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.WorkloadID == "" || req.ImageRef == "" {
		writeError(w, http.StatusBadRequest, "workload_id and image_ref are required")
		return
	}

	rootfsRef, err := h.b.Build(r.Context(), req.WorkloadID, req.ImageRef)
	if err != nil {
		slog.Error("image build failed", "workload_id", req.WorkloadID, "image_ref", req.ImageRef, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, buildResponse{RootfsRef: rootfsRef})
}
