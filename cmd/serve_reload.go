package cmd

import (
	"context"
	"net/http"

	"github.com/samsaffron/term-llm/internal/restart"
)

// admitServeWork fences a new detached operation before any durable acceptance.
// Request disconnects do not cancel accepted work. Explicit Stop retains its
// existing operation-specific cancellation path.
func admitServeWork(w http.ResponseWriter, r *http.Request) (context.Context, func(), bool) {
	ctx, release, err := restart.Default.Root(context.WithoutCancel(r.Context()))
	if err != nil {
		w.Header().Set("Retry-After", "1")
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_reloading", err.Error())
		return nil, nil, false
	}
	return ctx, release, true
}
