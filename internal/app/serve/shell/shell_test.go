//go:build !windows && !plan9 && !js && !wasip1

package shell

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestServeShellReplayResetAndExit(t *testing.T) {
	shell := NewShell("sh_test", "session", t.TempDir())
	payload := bytes.Repeat([]byte("x"), ReplayBytes+123)
	shell.AppendOutput(payload)
	snapshot := shell.Snapshot(0)
	if !snapshot.Reset || snapshot.BaseOffset != 123 || snapshot.NextOffset != int64(len(payload)) {
		t.Fatalf("reset snapshot = %+v", snapshot)
	}
	snapshot = shell.Snapshot(snapshot.BaseOffset)
	if snapshot.Reset || snapshot.DataOffset != 123 || len(snapshot.Data) != chunkBytes {
		t.Fatalf("replay snapshot = offset %d len %d reset %t", snapshot.DataOffset, len(snapshot.Data), snapshot.Reset)
	}
	shell.mu.Lock()
	shell.exited = true
	shell.exitCode = 7
	shell.notifyLocked()
	shell.mu.Unlock()
	exit := shell.Snapshot(shell.nextOffset)
	if !exit.Exited || exit.ExitCode != 7 {
		t.Fatalf("exit snapshot = %+v", exit)
	}
}

func TestServeShellReplayCompactsDroppedPrefixesInBatches(t *testing.T) {
	const totalBytes = 3*ReplayBytes + 3*chunkBytes + 123
	all := make([]byte, totalBytes)
	for i := range all {
		all[i] = byte((i*31 + 17) % 251)
	}

	shell := NewShell("sh_compaction", "session", t.TempDir())
	for start := 0; start < len(all); start += chunkBytes {
		end := min(start+chunkBytes, len(all))
		shell.AppendOutput(all[start:end])
	}

	wantBase := int64(len(all) - ReplayBytes)
	wantOutputStart := len(all) - 3*ReplayBytes
	shell.mu.Lock()
	if shell.baseOffset != wantBase || shell.nextOffset != int64(len(all)) {
		t.Fatalf("offsets = base %d next %d, want base %d next %d", shell.baseOffset, shell.nextOffset, wantBase, len(all))
	}
	if shell.outputStart != wantOutputStart || len(shell.output)-shell.outputStart != ReplayBytes {
		t.Fatalf("storage = start %d active %d, want start %d active %d", shell.outputStart, len(shell.output)-shell.outputStart, wantOutputStart, ReplayBytes)
	}

	redactionStart := shell.baseOffset + 1234
	redactionEnd := redactionStart + 32
	replacement := []byte("shared")
	shell.replaceReplayRangeLocked(redactionStart, redactionEnd, replacement)
	shell.mu.Unlock()

	want := append([]byte(nil), all[len(all)-ReplayBytes:]...)
	redactionIndex := int(redactionStart - wantBase)
	clear(want[redactionIndex : redactionIndex+int(redactionEnd-redactionStart)])
	copy(want[redactionIndex:], replacement)

	reset := shell.Snapshot(0)
	if !reset.Reset || reset.BaseOffset != wantBase || reset.DataOffset != wantBase {
		t.Fatalf("reset snapshot = reset %t base %d data %d, want base/data %d", reset.Reset, reset.BaseOffset, reset.DataOffset, wantBase)
	}

	var replay []byte
	for offset := wantBase; offset < int64(len(all)); {
		snapshot := shell.Snapshot(offset)
		if snapshot.Reset || snapshot.DataOffset != offset || len(snapshot.Data) == 0 || len(snapshot.Data) > chunkBytes {
			t.Fatalf("snapshot at %d = reset %t data offset %d len %d", offset, snapshot.Reset, snapshot.DataOffset, len(snapshot.Data))
		}
		replay = append(replay, snapshot.Data...)
		offset += int64(len(snapshot.Data))
	}
	if !bytes.Equal(replay, want) {
		t.Fatalf("chunked replay differs: got %d bytes, want %d", len(replay), len(want))
	}

	// A large write must reset the logical prefix left by small writes.
	large := bytes.Repeat([]byte("z"), ReplayBytes+17)
	shell.AppendOutput(large)
	shell.AppendOutput([]byte("tail"))
	snapshot := shell.Snapshot(0)
	wantBase = int64(len(all) + len(large) + 4 - ReplayBytes)
	if !snapshot.Reset || snapshot.BaseOffset != wantBase || !bytes.Equal(snapshot.Data, large[:chunkBytes]) {
		t.Fatalf("large write after compaction: base %d, reset %t, data length %d", snapshot.BaseOffset, snapshot.Reset, len(snapshot.Data))
	}
	if tail := shell.Snapshot(snapshot.NextOffset - 4); string(tail.Data) != "tail" {
		t.Fatalf("replay tail = %q, want tail", tail.Data)
	}
}

func BenchmarkServeShellReplaySmallChunks(b *testing.B) {
	const bytesPerIteration = 256 << 20
	chunk := bytes.Repeat([]byte("x"), chunkBytes)
	shell := NewShell("sh_benchmark", "session", b.TempDir())
	shell.AppendOutput(bytes.Repeat([]byte("p"), ReplayBytes))
	b.SetBytes(bytesPerIteration)
	b.ResetTimer()
	for range b.N {
		for written := 0; written < bytesPerIteration; written += len(chunk) {
			shell.AppendOutput(chunk)
		}
	}
}

func TestServeShellProcessDrainsHighVolumeOutputBeforeExit(t *testing.T) {
	dir := t.TempDir()
	const repetitions = 4_096
	const block = "0123456789abcdef0123456789abcdef"
	payload := strings.Repeat(block, repetitions)
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "write-and-exit.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat output.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", script)

	var outputMu sync.Mutex
	var output bytes.Buffer
	process, err := startProcess(dir, 80, 24, func(data []byte) {
		// Simulate a temporarily busy consumer so output can still be queued in
		// the PTY when the producer exits.
		time.Sleep(125 * time.Millisecond)
		outputMu.Lock()
		_, _ = output.Write(data)
		outputMu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(process.Close)
	select {
	case exit := <-process.Done():
		if exit.Code != 0 {
			t.Fatalf("exit = %+v", exit)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shell process did not finish")
	}
	outputMu.Lock()
	got := output.Len()
	outputMu.Unlock()
	if want := repetitions * len(block); got != want {
		t.Fatalf("drained output bytes = %d, want %d", got, want)
	}
}

func TestServeShellCleanupOnIdleSessionDeletionAndShutdown(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	var exists atomic.Bool
	exists.Store(true)
	manager := NewManager(10*time.Millisecond, func(string) bool { return exists.Load() })
	cwd := t.TempDir()
	first, _, err := manager.Create("idle", cwd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	first.lastUsed = time.Now().Add(-time.Hour)
	first.mu.Unlock()
	manager.evictExpired()
	waitForShellExit(t, first)

	second, _, err := manager.Create("deleted", cwd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	exists.Store(false)
	manager.evictExpired()
	waitForShellExit(t, second)

	exists.Store(true)
	third, _, err := manager.Create("shutdown", cwd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	manager.Close()
	waitForShellExit(t, third)
	if _, _, err := manager.Create("late", cwd, 80, 24); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after shutdown error = %v", err)
	}
}

func waitForShellExit(t *testing.T, shell *Shell) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !shell.Alive() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shell process did not exit")
}

func TestServeShellProcessGroupCleanup(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	manager := NewManager(time.Minute, nil)
	defer manager.Close()
	shell, _, err := manager.Create("group", t.TempDir(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	// Whitespace after $! avoids interactive Bash history expansion of !;.
	if err := shell.write([]byte("sleep 30 & child=$! ; printf '__child__%s\\n' \"$child\"\n")); err != nil {
		t.Fatal(err)
	}
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := shell.Snapshot(0)
		text := string(snapshot.Data)
		if marker := strings.LastIndex(text, "__child__"); marker >= 0 {
			_, _ = fmt.Sscanf(text[marker:], "__child__%d", &childPID)
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatalf("did not capture background child PID; shell output: %q", shell.Snapshot(0).Data)
	}
	manager.CloseSession("group")
	waitForShellExit(t, shell)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !serveShellTestProcessRunning(childPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background process %d survived shell cleanup", childPID)
}

func serveShellTestProcessRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	// Linux keeps a killed orphan visible briefly as a zombie until PID 1
	// reaps it. It can no longer execute and therefore counts as terminated.
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}
