//go:build !windows

package mcp

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/term-llm/term-llm/internal/procutil"
	"github.com/term-llm/term-llm/internal/testutil/processinfo"
)

type fakeMCPConnection struct{}

func (fakeMCPConnection) Read(context.Context) (jsonrpc.Message, error) { return nil, io.EOF }
func (fakeMCPConnection) Write(context.Context, jsonrpc.Message) error  { return nil }
func (fakeMCPConnection) Close() error                                  { return nil }
func (fakeMCPConnection) SessionID() string                             { return "" }

func TestOwnedCommandConnSendsSIGTERMAfterEOF(t *testing.T) {
	dir := t.TempDir()
	signalPath := filepath.Join(dir, "term")
	pidPath := filepath.Join(dir, "pid")
	script := `trap 'printf term > "$1"; exit 0' TERM; printf '%s' $$ > "$2"; while :; do sleep 10; done`
	cmd := exec.Command("sh", "-c", script, "sh", signalPath, pidPath)
	owned, err := procutil.PrepareCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitMCPPID(t, pidPath)
	conn := &ownedCommandConn{Connection: fakeMCPConnection{}, cmd: owned, terminate: time.Second}
	done := make(chan error, 1)
	go func() { done <- conn.Close() }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Close did not finish after SIGTERM")
	}
	if data, err := os.ReadFile(signalPath); err != nil || strings.TrimSpace(string(data)) != "term" {
		t.Fatalf("SIGTERM acknowledgement = %q, %v", data, err)
	}
	if !processinfo.ProcessHasExited(pid) {
		t.Fatalf("process %d survived graceful shutdown", pid)
	}
}

func TestOwnedCommandConnEscalatesStubbornServer(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "pid")
	script := `trap '' TERM; printf '%s' $$ > "$1"; while :; do sleep 10; done`
	cmd := exec.Command("sh", "-c", script, "sh", pidPath)
	owned, err := procutil.PrepareCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitMCPPID(t, pidPath)
	conn := &ownedCommandConn{Connection: fakeMCPConnection{}, cmd: owned, terminate: 100 * time.Millisecond}
	start := time.Now()
	if err := conn.Close(); err == nil {
		// A killed process may produce an ExitError; this test is about the
		// escalation bound, not the exit status text.
		t.Log("Close returned no exit error after SIGKILL")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stubborn server cleanup took %s", elapsed)
	}
	if !processinfo.ProcessHasExited(pid) {
		t.Fatalf("stubborn process %d survived SIGKILL", pid)
	}
}

func waitMCPPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}
