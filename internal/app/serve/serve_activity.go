package serve

import (
	"context"
	"net/http"
	"strings"

	"github.com/term-llm/term-llm/internal/app/serve/sessionactivity"
)

func (s *serveServer) handleSessionActivity(w http.ResponseWriter, r *http.Request, id, suffix string) {
	handler := sessionactivity.Handler{Store: s.store, Cells: s.codeCellSummaries, Jobs: s.jobsV2, Children: func(ctx context.Context, parentID string) ([]sessionactivity.ChildRunProjection, bool, error) {
		projection, err := s.projectSessionChildren(ctx, parentID)
		return projection.Items, projection.Truncated, err
	}}
	if suffix == "activity" {
		handler.ServeSession(w, r, id)
	} else {
		handler.ServeRun(w, r, id, strings.TrimPrefix(suffix, "activity/runs/"))
	}
}
