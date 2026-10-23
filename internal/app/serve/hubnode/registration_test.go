package hubnode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func hostUnreachableError(ip string) error {
	return &url.Error{Op: "Post", URL: "https://hub.example/api/register-node", Err: &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 443},
		Err:  os.NewSyscallError("connect", syscall.EHOSTUNREACH),
	}}
}

// failingTransport fails the first `failures` requests like a blocked dial.
type failingTransport struct {
	failures int32
	calls    atomic.Int32
	next     http.RoundTripper
}

func (f *failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.calls.Add(1) <= f.failures {
		return nil, hostUnreachableError("10.0.0.51").(*url.Error).Err
	}
	return f.next.RoundTrip(r)
}

func withFastRetry(t *testing.T) {
	t.Helper()
	prevInitial, prevMax := retryInitialDelay, retryMaxDelay
	retryInitialDelay, retryMaxDelay = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() {
		retryInitialDelay, retryMaxDelay = prevInitial, prevMax
	})
}

type recordedLogs struct {
	mu         sync.Mutex
	lines      []string
	registered chan struct{}
}

func (r *recordedLogs) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if r.registered != nil && strings.Contains(line, "hub registration: registered ") {
		close(r.registered)
	}
}

func (r *recordedLogs) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func TestRegistrationRetriesUnreachableHubInBackground(t *testing.T) {
	withFastRetry(t)
	var deregistered atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deregistered.Store(r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	transport := &failingTransport{failures: 3, next: ts.Client().Transport}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := recordedLogs{registered: make(chan struct{})}

	reg, err := Start(ctx, &http.Client{Transport: transport}, ts.URL, "reg-secret", RegisterRequest{ID: "laptop"}, logs.logf)
	if err != nil {
		t.Fatalf("unreachable Hub must not fail startup: %v", err)
	}
	if reg.Registered() {
		t.Fatal("registration reported before the Hub accepted it")
	}
	select {
	case <-logs.registered:
	case <-time.After(10 * time.Second):
		t.Fatalf("background registration did not complete; logs:\n%s", logs.String())
	}
	cancel()
	if attempted, err := reg.DeregisterAfterStop(5 * time.Second); !attempted || err != nil {
		t.Fatalf("deregistration = (%v, %v); logs:\n%s", attempted, err, logs.String())
	}
	if got, _ := deregistered.Load().(string); got != "/api/register-node/laptop" {
		t.Fatalf("Hub saw deregistration path %q", got)
	}
	got := logs.String()
	for _, want := range []string{"serving locally and retrying", "registered laptop with " + ts.URL} {
		if !strings.Contains(got, want) {
			t.Errorf("logs lack %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "; retrying"); n > 0 {
		t.Errorf("identical retry failures logged %d times:\n%s", n, got)
	}
}

func TestRegistrationRejectedIsFatal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid registration token", http.StatusUnauthorized)
	}))
	defer ts.Close()
	reg, err := Start(t.Context(), ts.Client(), ts.URL, "wrong", RegisterRequest{ID: "laptop"}, func(string, ...any) {})
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized || reg != nil {
		t.Fatalf("rejected registration = (%v, %v), want fatal 401", reg, err)
	}
	if !strings.Contains(err.Error(), "hub registration failed: HTTP 401: invalid registration token") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestRegistrationStopsRetryingOnShutdown(t *testing.T) {
	withFastRetry(t)
	transport := &failingTransport{failures: 1 << 30}
	ctx, cancel := context.WithCancel(t.Context())
	reg, err := Start(ctx, &http.Client{Transport: transport}, "https://hub.example", "reg-secret", RegisterRequest{ID: "laptop"}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan bool, 1)
	go func() {
		attempted, _ := reg.DeregisterAfterStop(time.Second)
		done <- attempted
	}()
	select {
	case attempted := <-done:
		if attempted {
			t.Fatal("never-registered node was deregistered")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("background registration did not stop with the serve context")
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"unreachable", hostUnreachableError("10.0.0.51"), true},
		{"timeout", context.DeadlineExceeded, true},
		{"server error", &StatusError{StatusCode: 502}, true},
		{"rate limited", &StatusError{StatusCode: 429}, true},
		{"unauthorized", &StatusError{StatusCode: 401}, false},
		{"conflict", &StatusError{StatusCode: 409}, false},
		{"canceled", fmt.Errorf("post: %w", context.Canceled), false},
		{"bad url", errors.New("parse hub url: invalid"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Retryable(tc.err); got != tc.want {
				t.Fatalf("retryable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLocalNetworkHint(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.51"), Port: 443}, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for _, tc := range []struct {
		name, goos string
		err        error
		want       string
	}{
		{"macOS private address", "darwin", hostUnreachableError("10.0.0.51"), "AllowedWiFiLocalNetworkAddresses -array 10.0.0.51/32"},
		{"macOS link-local IPv6", "darwin", hostUnreachableError("fe80::1"), "-array fe80::1/128"},
		{"macOS public address", "darwin", hostUnreachableError("203.0.113.9"), ""},
		{"Linux private address", "linux", hostUnreachableError("10.0.0.51"), ""},
		{"macOS refused", "darwin", refused, ""},
		{"unrelated", "darwin", errors.New("boom"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LocalNetworkHint(tc.goos, tc.err)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("unexpected hint: %s", got)
				}
				return
			}
			for _, want := range []string{tc.want, "Local Network privacy", "Privacy & Security > Local Network"} {
				if !strings.Contains(got, want) {
					t.Fatalf("hint lacks %q: %s", want, got)
				}
			}
		})
	}
}
