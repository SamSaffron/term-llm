package restart

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// Handler admits requests before dispatch and retains ownership through handler
// return. Async work MUST acquire a child from r.Context() before returning.
// Apply this to operation ingress, not passive event streams or control endpoints.
// It deliberately makes no assumptions about HTTP methods or URL spelling.
func (c *Coordinator) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release, err := c.Enter(r.Context())
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, ErrDraining.Error(), http.StatusServiceUnavailable)
			return
		}
		defer release()
		ctx, finish := c.Cancellable(ctx)
		defer finish()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HTTPConnections retains HTTP/1 transport ownership until net/http has flushed
// the response, not merely until middleware returns. Install with Handler on a
// plain HTTP server. HTTP/2 and hijacked/stateful transports need their own owner.
// New active connections during drain are closed before dispatching work; idle
// keep-alive connections do not block replacement. Hijacked connections retain
// ownership: net/http no longer knows when their work ends, so fail closed.
func (c *Coordinator) HTTPConnections() func(net.Conn, http.ConnState) {
	var mu sync.Mutex
	active := make(map[net.Conn]func())
	return func(conn net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case http.StateActive:
			if active[conn] != nil {
				return
			}
			_, release, err := c.Enter(context.Background())
			if err != nil {
				_ = conn.Close()
				return
			}
			active[conn] = release
		case http.StateIdle, http.StateClosed:
			if release := active[conn]; release != nil {
				delete(active, conn)
				release()
			}
		}
	}
}
