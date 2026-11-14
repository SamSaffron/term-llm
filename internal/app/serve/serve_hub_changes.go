package serve

import (
	"net/http"

	apphub "github.com/term-llm/term-llm/internal/app/serve/hub"
)

func (s *hubServer) handleHubChanges(w http.ResponseWriter, r *http.Request) {
	apphub.HandleChanges(w, r, s.changes)
}
