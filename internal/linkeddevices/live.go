package linkeddevices

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

// LiveOwner binds a call to a verified device and its trusted peer lifetime.
// Its zero/nil value represents an ordinary owner call, not a linked call.
// Identity is immutable after construction; Finish may race lifetime shutdown.
type LiveOwner struct {
	deviceID string
	lifetime context.Context
	done     chan struct{}
	finished sync.Once
}

// NewLiveOwner captures authenticated device identity, never client body fields.
func NewLiveOwner(r *http.Request) *LiveOwner {
	if principal, ok := DeviceFromRequest(r); ok {
		return &LiveOwner{deviceID: principal.DeviceID, lifetime: principal.Lifetime, done: make(chan struct{})}
	}
	return nil
}

// Finish releases the lifetime watcher after the host removes a call.
func (owner *LiveOwner) Finish() {
	if owner != nil && owner.done != nil {
		owner.finished.Do(func() { close(owner.done) })
	}
}

// Watch stops a call on revocation, sharing stop/reset, peer replacement or
// disconnect. The creation request's context must not be used: it ends after
// the SDP reply. The host callback owns controller/provider teardown.
func (owner *LiveOwner) Watch(stop func(context.Context)) {
	if owner == nil || owner.lifetime == nil {
		return
	}
	go func() {
		select {
		case <-owner.done:
			return
		case <-owner.lifetime.Done():
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop(ctx)
		}
	}()
}

// LiveStartContext preserves detached owner startup, while linked startup must
// abort if the trusted peer lifetime ends during provider negotiation.
func LiveStartContext(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	if principal, ok := DeviceFromRequest(r); ok {
		if principal.Lifetime == nil {
			cancel()
			return ctx, cancel
		}
		stop := context.AfterFunc(principal.Lifetime, cancel)
		return ctx, func() { stop(); cancel() }
	}
	return ctx, cancel
}

// LiveAdmissionError describes a linked policy failure without writing a host-
// specific HTTP error envelope. Missing and foreign calls share the same error.
type LiveAdmissionError struct {
	Status  int
	Type    string
	Message string
}

func (e *LiveAdmissionError) Error() string { return e.Message }

// ValidateLiveStart constrains linked calls before session reservation, provider
// startup or tool declarations. Ordinary owner requests are unchanged.
func ValidateLiveStart(r *http.Request, request LiveStartRequest, provider string) *LiveAdmissionError {
	principal, linked := DeviceFromRequest(r)
	if !linked {
		return nil
	}
	if !RouteAllowed(r.Method, r.URL.Path) || r.URL.EscapedPath() != r.URL.Path ||
		principal.Lifetime == nil || principal.Lifetime.Err() != nil {
		return &LiveAdmissionError{http.StatusForbidden, "permission_error", "linked Live requires an active peer connection"}
	}
	if problem := linkedLiveStartProblem(request, provider); problem != "" {
		return &LiveAdmissionError{http.StatusBadRequest, "invalid_request_error", problem}
	}
	return nil
}

func linkedLiveStartProblem(request LiveStartRequest, provider string) string {
	if mode := strings.TrimSpace(request.DelegationMode); mode != "" && mode != "server" {
		return "Linked devices require server-owned Live delegation"
	}
	if request.DelegationContext != "" || len(request.ClientTools) != 0 {
		return "Linked devices cannot provide Live delegation context or client tools"
	}
	transport := request.AudioTransport
	if provider == config.LiveProviderGemini {
		if transport != "http_pcm" {
			return "Linked Gemini Live requires http_pcm audio transport"
		}
	} else if transport != "" && transport != "webrtc" {
		return "This Live provider requires webrtc audio transport"
	}
	return ""
}

// AuthorizeLiveRequest is the handler boundary after peer ingress. It excludes
// WebSocket and client-owned result/tool routes even with direct dispatch. The
// host supplies only a call lookup; device ownership remains private here.
func AuthorizeLiveRequest(r *http.Request, lookup func(string) *LiveOwner) *LiveAdmissionError {
	principal, linked := DeviceFromRequest(r)
	if !linked {
		return nil
	}
	if !RouteAllowed(r.Method, r.URL.Path) || r.URL.EscapedPath() != r.URL.Path {
		return &LiveAdmissionError{http.StatusForbidden, "permission_error", "Live route is not available to Linked devices"}
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/live/sessions/"), "/")
	owner := lookup(id)
	if principal.Lifetime == nil || principal.Lifetime.Err() != nil || owner == nil ||
		owner.deviceID != principal.DeviceID || owner.lifetime == nil || owner.lifetime.Err() != nil {
		return &LiveAdmissionError{http.StatusNotFound, "invalid_request_error", "live session not found"}
	}
	return nil
}
