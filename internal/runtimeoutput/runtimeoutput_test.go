package runtimeoutput

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestNestedFallbackBeforeDuringAndAfterUI(t *testing.T) {
	var outside bytes.Buffer
	writer := Fallback(Fallback(&outside))
	write := func(text string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { _, err := writer.Write([]byte(text)); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("nested fallback write deadlocked")
		}
	}
	write("before\n")
	closeLog, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	write("during\n")
	closeLog()
	write("after\n")
	if got := outside.String(); got != "before\nafter\n" {
		t.Fatalf("fallback output: %q", got)
	}
}

func TestLogfWithFallbackLoggerOutsideUI(t *testing.T) {
	var outside bytes.Buffer
	old := log.Writer()
	log.SetOutput(Fallback(&outside))
	defer log.SetOutput(old)
	done := make(chan struct{})
	go func() { Logf("ordinary logger after UI close"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Logf deadlocked calling a fallback-backed standard logger")
	}
	if !strings.Contains(outside.String(), "ordinary logger after UI close") {
		t.Fatalf("logger output: %q", outside.String())
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

func TestBestEffortDiscardOnOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	closeLog, warning, err := StartBestEffort(path)
	if err != nil || warning == nil || !Active() {
		t.Fatalf("fallback = %v, warning = %v, err = %v", Active(), warning, err)
	}
	defer closeLog()
	if _, _, err := StartBestEffort(t.TempDir()); err != ErrAlreadyActive {
		t.Fatalf("overlap = %v", err)
	}
	var outside bytes.Buffer
	old := log.Writer()
	log.SetOutput(&outside)
	defer log.SetOutput(old)
	Logf("discard this diagnostic")
	if n, err := Fallback(&outside).Write([]byte("discard this too")); n != len("discard this too") || err != nil {
		t.Fatalf("fallback write = %d, %v", n, err)
	}
	if outside.Len() != 0 {
		t.Fatalf("diagnostics escaped: %q", outside.String())
	}
	closeLog()
	if Active() {
		t.Fatal("UI still active after closing discard sink")
	}
	Logf("ordinary CLI again")
	if !strings.Contains(outside.String(), "ordinary CLI again") {
		t.Fatalf("CLI logger not restored: %q", outside.String())
	}
}

func TestRawDebugBlocksAndStructuredLevels(t *testing.T) {
	dir := t.TempDir()
	closeLog, err := Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	Printf("\n{\n  \"secret\": \"private\"\n}\n")
	Info("retry", "attempt", 1)
	Error("failure", "detail", "redacted")
	closeLog()
	data, err := os.ReadFile(filepath.Join(dir, "tui.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"\n{\n  \"secret\": \"private\"\n}\n", "level=INFO msg=retry", "level=ERROR msg=failure"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

func TestOldDiscardCloserCannotReleaseNewUI(t *testing.T) {
	oldClose, warning, err := StartBestEffort("")
	if err != nil || warning == nil {
		t.Fatalf("first start: %v, %v", warning, err)
	}
	oldClose()
	newClose, warning, err := StartBestEffort("")
	if err != nil || warning == nil {
		t.Fatalf("second start: %v, %v", warning, err)
	}
	defer newClose()
	oldClose()
	if !Active() {
		t.Fatal("old closer released the new UI")
	}
}
