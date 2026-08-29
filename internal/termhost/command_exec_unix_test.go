//go:build unix

package termhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

func TestRunCommandUsesRealExecutablePathAndPreservesArguments(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "capture.log")
	scriptPath := filepath.Join(dir, "bridge")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$LIFECYCLE_TEST_LOG\"\ncat >> \"$LIFECYCLE_TEST_LOG\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIFECYCLE_TEST_LOG", logPath)
	args := []string{"literal argument", "$(touch should-not-run)", "; echo nope"}
	stdin := []byte("{\"schema_version\":1}\n")
	if err := runCommand(context.Background(), scriptPath, args, stdin); err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(args, "\n") + "\n" + string(stdin)
	if string(got) != want {
		t.Fatalf("capture = %q, want %q", got, want)
	}
}

func TestRunCommandTimeoutKillsBackgroundDescendant(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "descendant.pid")
	scriptPath := filepath.Join(dir, "bridge")
	script := "#!/bin/sh\nsleep 30 &\necho $! > \"$1\"\nwait\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := runCommand(ctx, scriptPath, []string{pidPath}, nil)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || err == nil {
		t.Fatalf("runCommand timeout = (%v, %v), want deadline and command error", ctx.Err(), err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("runCommand took %v after timeout; descendant likely retained exec resources", elapsed)
	}
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("parse descendant pid: %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Kill()
	deadline := time.Now().Add(time.Second)
	for {
		err = process.Signal(syscall.Signal(0))
		if err != nil || processIsZombie(pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background descendant %d survived lifecycle command timeout", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processIsZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 {
		return false
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func TestCommandSinkDoesNotInvokeShellForAdversarialContent(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "injected")
	logPath := filepath.Join(dir, "event.json")
	scriptPath := filepath.Join(dir, "bridge")
	script := "#!/bin/sh\ncat > \"$1\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sink, err := newCommandSink(config.LifecycleCommandConfig{
		Name:    "adversarial",
		Command: []string{scriptPath, logPath, "; touch " + marker, "$(touch " + marker + ")"},
	}, runCommand)
	if err != nil {
		t.Fatal(err)
	}
	event := lifecycle.NewEvent(lifecycle.KindState, 9, time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC), lifecycle.Metadata{
		Producer: "term-llm", PID: 1, CWD: dir,
	}, lifecycle.Snapshot{State: lifecycle.Blocked, SessionID: "x; touch " + marker, Message: "$(touch " + marker + ")"})
	if err := sink.Send(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell injection created marker: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded lifecycle.Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode sink event: %v\n%s", err, data)
	}
	if decoded.Message != event.Message || decoded.SessionID != event.SessionID {
		t.Fatalf("decoded event = %#v, want content %#v", decoded, event)
	}
}
