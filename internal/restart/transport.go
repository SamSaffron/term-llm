package restart

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// httpDrainAllowance is a final, fixed ownership window after the normal grace
// period. Neither overlapping requests nor fresh connections can extend it.
const httpDrainAllowance = 5 * time.Second

// HTTPTransport accounts for finite request effects and response flushing while
// permitting controls/lease maintenance throughout drain.
// Detached producers must use Root; request-bound producers must fence admission
// before effects. Passive streams detach explicitly, never by method or URL.
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
		_, release, err := h.Coordinator.httpActivity(context.Background(), false)
		if err != nil {
			_ = conn.Close()
			return
		}
		entry.release = release
	case http.StateIdle, http.StateClosed, http.StateHijacked:
		// Hijacked connections never receive StateClosed. The handler and any
		// Root operation retain their own tickets until their work finishes.
		if entry.release != nil {
			entry.release()
			entry.release = nil
		}
	}
}

// AdmitRequest accounts for a request through actual completion, including
// protocol adapters (such as reverse forwarding) that dispatch outside net/http.
// It shares Handler's bounded drain allowance, never root producer admission.
func (h *HTTPTransport) AdmitRequest(ctx context.Context) (context.Context, func(), error) {
	return h.Coordinator.httpActivity(ctx, true)
}

func (h *HTTPTransport) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release, err := h.AdmitRequest(r.Context())
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, ErrDraining.Error(), http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// httpActivity atomically classifies transport ownership and cancellation.
// After the fixed ownership window, controls are observers: they can release
// blockers but cannot prolong drain, and exec may cut off their replies.
// Producers must fence admission independently. Existing ownership still waits
// for actual settlement, never merely cancellation.
func (c *Coordinator) httpActivity(ctx context.Context, handler bool) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.draining {
		if c.status.Phase == "replacing" {
			return ctx, nil, ErrDraining
		}
		if !time.Now().Before(c.httpDeadline) {
			return context.WithValue(ctx, operationKey{}, &operation{coordinator: c, released: true}), func() {}, nil
		}
	}
	source := "HTTP connection"
	if handler {
		source = "HTTP handler"
	}
	ctx, release, err := c.admitLocked(ctx, true, false, source)
	if err != nil {
		return ctx, nil, err
	}
	if handler {
		var finish func()
		if c.draining {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadlineCause(ctx, c.httpDeadline, ErrInterrupt)
			finish = cancel
		} else {
			ctx, finish = c.cancellableLocked(ctx)
		}
		releaseOwner := release
		release = func() { finish(); releaseOwner() }
	}
	return ctx, release, nil
}

// DrainingRequest also fences observer requests admitted without ownership,
// even if a failed replacement returned to ready before they reached ingress.
func (c *Coordinator) DrainingRequest(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, _ := ctx.Value(operationKey{}).(*operation)
	return c.draining || (op != nil && op.coordinator == c && op.released)
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
