package widgets

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
