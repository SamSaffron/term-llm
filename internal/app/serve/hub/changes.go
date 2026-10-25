package hub

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/restart"
)

const (
	changesDefaultWait = 15 * time.Second
	changesMaxWait     = 30 * time.Second
)

// HandleChanges long-polls the dashboard change feed:
//
//	GET /api/changes?since=<version>&timeout=<seconds>
//
// It answers {"version":N} as soon as the version differs from since, or
// after timeout (default 15s, max 30s). Omitting since returns immediately.
// Clients refetch their panels on every answer, so a timeout doubles as the
// plain periodic refresh for state the feed does not track (node health).
func HandleChanges(w http.ResponseWriter, r *http.Request, feed *hub.ChangeFeed) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		wire.WriteOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	query := r.URL.Query()
	wait, err := changesTimeout(query.Get("timeout"))
	if err != nil {
		wire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	version := feed.Version()
	if value := strings.TrimSpace(query.Get("since")); value != "" {
		since, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			wire.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "since must be a version number")
			return
		}
		// A parked observer must not hold a restart drain open.
		restart.Passive(r.Context())
		version = feed.Wait(r.Context(), since, wait)
	}
	w.Header().Set("Cache-Control", "no-store")
	wire.WriteJSON(w, http.StatusOK, map[string]any{"version": version})
}

func changesTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return changesDefaultWait, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0, errors.New("timeout must be a non-negative integer")
	}
	return time.Duration(min(seconds, int(changesMaxWait/time.Second))) * time.Second, nil
}
