package runtimeoutput

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestInteractiveDiagnosticsRouteAndRestore(t *testing.T) {
	var outside bytes.Buffer
	old := log.Writer()
	log.SetOutput(&outside)
	defer log.SetOutput(old)
	Logf("before")
	dir := t.TempDir()
	closeLog, err := Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(dir); err == nil {
		t.Fatal("concurrent UI log should be rejected")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Debug("invalid skill", "path", "example")
			Warn("runtime warning", "count", 1)
			Printf("provider diagnostic\n")
			Logf("internal log")
		}()
	}
	wg.Wait()
	closeLog()
	closeLog() // Closing a finished UI must be harmless.
	Logf("after")
	if got := outside.String(); !strings.Contains(got, "before") || !strings.Contains(got, "after") || strings.Contains(got, "internal log") {
		t.Fatalf("CLI log output: %q", got)
	}
	path := filepath.Join(dir, "tui.log")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions: %v", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"invalid skill", "runtime warning", "provider diagnostic", "internal log"} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("diagnostic %q missing from log", text)
		}
	}
	if strings.Contains(string(data), "before") || strings.Contains(string(data), "after") {
		t.Fatalf("non-interactive output leaked into UI log")
	}
}

func TestDetachedWarningFallbackWhileAnotherUIRuns(t *testing.T) {
	var outside bytes.Buffer
	writer := Fallback(&outside)
	if _, err := writer.Write([]byte("before\n")); err != nil {
		t.Fatal(err)
	}
	closeLog, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("old session cleanup\n")); err != nil {
		t.Fatal(err)
	}
	closeLog()
	if _, err := writer.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	if got := outside.String(); got != "before\nafter\n" {
		t.Fatalf("fallback output: %q", got)
	}
}

func TestInteractiveLogRestrictsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tui.log")
	if err := os.WriteFile(path, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	closeLog, err := Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	closeLog()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("existing log permissions: %v", info.Mode())
	}
}

func TestInteractiveLogOpenFailureDoesNotCaptureOutsideLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if closeLog, err := Start(path); err == nil {
		closeLog()
		t.Fatal("expected open failure")
	}
	var outside bytes.Buffer
	old := log.Writer()
	log.SetOutput(&outside)
	defer log.SetOutput(old)
	Logf("still routed to ordinary logger")
	if !strings.Contains(outside.String(), "still routed") {
		t.Fatalf("outside log: %q", outside.String())
	}
}
