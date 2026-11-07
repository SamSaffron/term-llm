//go:build !windows

package share

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandPublisherTimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group assertion is Unix-specific")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("TERM_LLM_SHARE_HELPER_CAPTURE", pidFile)
	publisher := commandTestPublisher(t, "timeout", 100*time.Millisecond)
	_, err := publisher.Create(context.Background(), commandTestRequest())
	if err == nil || AsError(err).Code != ErrorTimeout {
		t.Fatalf("error=%v", err)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived timeout cleanup", pid)
}

func processAlive(pid int) bool {
	if killErr := syscall.Kill(pid, 0); killErr != nil {
		return false
	}
	// A killed orphan may remain briefly as a zombie until PID 1 reaps it; it
	// can no longer execute and therefore counts as cleaned up.
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}

func TestCommandPublisherAcceptsZeroExitAfterWaitDelayAndParsesStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pipe-retaining descendant test is Unix-specific")
	}
	publisher := commandTestPublisher(t, "background-pipes", 3*time.Second)
	pidFile := filepath.Join(t.TempDir(), "background.pid")
	t.Setenv("TERM_LLM_SHARE_HELPER_CAPTURE", pidFile)
	started := time.Now()
	capabilities, err := publisher.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if capabilities.Provider.ID != "test-helper" || time.Since(started) >= 4*time.Second {
		t.Fatalf("capabilities=%+v elapsed=%s", capabilities, time.Since(started))
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("background descendant %d survived successful helper cleanup", pid)
	}
}
