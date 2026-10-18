package cmd

import (
	"net/http"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/hub"
)

// Thin adapters keep the existing cmd call sites on the extracted hub limiter.
type hubClientPeerResolver struct{ r *hub.ClientPeerResolver }

func newHubClientPeerResolver(values []string) (*hubClientPeerResolver, error) {
	r, err := hub.NewClientPeerResolver(values)
	if err != nil {
		return nil, err
	}
	return &hubClientPeerResolver{r: r}, nil
}

func (r *hubClientPeerResolver) peer(request *http.Request) string {
	if r == nil {
		return (*hub.ClientPeerResolver)(nil).Peer(request)
	}
	return r.r.Peer(request)
}

type hubAuthLimiter struct{ l *hub.AuthLimiter }

func newHubAuthLimiter(now func() time.Time) *hubAuthLimiter {
	return &hubAuthLimiter{l: hub.NewAuthLimiter(now)}
}

func (l *hubAuthLimiter) allow(remote string) bool { return l.l.Allow(remote) }

func writeHubRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "6")
	w.Header().Set("Cache-Control", "no-store")
	writeOpenAIError(w, http.StatusTooManyRequests, "rate_limited", "too many authentication requests")
}
