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

func TestManagerCloseContextPreventsStartAfterShutdownBegins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}

	t.Setenv("TERM_LLM_WIDGET_TEST_CHILD", "1")

	dir := t.TempDir()
	e := &widgetEntry{
		manifest: &Manifest{
			ID:      "test-widget",
			Title:   "Test Widget",
			Mount:   "test-widget",
			Dir:     dir,
			Command: []string{os.Args[0], "-test.run=TestWidgetChildHTTPServer", "--", "$PORT"},
		},
		state: stateStopped,
	}
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	m := &Manager{
		basePath:    "/chat",
		entries:     map[string]*widgetEntry{"test-widget": e},
		shutdownCtx: shutdownCtx,
		shutdown:    shutdown,
		stopCh:      make(chan struct{}),
	}

	e.startMu.Lock()
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.ensureRunning(e)
	}()

	m.CloseContext(context.Background())
	e.startMu.Unlock()

	err := <-errCh
	if !errors.Is(err, errManagerClosed) {
		t.Fatalf("expected shutdown error, got %v", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateStopped {
		t.Fatalf("expected stopped state, got %v", e.state)
	}
	if e.proc != nil {
		t.Fatal("widget process started after shutdown began")
	}
	if e.proxy != nil {
		t.Fatal("widget proxy initialized after shutdown began")
	}
}

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

	if err := e.startProcess("/chat"); err != nil {
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
