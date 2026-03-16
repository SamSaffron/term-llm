package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveCommand_TimesOutAndKillsProcessGroup(t *testing.T) {
	origTimeout := resolveExecTimeout
	origWaitDelay := resolveExecWaitDelay
	resolveExecTimeout = 50 * time.Millisecond
	resolveExecWaitDelay = 50 * time.Millisecond
	defer func() {
		resolveExecTimeout = origTimeout
		resolveExecWaitDelay = origWaitDelay
	}()

	start := time.Now()
	_, err := resolveCommand("sleep 2 & wait")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want timeout message", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("resolveCommand took %s after timeout, want under 500ms", elapsed)
	}
}

func TestResolveCommand_RejectsOversizedOutput(t *testing.T) {
	origLimit := resolveExecOutputLimit
	resolveExecOutputLimit = 32
	defer func() {
		resolveExecOutputLimit = origLimit
	}()

	_, err := resolveCommand("i=0; while [ $i -lt 200 ]; do printf x; i=$((i+1)); done")
	if err == nil {
		t.Fatalf("expected output limit error")
	}
	if !strings.Contains(err.Error(), "output exceeded 32 bytes") {
		t.Fatalf("error = %q, want output limit message", err)
	}
}

func TestResolveValue_FileContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(path, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := ResolveValue("file://" + path)
	if err != nil {
		t.Fatalf("ResolveValue(file) error: %v", err)
	}
	if got != "secret-token" {
		t.Fatalf("ResolveValue(file) = %q, want %q", got, "secret-token")
	}
}

func TestResolveValue_FileJSONFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	content := `{"access_token":"abc123","nested":{"value":"xyz"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := ResolveValue("file://" + path + "#access_token")
	if err != nil {
		t.Fatalf("ResolveValue(file#access_token) error: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("ResolveValue(file#access_token) = %q, want %q", got, "abc123")
	}

	got, err = ResolveValue("file://" + path + "#nested.value")
	if err != nil {
		t.Fatalf("ResolveValue(file#nested.value) error: %v", err)
	}
	if got != "xyz" {
		t.Fatalf("ResolveValue(file#nested.value) = %q, want %q", got, "xyz")
	}
}
