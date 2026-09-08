//go:build linux

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func testRecord(t *testing.T, pid int) Record {
	t.Helper()
	start, err := identity(pid)
	if err != nil {
		t.Fatal(err)
	}
	return Record{PID: pid, Start: start, Instance: "test-instance", BuildID: "test-build", Executable: "/test/term-llm", Phase: "ready"}
}

func TestListCleansDeadAndReusedIdentity(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	self := testRecord(t, os.Getpid())
	if err := write(root, self); err != nil {
		t.Fatal(err)
	}
	records, err := List()
	if err != nil || len(records) != 1 {
		t.Fatalf("live record: %+v, %v", records, err)
	}
	self.Start = "previous-owner"
	if err := write(root, self); err != nil {
		t.Fatal(err)
	}
	records, err = List()
	if err != nil || len(records) != 0 {
		t.Fatalf("stale identity: %+v, %v", records, err)
	}
	if _, err := root.Stat(filename(self)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale file retained: %v", err)
	}

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	dead := testRecord(t, child.Process.Pid)
	if err := write(root, dead); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	records, err = List()
	if err != nil || len(records) != 0 {
		t.Fatalf("dead record: %+v, %v", records, err)
	}
	if _, err := root.Stat(filename(dead)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead file retained: %v", err)
	}
}

func TestRegistryRejectsUnsafeDirectory(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	path := filepath.Join(base, "term-llm-processes")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if root, err := registry(); err == nil {
		root.Close()
		t.Fatal("accepted nonprivate registry")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	if root, err := registry(); err == nil {
		root.Close()
		t.Fatal("accepted symlink registry")
	}
}

func TestPublisherStopDoesNotResurrect(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	for i := 0; i < 20; i++ {
		p := Start("test", "")
		State("test", "ready", "")
		p.Stop()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			t.Fatal("publisher did not stop")
		}
		if i == 0 {
			if _, err := os.Stat(filepath.Join(runtimeDir, "term-llm-processes")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("short-lived publisher touched registry: %v", err)
			}
		}
		records, err := List()
		if err != nil || len(records) != 0 {
			t.Fatalf("record resurrected after Stop: %+v, %v", records, err)
		}
	}
}

func TestPublisherLifecycle(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	p := Start("test", "")
	defer p.Stop()
	State("test", "ready", "")
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		records, err := List()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 && records[0].Phase == "ready" {
			if records[0].Instance == "" || records[0].BuildID == "" {
				t.Fatalf("incomplete: %+v", records[0])
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("publisher never became discoverable")
		case <-tick.C:
		}
	}
	p.Stop()
	<-p.done
	records, err := List()
	if err != nil || len(records) != 0 {
		t.Fatalf("normal cleanup: %+v %v", records, err)
	}
}

func TestRestartRefusesSelfAndUnsupported(t *testing.T) {
	_, err := Restart(context.Background(), Record{PID: os.Getpid(), Phase: "ready"}, true)
	if err == nil {
		t.Fatal("accepted self restart")
	}
	_, err = Restart(context.Background(), Record{PID: 123, Phase: "deferred"}, true)
	if err == nil {
		t.Fatal("accepted deferred restart")
	}
}

// This subprocess registers exactly like the executable, then self-execs on
// SIGUSR2. The parent only signals the child it created, never a live service.
func TestProcessRestartHelper(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_PROCESS_HELPER") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR2)
	p := Start("sandbox", "")
	defer p.Stop()
	State("sandbox", "ready", "")
	<-signals
	State("sandbox", "replacing", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRealSelfExec(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	child := exec.Command(os.Args[0], "-test.run=^TestProcessRestartHelper$")
	child.Env = append(os.Environ(), "TERM_LLM_TEST_PROCESS_HELPER=1")
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	var before Record
	for before.PID == 0 {
		records, err := List()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.PID == child.Process.Pid && r.Phase == "ready" {
				before = r
			}
		}
		if before.PID != 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child never published ready")
		case <-tick.C:
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var phases []string
	after, err := RestartWithProgress(ctx, before, true, func(phase string) { phases = append(phases, phase) })
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) < 3 || phases[0] != "checking" || phases[1] != "waiting" || phases[len(phases)-1] != "verifying" {
		t.Fatalf("unexpected restart progress: %v", phases)
	}
	if after.PID != before.PID || after.Start != before.Start || after.Instance == before.Instance || after.BuildID != before.BuildID {
		t.Fatalf("bad handoff: before=%+v after=%+v", before, after)
	}
	// The old instance record must not authorize a second signal.
	if _, err := Restart(ctx, before, true); err == nil {
		t.Fatal("accepted stale instance")
	}
	// A pidfd/start match alone must not authorize an image that no longer
	// matches the registry (for example, exec into an unrelated program).
	root, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mismatched := after
	mismatched.BuildID = "different-loaded-image"
	if err := write(root, mismatched); err != nil {
		t.Fatal(err)
	}
	if _, err := Restart(ctx, mismatched, false); err == nil {
		t.Fatal("signalled a process whose loaded image did not match the record")
	}
	if err := write(root, after); err != nil {
		t.Fatal(err)
	}
	t.Logf("same PID %d, new instance %s -> %s, ready", before.PID, before.Instance, after.Instance)
}
