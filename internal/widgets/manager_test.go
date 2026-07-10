package widgets

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestWidgetStopProcessReapsChildProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}

	t.Setenv("TERM_LLM_WIDGET_TEST_CHILD", "1")

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "wrapper.sh")
	script := "#!/bin/sh\n\"$1\" -test.run=TestWidgetChildHTTPServer -- \"$2\" >/dev/null 2>&1 &\nwait\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	e := &widgetEntry{
		manifest: &Manifest{
			ID:      "test-widget",
			Title:   "Test Widget",
			Mount:   "test-widget",
			Dir:     dir,
			Command: []string{scriptPath, os.Args[0], "$PORT"},
		},
		state: stateStopped,
	}

	if err := e.startProcess(context.Background(), "/chat"); err != nil {
		t.Fatalf("startProcess: %v", err)
	}

	e.mu.Lock()
	port := e.port
	e.mu.Unlock()
	if port == 0 {
		t.Fatal("widget did not record a port")
	}

	e.stopProcess()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			return
		}
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("widget child process still serving after stop on %s", url)
}

func TestManagerCloseContextPreventsStartAfterShutdownBegins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}

	t.Setenv("TERM_LLM_WIDGET_TEST_CHILD", "1")

	dir := t.TempDir()
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	m := &Manager{
		basePath:       "/chat",
		entries:        make(map[string]*widgetEntry),
		stopCh:         make(chan struct{}),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}
	e := &widgetEntry{
		manifest: &Manifest{
			ID:      "queued-widget",
			Title:   "Queued Widget",
			Mount:   "queued-widget",
			Dir:     dir,
			Command: []string{os.Args[0], "-test.run=TestWidgetChildHTTPServer", "--", "$PORT"},
		},
		state:   stateStopped,
		lastReq: time.Now(),
	}
	m.entries[e.manifest.Mount] = e

	// Simulate a request queued behind another start attempt. CloseContext takes
	// its one-time snapshot while this goroutine is still waiting on startMu.
	e.startMu.Lock()
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.ensureRunning(e)
	}()
	time.Sleep(50 * time.Millisecond)

	m.CloseContext(context.Background())
	e.startMu.Unlock()

	select {
	case err := <-errCh:
		if !errors.Is(err, errWidgetManagerShuttingDown) {
			e.stopProcess()
			t.Fatalf("ensureRunning error = %v, want %v", err, errWidgetManagerShuttingDown)
		}
	case <-time.After(2 * time.Second):
		e.stopProcess()
		t.Fatal("ensureRunning did not return after shutdown began")
	}

	e.mu.Lock()
	proc := e.proc
	state := e.state
	e.mu.Unlock()
	if proc != nil {
		e.stopProcess()
		t.Fatal("widget process started after shutdown began")
	}
	if state != stateStopped {
		t.Fatalf("widget state = %v, want stopped", state)
	}
}

func TestManagerReapIdleKeepsActiveProxyRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	defer releaseOnce.Do(func() { close(release) })

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	m := &Manager{
		basePath: "/chat",
		entries:  make(map[string]*widgetEntry),
	}
	e := &widgetEntry{
		manifest: &Manifest{
			ID:    "slow-widget",
			Title: "Slow Widget",
			Mount: "slow-widget",
		},
		state:   stateRunning,
		proxy:   httputil.NewSingleHostReverseProxy(targetURL),
		lastReq: time.Now().Add(-idleTimeout - time.Minute),
	}
	m.entries[e.manifest.Mount] = e

	errCh := make(chan error, 1)
	go func() {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/widgets/slow-widget/stream", nil)
		m.Proxy("slow-widget", rr, req)
		if rr.Code != http.StatusOK {
			errCh <- fmt.Errorf("proxy status = %d, want %d: %q", rr.Code, http.StatusOK, rr.Body.String())
			return
		}
		if got := rr.Body.String(); got != "ok" {
			errCh <- fmt.Errorf("proxy body = %q, want %q", got, "ok")
			return
		}
		errCh <- nil
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("proxy request did not reach upstream")
	}

	e.mu.Lock()
	e.lastReq = time.Now().Add(-idleTimeout - time.Minute)
	inFlight := e.inFlight
	e.mu.Unlock()
	if inFlight != 1 {
		releaseOnce.Do(func() { close(release) })
		t.Fatalf("inFlight = %d, want 1 while proxy request is active", inFlight)
	}

	m.reapIdle()

	e.mu.Lock()
	state := e.state
	proxy := e.proxy
	inFlight = e.inFlight
	e.mu.Unlock()
	if state != stateRunning {
		releaseOnce.Do(func() { close(release) })
		t.Fatalf("state = %v, want running while proxy request is active", state)
	}
	if proxy == nil {
		releaseOnce.Do(func() { close(release) })
		t.Fatal("proxy was cleared while request was active")
	}
	if inFlight != 1 {
		releaseOnce.Do(func() { close(release) })
		t.Fatalf("inFlight = %d, want 1 while proxy request is active", inFlight)
	}

	beforeRelease := time.Now()
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy request did not finish")
	}

	e.mu.Lock()
	inFlight = e.inFlight
	lastReq := e.lastReq
	e.mu.Unlock()
	if inFlight != 0 {
		t.Fatalf("inFlight = %d, want 0 after proxy request finishes", inFlight)
	}
	if lastReq.Before(beforeRelease) {
		t.Fatalf("lastReq = %v, want refresh after request completion at %v", lastReq, beforeRelease)
	}

	e.mu.Lock()
	e.lastReq = time.Now().Add(-idleTimeout - time.Minute)
	e.mu.Unlock()
	m.reapIdle()

	e.mu.Lock()
	state = e.state
	e.mu.Unlock()
	if state != stateStopped {
		t.Fatalf("state = %v, want stopped after idle request completes", state)
	}
}

func TestWidgetChildHTTPServer(t *testing.T) {
	if os.Getenv("TERM_LLM_WIDGET_TEST_CHILD") != "1" {
		t.Skip("helper process")
	}

	var port string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			port = os.Args[i+1]
			break
		}
	}
	if port == "" {
		log.Fatal("missing port")
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, handler))
}
