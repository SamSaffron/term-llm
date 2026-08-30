package widgets

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestWidgetStopProcessReapsChildProcessGroup(t *testing.T) {
	t.Run("entire process group", func(t *testing.T) {
		testWidgetStopProcessReapsChildProcessGroup(t)
	})
}

func testWidgetStopProcessReapsChildProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}

	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep executable unavailable: %v", err)
	}
	leader := exec.Command(sleep, "60")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatalf("start process-group leader: %v", err)
	}
	leaderDone := make(chan struct{})
	go func() {
		_ = leader.Wait()
		close(leaderDone)
	}()

	member := exec.Command(sleep, "60")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := member.Start(); err != nil {
		_ = leader.Process.Kill()
		<-leaderDone
		t.Fatalf("start process-group member: %v", err)
	}
	memberDone := make(chan struct{})
	go func() {
		_ = member.Wait()
		close(memberDone)
	}()

	e := &widgetEntry{
		manifest: &Manifest{ID: "test-widget", Mount: "test-widget", Dir: t.TempDir()},
		state:    stateRunning,
		proc:     leader.Process,
		procDone: leaderDone,
	}
	e.stopProcess()

	select {
	case <-memberDone:
	case <-time.After(100 * time.Millisecond):
		_ = member.Process.Kill()
		t.Fatal("widget process-group member survived stop")
	}
}

func TestWidgetStopSerializesConcurrentRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups and Unix sockets are Unix-specific")
	}

	t.Setenv("TERM_LLM_WIDGET_TEST_CHILD", "1")
	termMarker := filepath.Join(t.TempDir(), "term-received")
	t.Setenv("TERM_LLM_WIDGET_TEST_TERM_DELAY", "50ms")
	t.Setenv("TERM_LLM_WIDGET_TEST_TERM_MARKER", termMarker)

	m := &Manager{
		basePath: "/chat",
		entries:  make(map[string]*widgetEntry),
	}
	e := &widgetEntry{
		manifest: &Manifest{
			ID:      "restart-widget",
			Title:   "Restart Widget",
			Mount:   "restart-widget",
			Dir:     t.TempDir(),
			Command: []string{os.Args[0], "-test.run=TestWidgetChildHTTPServer", "--", "$SOCKET"},
		},
		state:   stateStopped,
		lastReq: time.Now(),
	}
	m.entries[e.manifest.Mount] = e

	if err := m.ensureRunning(e); err != nil {
		t.Fatalf("start initial widget: %v", err)
	}
	defer e.stopProcess()

	e.mu.Lock()
	oldProc := e.proc
	e.mu.Unlock()
	if oldProc == nil {
		t.Fatal("initial widget did not record its process")
	}

	stopDone := make(chan struct{})
	go func() {
		e.stopProcess()
		close(stopDone)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(termMarker); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat termination marker: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("stopping widget did not receive SIGTERM")
		}
		time.Sleep(10 * time.Millisecond)
	}

	startDone := make(chan error, 1)
	go func() {
		startDone <- m.ensureRunning(e)
	}()

	select {
	case err := <-startDone:
		t.Fatalf("replacement start completed before old process exited: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stopProcess did not finish")
	}
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("start replacement widget: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not start after old process exited")
	}

	e.mu.Lock()
	newProc := e.proc
	state := e.state
	proxy := e.proxy
	e.mu.Unlock()
	if state != stateRunning {
		t.Fatalf("replacement state = %v, want running", state)
	}
	if newProc == nil || newProc.Pid == oldProc.Pid {
		t.Fatalf("replacement process = %v, old PID = %d", newProc, oldProc.Pid)
	}
	if proxy == nil {
		t.Fatal("replacement proxy was cleared by stale process cleanup")
	}
	if err := oldProc.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("old process is not reaped: signal error = %v", err)
	}

	socketPath := filepath.Join(socketRuntimeDir, e.manifest.ID+".sock")
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("replacement socket is unavailable: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/widgets/restart-widget/", nil)
	m.Proxy(e.manifest.Mount, rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("replacement response = (%d, %q), want (200, %q)", rr.Code, rr.Body.String(), "ok")
	}
}

func TestManagerStopAllKeepsWidgetsRegisteredForLazyRestart(t *testing.T) {
	m := &Manager{entries: make(map[string]*widgetEntry)}
	for _, mount := range []string{"alpha", "beta"} {
		m.entries[mount] = &widgetEntry{
			manifest: &Manifest{ID: mount, Mount: mount, Title: mount},
			state:    stateRunning,
			proxy:    &httputil.ReverseProxy{},
			port:     9000,
		}
	}

	m.StopAll()

	statuses := m.Status()
	if len(statuses) != 2 {
		t.Fatalf("status count = %d, want 2", len(statuses))
	}
	for _, status := range statuses {
		if status.State != "stopped" {
			t.Errorf("widget %q state = %q, want stopped", status.Mount, status.State)
		}
		if status.Port != 0 {
			t.Errorf("widget %q port = %d, want 0", status.Mount, status.Port)
		}
	}
	if len(m.entries) != 2 {
		t.Fatalf("registered widget count = %d, want 2", len(m.entries))
	}
}

func TestManagerCloseContextPreventsStartAfterShutdownBegins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}

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

	closeDone := make(chan struct{})
	go func() {
		m.CloseContext(context.Background())
		close(closeDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !m.isShuttingDown() {
		if time.Now().After(deadline) {
			e.startMu.Unlock()
			t.Fatal("shutdown did not begin")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseContext did not finish")
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
	var address string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			address = os.Args[i+1]
			break
		}
	}
	if address == "" {
		t.Skip("helper process")
	}

	if delayValue := os.Getenv("TERM_LLM_WIDGET_TEST_TERM_DELAY"); delayValue != "" {
		delay, err := time.ParseDuration(delayValue)
		if err != nil {
			log.Fatalf("parse termination delay: %v", err)
		}
		termCh := make(chan os.Signal, 1)
		signal.Notify(termCh, syscall.SIGTERM)
		go func() {
			<-termCh
			if marker := os.Getenv("TERM_LLM_WIDGET_TEST_TERM_MARKER"); marker != "" {
				if err := os.WriteFile(marker, nil, 0600); err != nil {
					log.Printf("write termination marker: %v", err)
				}
			}
			time.Sleep(delay)
			os.Exit(0)
		}()
	}

	network := "tcp"
	if os.Getenv("TERM_LLM_WIDGET_SOCKET") != "" {
		network = "unix"
	} else {
		address = "127.0.0.1:" + address
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		log.Fatal(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	log.Fatal(http.Serve(listener, handler))
}
