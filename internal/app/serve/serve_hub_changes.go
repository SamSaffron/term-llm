package serve

import (
	"net/http"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
)

func (s *hubServer) handleHubChanges(w http.ResponseWriter, r *http.Request) {
	apphub.HandleChanges(w, r, s.changes)
}
