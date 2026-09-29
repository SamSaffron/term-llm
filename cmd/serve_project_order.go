package cmd

import (
	"errors"
	"log"
	"net/http"

	"github.com/samsaffron/term-llm/internal/session"
)

// projectOrderRequest lists project IDs in their new relative order.
type projectOrderRequest struct {
	ProjectIDs []string `json:"project_ids"`
}

// handleProjectsOrder serves PATCH /v1/projects/order. The listed projects
// move, in the given order, into the sidebar ranks they already occupy;
// projects the caller did not list (for example archived projects, which the
// sidebar shows after active ones) keep their positions. The request is
// atomic and idempotent, and the response is the complete committed order.
func (s *serveServer) handleProjectsOrder(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled {
		writeProjectError(w, http.StatusNotFound, "projects_disabled", "project mode is disabled")
		return
	}
	if r.Method != http.MethodPatch {
		w.Header().Set("Allow", http.MethodPatch)
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeProjectError(w, http.StatusUnsupportedMediaType, "invalid_project_order", err.Error())
		return
	}
	projects, ok := s.projectStore()
	if !ok {
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
		return
	}
	var req projectOrderRequest
	if err := decodeSmallJSON(w, r, &req); err != nil {
		writeProjectError(w, http.StatusBadRequest, "invalid_project_order", "invalid project order request: "+err.Error())
		return
	}
	order, err := projects.ReorderProjects(r.Context(), req.ProjectIDs)
	if err != nil {
		writeProjectOrderError(w, err)
		return
	}
	for _, id := range order.Changed {
		s.publishEvent(serveEventInput{Type: serveEventProjectUpdated, ProjectID: id, Reason: "order"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": order.Positions})
}

func writeProjectOrderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrProjectOrderInvalid):
		writeProjectError(w, http.StatusBadRequest, "invalid_project_order", err.Error())
	case errors.Is(err, session.ErrNotFound):
		writeProjectError(w, http.StatusNotFound, "project_not_found", "a listed project was not found; refresh and try again")
	case errors.Is(err, session.ErrProjectsUnsupported):
		writeProjectError(w, http.StatusServiceUnavailable, "projects_unavailable", "project storage is unavailable")
	default:
		log.Printf("[serve] reorder projects: %v", err)
		writeProjectError(w, http.StatusInternalServerError, "projects_unavailable", "failed to save the project order")
	}
}
