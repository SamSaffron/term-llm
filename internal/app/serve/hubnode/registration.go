package hubnode

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	retryInitialDelay = 2 * time.Second
	retryMaxDelay     = time.Minute
)

// Retryable reports whether a registration failure can clear without changing
// configuration: an unreachable Hub, a Hub outage or rate limiting. Explicit
// rejections, such as a wrong registration token, are not retryable.
func Retryable(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.StatusCode >= http.StatusInternalServerError ||
			status.StatusCode == http.StatusRequestTimeout ||
			status.StatusCode == http.StatusTooManyRequests
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &urlErr) || errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded)
}

// Registration tracks a reverse node's Hub registration. A Hub that is
// unreachable at startup must not stop the local server: launchd/systemd would
// restart it in a tight loop and the local UI would never come up.
type Registration struct {
	client        *http.Client
	hubURL, token string
	req           RegisterRequest
	logf          func(format string, args ...any)

	done       chan struct{}
	mu         sync.Mutex
	registered bool
}

// Start registers synchronously. Retryable failures are logged and retried in
// the background until registration succeeds or ctx ends; other failures are
// returned so misconfiguration still fails startup. A nil logf uses log.Printf.
func Start(ctx context.Context, client *http.Client, hubURL, registrationToken string, req RegisterRequest, logf func(string, ...any)) (*Registration, error) {
	if logf == nil {
		logf = log.Printf
	}
	r := &Registration{client: client, hubURL: hubURL, token: registrationToken, req: req, logf: logf, done: make(chan struct{})}
	err := Register(ctx, client, hubURL, registrationToken, req)
	if err == nil {
		r.markRegistered()
		close(r.done)
		return r, nil
	}
	if ctx.Err() != nil || !Retryable(err) {
		return nil, err
	}
	logf("hub registration: %v; serving locally and retrying registration in the background", err)
	LogLocalNetworkHint(logf, err)
	go r.retry(ctx, err)
	return r, nil
}

func (r *Registration) retry(ctx context.Context, lastErr error) {
	defer close(r.done)
	delay := retryInitialDelay
	last := lastErr.Error()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		err := Register(ctx, r.client, r.hubURL, r.token, r.req)
		if err == nil {
			r.markRegistered()
			r.logf("hub registration: registered %s with %s", r.req.ID, r.hubURL)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !Retryable(err) {
			r.logf("hub registration: %v; not retrying (fix the Hub registration settings and restart)", err)
			return
		}
		// Log changes rather than every attempt to keep long outages readable.
		if msg := err.Error(); msg != last {
			r.logf("hub registration: %v; retrying", err)
			last = msg
		}
		LogLocalNetworkHint(r.logf, err)
		delay = min(delay*2, retryMaxDelay)
		timer.Reset(delay)
	}
}

func (r *Registration) markRegistered() {
	r.mu.Lock()
	r.registered = true
	r.mu.Unlock()
}

// Registered reports whether the Hub has accepted the registration.
func (r *Registration) Registered() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registered
}

// DeregisterAfterStop waits for background retries, which end with the
// context passed to Start, then removes the node from the Hub if it was
// registered. It reports whether a deregistration was attempted.
func (r *Registration) DeregisterAfterStop(timeout time.Duration) (bool, error) {
	if r == nil {
		return false, nil
	}
	<-r.done
	if !r.Registered() {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return true, Unregister(ctx, nil, r.hubURL, r.token, r.req.ID)
}

// HubURL and NodeID identify the registration in logs.
func (r *Registration) HubURL() string { return r.hubURL }

// NodeID returns the registered node's id.
func (r *Registration) NodeID() string { return r.req.ID }
