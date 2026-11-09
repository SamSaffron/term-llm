package serve

import (
	"errors"
	"net/http"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
	chatui "github.com/samsaffron/term-llm/internal/tui/chat"
)

// handleDraftSkills lists skills for a new chat, which has no session until its
// first send, from the workspace that chat will bind to.
func (s *serveServer) handleDraftSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	q := r.URL.Query()
	dir, err := s.resolveMentionSearchRootForProject(r.Context(), "", q.Get("project_id"), q.Get("worktree_dir"), q.Get("no_project") == "true", isFirstPartyUIResponseRequest(r))
	if typed := (*serveWorkspaceError)(nil); errors.As(err, &typed) {
		writeWorkspaceError(w, err)
	} else if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
	} else {
		s.writeSkillCatalog(w, s.skillsForServeSession(&session.Session{CWD: dir}), []any{})
	}
}

// writeSkillCatalog lists setup's skills; agents, archive, pin and rename are Web-only commands.
func (s *serveServer) writeSkillCatalog(w http.ResponseWriter, setup *skills.Setup, runs any) {
	items, diagnostics := []skills.UserCatalogEntry{}, []skills.UserCatalogDiagnostic{}
	if setup != nil && setup.Registry != nil {
		var err error
		if items, diagnostics, err = setup.Registry.UserCatalog(chatui.CommandNames("agents", "archive", "pin", "rename")); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": items, "diagnostics": diagnostics, "runs": runs})
}
