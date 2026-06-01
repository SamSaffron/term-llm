package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCreateStdioTransport_InheritsEnv(t *testing.T) {
	// Server with custom env should inherit parent PATH
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
			Env: map[string]string{
				"CUSTOM_VAR": "custom_value",
			},
		},
	}

	transport := client.createStdioTransport(context.Background())
	ct, ok := transport.(*sdkmcp.CommandTransport)
	if !ok {
		t.Fatal("expected sdkmcp.CommandTransport")
	}

	env := ct.Command.Env
	if env == nil {
		t.Fatal("expected non-nil env when config has env vars")
	}

	// Check that parent PATH is inherited
	hasPath := false
	hasCustom := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
		if e == "CUSTOM_VAR=custom_value" {
			hasCustom = true
		}
	}

	if !hasPath {
		t.Error("parent PATH not inherited in subprocess env")
	}
	if !hasCustom {
		t.Error("custom env var not set")
	}
}

func TestCreateStdioTransport_NoEnvNil(t *testing.T) {
	// Server with no custom env should leave cmd.Env nil (inherit all)
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
		},
	}

	transport := client.createStdioTransport(context.Background())
	ct, ok := transport.(*sdkmcp.CommandTransport)
	if !ok {
		t.Fatal("expected sdkmcp.CommandTransport")
	}

	if ct.Command.Env != nil {
		t.Error("expected nil env when no config env vars (inherits parent automatically)")
	}
}

func TestCreateStdioTransport_EmptyEnvNil(t *testing.T) {
	// Server with empty env map should also leave cmd.Env nil
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
			Env:     map[string]string{},
		},
	}

	transport := client.createStdioTransport(context.Background())
	ct, ok := transport.(*sdkmcp.CommandTransport)
	if !ok {
		t.Fatal("expected sdkmcp.CommandTransport")
	}

	if ct.Command.Env != nil {
		t.Error("expected nil env when env map is empty")
	}
}

func TestCreateStdioTransport_EnvOverridesParent(t *testing.T) {
	// Set a known env var, then override it
	os.Setenv("TEST_MCP_VAR", "original")
	defer os.Unsetenv("TEST_MCP_VAR")

	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Env: map[string]string{
				"TEST_MCP_VAR": "overridden",
			},
		},
	}

	transport := client.createStdioTransport(context.Background())
	ct := transport.(*sdkmcp.CommandTransport)

	// The overridden value should appear (last wins in exec.Cmd)
	found := false
	for _, e := range ct.Command.Env {
		if e == "TEST_MCP_VAR=overridden" {
			found = true
		}
	}
	if !found {
		t.Error("expected overridden env var in subprocess env")
	}
}

func TestCreateStdioTransport_ConfiguresProcessGroupCancellation(t *testing.T) {
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
		},
	}

	transport := client.createStdioTransport(context.Background())
	ct := transport.(*sdkmcp.CommandTransport)

	if ct.Command.Cancel == nil {
		t.Fatalf("expected subprocess cancel hook to be configured")
	}
	if ct.Command.SysProcAttr == nil || !ct.Command.SysProcAttr.Setpgid {
		t.Fatalf("expected subprocess to run in its own process group")
	}
	if ct.Command.WaitDelay != time.Second {
		t.Fatalf("WaitDelay = %v, want %v", ct.Command.WaitDelay, time.Second)
	}
	if client.processCancel == nil {
		t.Fatalf("expected client to retain a stdio process cancel func")
	}
}

func TestClientStop_CancelsStdioProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires sh")
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	client := NewClient("greeter", ServerConfig{
		Command: "sh",
		Args: []string{
			"-c",
			"sleep 30 >/dev/null 2>&1 & echo $! > \"$1\"; exec \"$2\"",
			"sh",
			pidFile,
			os.Args[0],
		},
		Env: map[string]string{
			runMCPManagerTestServerEnv: "1",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pid := waitForRecordedPID(t, pidFile)
	defer killProcessIfRunning(pid)

	if err := client.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	waitForMCPProcessExit(t, pid)
}

func waitForRecordedPID(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pidText := strings.TrimSpace(string(data))
			if pidText != "" {
				pid, convErr := strconv.Atoi(pidText)
				if convErr != nil {
					t.Fatalf("parse pid %q: %v", pidText, convErr)
				}
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func waitForMCPProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mcpProcessHasExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for process %d to exit", pid)
}

func killProcessIfRunning(pid int) {
	if pid <= 0 || mcpProcessHasExited(pid) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func mcpProcessHasExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS == "linux" {
		state, ok := linuxMCPProcState(pid)
		return ok && state == 'Z'
	}
	return false
}

func linuxMCPProcState(pid int) (byte, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return mcpProcStatState(data)
}

func mcpProcStatState(data []byte) (byte, bool) {
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end == -1 || end+2 >= len(stat) {
		return 0, false
	}
	return stat[end+2], true
}
