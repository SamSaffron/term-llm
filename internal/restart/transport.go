package restart

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// HTTPTransport accounts for finite request effects and response flushing while
// permitting controls/lease maintenance during drain. Long-running operations
// started by handlers must use Root. Passive streams explicitly detach; HTTP
// methods and URL strings are not used as proxies for ownership.
type HTTPTransport struct {
	Coordinator *Coordinator
	mu          sync.Mutex
	connections map[net.Conn]*httpConnection
}
type httpConnection struct {
	mu      sync.Mutex
	release func()
}
type connectionKey struct{}

func (h *HTTPTransport) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	entry := &httpConnection{}
	h.mu.Lock()
	if h.connections == nil {
		h.connections = make(map[net.Conn]*httpConnection)
	}
	h.connections[conn] = entry
	h.mu.Unlock()
	return context.WithValue(ctx, connectionKey{}, entry)
}
func (h *HTTPTransport) ConnState(conn net.Conn, state http.ConnState) {
	h.mu.Lock()
	entry := h.connections[conn]
	if state == http.StateClosed || state == http.StateHijacked {
		delete(h.connections, conn)
	}
	h.mu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	switch state {
	case http.StateActive:
		if entry.release != nil {
			return
		}
		_, release, err := h.Coordinator.Activity(context.Background())
		if err != nil {
			_ = conn.Close()
			return
		}
		entry.release = release
	case http.StateIdle, http.StateClosed:
		if entry.release != nil {
			entry.release()
			entry.release = nil
		}
	}
}
func (h *HTTPTransport) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release, err := h.Coordinator.Activity(r.Context())
		if err != nil {
			http.Error(w, ErrDraining.Error(), http.StatusServiceUnavailable)
			return
		}
		defer release()
		ctx, finish := h.Coordinator.Cancellable(ctx)
		defer finish()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Passive releases only the observer request and its HTTP connection. Call at
// the start of an explicitly passive event subscription, never a producer. Any
// detached producer must already have its own operation ticket.
func Passive(ctx context.Context) {
	if op, _ := ctx.Value(operationKey{}).(*operation); op != nil {
		c := op.coordinator
		c.mu.Lock()
		c.releaseLocked(op)
		c.mu.Unlock()
	}
	if entry, _ := ctx.Value(connectionKey{}).(*httpConnection); entry != nil {
		entry.mu.Lock()
		if entry.release != nil {
			entry.release()
			entry.release = nil
		}
		entry.mu.Unlock()
	}
}
