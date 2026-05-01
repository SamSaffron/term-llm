package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunAgentScriptTool(t *testing.T) {
	// Create a temp agent directory with test scripts
	agentDir := t.TempDir()

	// Create a simple script that echoes its args
	echoScript := filepath.Join(agentDir, "echo.sh")
	os.WriteFile(echoScript, []byte("#!/bin/sh\necho \"hello $@\"\n"), 0755)

	// Create a script that exits with non-zero
	failScript := filepath.Join(agentDir, "fail.sh")
	os.WriteFile(failScript, []byte("#!/bin/sh\necho 'failing'\nexit 42\n"), 0755)

	limits := DefaultOutputLimits()

	tests := []struct {
		name     string
		agentDir string
		args     RunAgentScriptArgs
		wantErr  string // substring expected in output (empty = no error)
		wantOut  string // substring expected in output
		wantExit string // exit code substring
	}{
		{
			name:     "missing script param",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{},
			wantErr:  "script is required",
		},
		{
			name:     "empty agent dir",
			agentDir: "",
			args:     RunAgentScriptArgs{Script: "echo.sh"},
			wantErr:  "no agent directory configured",
		},
		{
			name:     "path traversal with dots",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{Script: "../foo.sh"},
			wantErr:  "must not contain path separators",
		},
		{
			name:     "path traversal with slash",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{Script: "subdir/evil.sh"},
			wantErr:  "must not contain path separators",
		},
		{
			name:     "path traversal with backslash",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{Script: "subdir\\evil.sh"},
			wantErr:  "must not contain path separators",
		},
		{
			name:     "script not found",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{Script: "nonexistent.sh"},
			wantErr:  "script not found",
		},
		{
			name:     "successful execution",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{Script: "echo.sh"},
			wantOut:  "hello",
			wantExit: "exit_code: 0",
		},
		{
			name:     "successful execution with args",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{Script: "echo.sh", Args: "world"},
			wantOut:  "hello world",
			wantExit: "exit_code: 0",
		},
		{
			name:     "non-zero exit",
			agentDir: agentDir,
			args:     RunAgentScriptArgs{Script: "fail.sh"},
			wantOut:  "failing",
			wantExit: "exit_code: 42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ToolConfig{AgentDir: tt.agentDir}
			tool := NewRunAgentScriptTool(cfg, limits)

			argsJSON, _ := json.Marshal(tt.args)
			output, err := tool.Execute(context.Background(), argsJSON)
			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}

			text := output.Content
			if tt.wantErr != "" {
				if !strings.Contains(text, tt.wantErr) {
					t.Errorf("expected error containing %q, got: %s", tt.wantErr, text)
				}
				return
			}
			if tt.wantOut != "" && !strings.Contains(text, tt.wantOut) {
				t.Errorf("expected output containing %q, got: %s", tt.wantOut, text)
			}
			if tt.wantExit != "" && !strings.Contains(text, tt.wantExit) {
				t.Errorf("expected %q in output, got: %s", tt.wantExit, text)
			}
		})
	}
}

func TestRunAgentScriptTool_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not supported on Windows")
	}

	// Create agent dir and an external dir
	agentDir := t.TempDir()
	externalDir := t.TempDir()

	// Create an external script
	externalScript := filepath.Join(externalDir, "evil.sh")
	os.WriteFile(externalScript, []byte("#!/bin/sh\necho pwned\n"), 0755)

	// Create a symlink inside agent dir pointing outside
	symlink := filepath.Join(agentDir, "evil.sh")
	os.Symlink(externalScript, symlink)

	cfg := &ToolConfig{AgentDir: agentDir}
	tool := NewRunAgentScriptTool(cfg, DefaultOutputLimits())

	args, _ := json.Marshal(RunAgentScriptArgs{Script: "evil.sh"})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	text := output.Content
	if !strings.Contains(text, "SYMLINK_ESCAPE") {
		t.Errorf("expected SYMLINK_ESCAPE error, got: %s", text)
	}
}

func TestRunAgentScriptTool_DirectoryTarget(t *testing.T) {
	agentDir := t.TempDir()

	// Create a subdirectory with the same name as a "script"
	os.Mkdir(filepath.Join(agentDir, "notascript.sh"), 0755)

	cfg := &ToolConfig{AgentDir: agentDir}
	tool := NewRunAgentScriptTool(cfg, DefaultOutputLimits())

	args, _ := json.Marshal(RunAgentScriptArgs{Script: "notascript.sh"})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	text := output.Content
	if !strings.Contains(text, "directory, not a file") {
		t.Errorf("expected directory error, got: %s", text)
	}
}

func TestRunAgentScriptTool_ArgsAreNotShellExpanded(t *testing.T) {
	agentDir := t.TempDir()
	writeErr := os.WriteFile(filepath.Join(agentDir, "echo.sh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$1\"\n"), 0755)
	if writeErr != nil {
		t.Fatalf("write script: %v", writeErr)
	}

	cfg := &ToolConfig{AgentDir: agentDir}
	tool := NewRunAgentScriptTool(cfg, DefaultOutputLimits())

	args, _ := json.Marshal(RunAgentScriptArgs{
		Script: "echo.sh",
		Args:   "hello; echo injected",
	})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	text := output.Content
	if !strings.Contains(text, "hello;") {
		t.Fatalf("expected literal shell metacharacters in output, got: %s", text)
	}
	if strings.Contains(text, "injected") {
		t.Fatalf("unexpected shell expansion in output: %s", text)
	}
}

func TestRunAgentScriptTool_ScriptPathWithSpaces(t *testing.T) {
	parent := t.TempDir()
	agentDir := filepath.Join(parent, "agent dir")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "echo.sh"), []byte("#!/bin/sh\necho spaced\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cfg := &ToolConfig{AgentDir: agentDir}
	tool := NewRunAgentScriptTool(cfg, DefaultOutputLimits())
	args, _ := json.Marshal(RunAgentScriptArgs{Script: "echo.sh"})

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(output.Content, "spaced") {
		t.Fatalf("expected script in spaced path to run, got: %s", output.Content)
	}
}

func TestRunAgentScriptTool_TimeoutKillsGrandchildren(t *testing.T) {
	t.Parallel()

	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "hang.sh"), []byte("#!/bin/sh\nsleep 60 & wait\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cfg := &ToolConfig{AgentDir: agentDir}
	tool := NewRunAgentScriptTool(cfg, DefaultOutputLimits())
	args, _ := json.Marshal(RunAgentScriptArgs{
		Script:         "hang.sh",
		TimeoutSeconds: 1,
	})

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !output.TimedOut {
		t.Fatal("expected timeout for grandchild-holding script")
	}
}

func TestRunAgentScriptTool_Preview(t *testing.T) {
	cfg := &ToolConfig{AgentDir: "/tmp/agent"}
	tool := NewRunAgentScriptTool(cfg, DefaultOutputLimits())

	tests := []struct {
		name string
		args RunAgentScriptArgs
		want string
	}{
		{
			name: "script only",
			args: RunAgentScriptArgs{Script: "create.sh"},
			want: "create.sh",
		},
		{
			name: "script with args",
			args: RunAgentScriptArgs{Script: "create.sh", Args: "foo bar"},
			want: "create.sh foo bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argsJSON, _ := json.Marshal(tt.args)
			got := tool.Preview(argsJSON)
			if got != tt.want {
				t.Errorf("Preview() = %q, want %q", got, tt.want)
			}
		})
	}
}
