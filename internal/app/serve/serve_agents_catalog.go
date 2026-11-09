package serve

import (
	"net/http"

	"github.com/samsaffron/term-llm/internal/app/serve/agentcatalog"
)

// handleAgentCatalog serves the Web UI's /agents catalog.
func (s *serveServer) handleAgentCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	} else if catalog, err := agentcatalog.List(s.cfgRef); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
	} else {
		writeJSON(w, http.StatusOK, catalog)
	}
}
