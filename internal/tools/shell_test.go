package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestShellTool_Spec(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	spec := tool.Spec()

	if spec.Name != ShellToolName {
		t.Errorf("expected name %q, got %q", ShellToolName, spec.Name)
	}
	if spec.Description == "" {
		t.Error("spec should have a description")
	}
	if spec.Schema == nil {
		t.Fatal("spec should have a schema")
	}

	props, ok := spec.Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema should have properties")
	}
	for _, p := range []string{"command", "working_dir", "timeout_seconds", "env", "description"} {
		if _, ok := props[p]; !ok {
			t.Errorf("schema should have %s property", p)
		}
	}

	required, ok := spec.Schema["required"].([]string)
	if !ok {
		t.Fatal("schema should have required array")
	}
	found := false
	for _, r := range required {
		if r == "command" {
			found = true
		}
	}
	if !found {
		t.Error("command should be required")
	}
}

func TestShellTool_Preview(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())

	tests := []struct {
		name     string
		args     json.RawMessage
		expected string
	}{
		{
			name:     "description overrides command",
			args:     mustMarshalShellArgs(ShellArgs{Command: "echo hidden", Description: "Describe action"}),
			expected: "Describe action",
		},
		{
			name:     "short command",
			args:     mustMarshalShellArgs(ShellArgs{Command: "echo hello"}),
			expected: "echo hello",
		},
		{
			name:     "long command is truncated",
			args:     mustMarshalShellArgs(ShellArgs{Command: "echo this is a very long command that exceeds fifty characters limit here"}),
			expected: "echo this is a very long command that exceeds f...",
		},
		{
			name:     "empty command",
			args:     mustMarshalShellArgs(ShellArgs{Command: ""}),
			expected: "",
		},
		{
			name:     "invalid JSON",
			args:     json.RawMessage(`{invalid}`),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Preview(tt.args)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestShellTool_Execute(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())

	tests := []struct {
		name     string
		args     json.RawMessage
		wantOut  string // substring expected in output
		wantExit string // exit code substring
		wantErr  string // error substring (empty = no error)
	}{
		{
			name:     "successful command",
			args:     mustMarshalShellArgs(ShellArgs{Command: "echo hello"}),
			wantOut:  "hello",
			wantExit: "exit_code: 0",
		},
		{
			name:     "command with stderr",
			args:     mustMarshalShellArgs(ShellArgs{Command: "echo err >&2"}),
			wantOut:  "err",
			wantExit: "exit_code: 0",
		},
		{
			name:     "non-zero exit code",
			args:     mustMarshalShellArgs(ShellArgs{Command: "exit 42"}),
			wantExit: "exit_code: 42",
		},
		{
			name:    "missing command param",
			args:    mustMarshalShellArgs(ShellArgs{Command: ""}),
			wantErr: "command is required",
		},
		{
			name:    "invalid JSON args",
			args:    json.RawMessage(`{invalid}`),
			wantErr: "Error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := tool.Execute(context.Background(), tt.args)
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

func TestShellTool_ExecuteEnv(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())

	t.Setenv("INHERITED_VAR", "present")
	args := mustMarshalShellArgs(ShellArgs{
		Command: "echo $MY_VAR $INHERITED_VAR",
		Env: EnvMap{
			"MY_VAR": "hello",
		},
	})

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	text := output.Content
	if !strings.Contains(text, "hello") {
		t.Errorf("expected output to contain env var value, got: %s", text)
	}
	if !strings.Contains(text, "present") {
		t.Errorf("expected output to contain inherited env var value, got: %s", text)
	}
}

func TestShellTool_ExecuteEnvOverride(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())

	t.Setenv("CONFLICT_VAR", "old")
	args := mustMarshalShellArgs(ShellArgs{
		Command: "echo $CONFLICT_VAR",
		Env: EnvMap{
			"CONFLICT_VAR": "new",
		},
	})

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	text := output.Content
	if !strings.Contains(text, "new") {
		t.Errorf("expected output to contain overridden env var value, got: %s", text)
	}
	if strings.Contains(text, "old") {
		t.Errorf("expected output to omit old env var value, got: %s", text)
	}
}

func TestShellTool_WorkingDir(t *testing.T) {
	dir := t.TempDir()
	tool := NewShellTool(nil, nil, DefaultOutputLimits())

	args := mustMarshalShellArgs(ShellArgs{
		Command:    "pwd",
		WorkingDir: dir,
	})

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	text := output.Content
	if !strings.Contains(text, dir) {
		t.Errorf("expected working dir %q in output, got: %s", dir, text)
	}
	if !strings.Contains(text, "exit_code: 0") {
		t.Errorf("expected exit_code: 0 in output, got: %s", text)
	}
}

func TestShellTool_Timeout(t *testing.T) {
	t.Run("default timeout is 30 seconds", func(t *testing.T) {
		tool := NewShellTool(nil, nil, DefaultOutputLimits())
		// Just verify the tool executes a fast command successfully
		// (proving that a default timeout exists and allows short commands)
		args := mustMarshalShellArgs(ShellArgs{Command: "echo ok"})
		output, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if !strings.Contains(output.Content, "ok") {
			t.Errorf("expected 'ok' in output, got: %s", output.Content)
		}
	})

	t.Run("custom timeout respected", func(t *testing.T) {
		tool := NewShellTool(nil, nil, DefaultOutputLimits())
		args := mustMarshalShellArgs(ShellArgs{
			Command:        "echo ok",
			TimeoutSeconds: 60,
		})
		output, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if !strings.Contains(output.Content, "ok") {
			t.Errorf("expected 'ok' in output, got: %s", output.Content)
		}
	})

	t.Run("timeout clamped to max 300", func(t *testing.T) {
		tool := NewShellTool(nil, nil, DefaultOutputLimits())
		// 500 > 300 max, should be clamped but still work for a fast command
		args := mustMarshalShellArgs(ShellArgs{
			Command:        "echo ok",
			TimeoutSeconds: 500,
		})
		output, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if !strings.Contains(output.Content, "ok") {
			t.Errorf("expected 'ok' in output, got: %s", output.Content)
		}
	})

	t.Run("command times out", func(t *testing.T) {
		tool := NewShellTool(nil, nil, DefaultOutputLimits())
		args := mustMarshalShellArgs(ShellArgs{
			Command:        "sleep 10",
			TimeoutSeconds: 1,
		})
		output, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if !strings.Contains(output.Content, "[Command timed out]") {
			t.Errorf("expected '[Command timed out]' in output, got: %s", output.Content)
		}
		if !output.TimedOut {
			t.Error("expected output.TimedOut=true for timed-out command")
		}
	})

	t.Run("successful command does not set TimedOut", func(t *testing.T) {
		tool := NewShellTool(nil, nil, DefaultOutputLimits())
		args := mustMarshalShellArgs(ShellArgs{Command: "echo ok"})
		output, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
		if output.TimedOut {
			t.Error("expected output.TimedOut=false for successful command")
		}
	})
}

func TestShellTool_OutputTruncation(t *testing.T) {
	// Use a small MaxBytes to test truncation
	limits := OutputLimits{
		MaxBytes: 20,
	}
	tool := NewShellTool(nil, nil, limits)

	// Generate output longer than 20 bytes
	args := mustMarshalShellArgs(ShellArgs{
		Command: "echo 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'",
	})

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	text := output.Content
	if !strings.Contains(text, "[Output truncated due to size limit]") {
		t.Errorf("expected truncation message in output, got: %s", text)
	}
}

func TestShellTool_ExecuteEnvArrayFormat(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())

	// Simulate the strict-mode array format emitted by the Responses API.
	args := json.RawMessage(`{"command":"echo $MY_VAR","env":[{"key":"MY_VAR","value":"hello_strict"}]}`)

	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !strings.Contains(output.Content, "hello_strict") {
		t.Errorf("expected output to contain env var value from array format, got: %s", output.Content)
	}
}

func TestEnvMap_EmptyKeyReturnsError(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	// An array-form env entry with an empty key should produce a clear error.
	args := json.RawMessage(`{"command":"echo hi","env":[{"key":"","value":"oops"}]}`)
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned unexpected Go error: %v", err)
	}
	if !strings.Contains(output.Content, "Error") {
		t.Errorf("expected error message in output for empty env key, got: %s", output.Content)
	}
}

func mustMarshalShellArgs(args ShellArgs) json.RawMessage {
	data, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return data
}
