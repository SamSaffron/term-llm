package tools

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
)

// makeCustomTool creates a test CustomScriptTool.
func makeCustomTool(t *testing.T, agentDir string, def agents.CustomToolDef) *CustomScriptTool {
	t.Helper()
	return newCustomScriptTool(def, agentDir, DefaultOutputLimits())
}

// writeScript writes an executable shell script into dir and returns its path.
func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

func TestCustomScriptTool_Spec_NoInput(t *testing.T) {
	tool := makeCustomTool(t, "/agent", agents.CustomToolDef{
		Name:        "my_tool",
		Description: "Does a thing",
		Script:      "scripts/my.sh",
	})
	spec := tool.Spec()
	if spec.Name != "my_tool" {
		t.Errorf("expected name my_tool, got %q", spec.Name)
	}
	if spec.Description != "Does a thing" {
		t.Errorf("unexpected description %q", spec.Description)
	}
	// When no input defined, schema should be an empty object schema
	tp, _ := spec.Schema["type"].(string)
	if tp != "object" {
		t.Errorf("expected schema type object, got %q", tp)
	}
}

func TestCustomScriptTool_Spec_WithInput(t *testing.T) {
	input := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"name"},
	}
	tool := makeCustomTool(t, "/agent", agents.CustomToolDef{
		Name:        "job_run",
		Description: "Run a job",
		Script:      "scripts/job-run.sh",
		Input:       input,
	})
	spec := tool.Spec()
	if spec.Schema["type"] != "object" {
		t.Errorf("expected schema type object")
	}
	props, ok := spec.Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("expected properties in schema")
	}
	if _, ok := props["name"]; !ok {
		t.Errorf("expected name in properties")
	}
}

func TestCustomScriptTool_Preview(t *testing.T) {
	tool := makeCustomTool(t, "/agent", agents.CustomToolDef{
		Name:   "my_tool",
		Script: "scripts/my.sh",
	})
	// Empty args → just tool name
	preview := tool.Preview(json.RawMessage("{}"))
	if preview != "my_tool" {
		t.Errorf("expected 'my_tool', got %q", preview)
	}
	// Non-trivial args
	preview2 := tool.Preview(json.RawMessage(`{"name":"foo"}`))
	if preview2 == "" {
		t.Error("expected non-empty preview for non-trivial args")
	}
}

func TestCustomScriptTool_BuildCommand_ArgsOnStdin(t *testing.T) {
	tool := makeCustomTool(t, "/agent", agents.CustomToolDef{
		Name:        "echo_name",
		Description: "Echo name",
		Script:      "echo-name.sh",
		Call:        "json",
	})

	cmd, err := tool.buildCommand(context.Background(), "/agent/echo-name.sh", json.RawMessage(`{"name":"jarvis"}`))
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	data, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if string(data) != `{"name":"jarvis"}` {
		t.Fatalf("stdin = %q", data)
	}
	if len(cmd.Args) != 1 || cmd.Args[0] != "/agent/echo-name.sh" {
		t.Fatalf("args = %#v", cmd.Args)
	}
}

func TestCustomScriptTool_BuildCommand_NamedArgs(t *testing.T) {
	tool := makeCustomTool(t, "/agent", agents.CustomToolDef{
		Name:        "echo_name",
		Description: "Echo name",
		Script:      "echo-name.sh",
	})

	cmd, err := tool.buildCommand(context.Background(), "/agent/echo-name.sh", json.RawMessage(`{"name":"jarvis"}`))
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	want := []string{"/agent/echo-name.sh", "--name", "jarvis"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args = %#v, want %#v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("args = %#v, want %#v", cmd.Args, want)
		}
	}
}

func TestCustomScriptTool_Execute_FailureStatus(t *testing.T) {
	tests := []struct {
		name        string
		script      string
		mode        os.FileMode
		wantContent string
	}{
		{
			name:        "missing script",
			script:      "missing.sh",
			wantContent: "script not found",
		},
		{
			name:        "unexecutable script",
			script:      "unexecutable.sh",
			mode:        0644,
			wantContent: "script error",
		},
		{
			name:        "nonzero exit",
			script:      "fails.sh",
			mode:        0755,
			wantContent: "exit_code: 7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.mode != 0 {
				content := "#!/bin/sh\necho diagnostic >&2\n"
				if tt.script == "fails.sh" {
					content += "exit 7\n"
				}
				if err := os.WriteFile(filepath.Join(dir, tt.script), []byte(content), tt.mode); err != nil {
					t.Fatalf("write script: %v", err)
				}
			}

			tool := makeCustomTool(t, dir, agents.CustomToolDef{
				Name:        "test_tool",
				Description: "Test failure status",
				Script:      tt.script,
			})
			out, err := tool.Execute(context.Background(), json.RawMessage("{}"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !out.IsError {
				t.Errorf("expected IsError=true, got output: %s", out.Content)
			}
			if !containsStr(out.Content, tt.wantContent) {
				t.Errorf("expected output containing %q, got %q", tt.wantContent, out.Content)
			}
		})
	}
}

func TestCustomScriptTool_Execute_SuccessStatus(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "success.sh", "#!/bin/sh\necho success\n")
	tool := makeCustomTool(t, dir, agents.CustomToolDef{
		Name:        "success",
		Description: "Successful script",
		Script:      "success.sh",
	})

	out, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.IsError || out.TimedOut {
		t.Fatalf("successful script returned failure flags: %+v", out)
	}
}

func TestCustomScriptTool_Execute_SymlinkEscape(t *testing.T) {
	agentDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a script outside the agent dir
	outsideScript := filepath.Join(outsideDir, "evil.sh")
	if err := os.WriteFile(outsideScript, []byte("#!/bin/sh\necho evil"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a symlink inside agentDir pointing outside
	symlink := filepath.Join(agentDir, "evil.sh")
	if err := os.Symlink(outsideScript, symlink); err != nil {
		t.Fatal(err)
	}

	tool := makeCustomTool(t, agentDir, agents.CustomToolDef{
		Name:        "evil",
		Description: "Evil symlink",
		Script:      "evil.sh",
	})

	out, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsStr(out.Content, "SYMLINK_ESCAPE") && !containsStr(out.Content, "symlink") {
		t.Errorf("expected symlink escape error in output, got %q", out.Content)
	}
}

func TestCustomScriptTool_JSONModePathWithSpaces(t *testing.T) {
	parent := t.TempDir()
	agentDir := filepath.Join(parent, "agent dir")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	writeScript(t, agentDir, "echo-name.sh", `#!/bin/sh
cat
`)

	tool := makeCustomTool(t, agentDir, agents.CustomToolDef{
		Name:        "echo_name",
		Description: "Echo name",
		Script:      "echo-name.sh",
		Call:        "json",
	})

	scriptPath, err := tool.resolveScript()
	if err != nil {
		t.Fatalf("resolveScript: %v", err)
	}
	cmd, err := tool.buildCommand(context.Background(), scriptPath, json.RawMessage(`{"name":"jarvis"}`))
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if cmd.Args[0] != scriptPath || !containsStr(cmd.Args[0], "agent dir") {
		t.Fatalf("script arg = %q, want resolved path with spaces %q", cmd.Args[0], scriptPath)
	}
	data, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if string(data) != `{"name":"jarvis"}` {
		t.Fatalf("stdin = %q", data)
	}
}

func TestCustomScriptTool_TimeoutKillsGrandchildren(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeScript(t, dir, "hang.sh", "#!/bin/sh\nsleep 60 & wait\n")

	tool := makeCustomTool(t, dir, agents.CustomToolDef{
		Name:        "hang",
		Description: "Hang",
		Script:      "hang.sh",
	})

	tool.def.TimeoutSeconds = 5
	// Short parent deadline instead of the 1s TimeoutSeconds floor; same
	// timeout/kill path, much faster.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := tool.Execute(ctx, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.TimedOut {
		t.Fatalf("expected timeout for grandchild-holding script, got: %s", out.Content)
	}
}

func TestRegisterCustomTools_BuiltinCollision(t *testing.T) {
	r := &LocalToolRegistry{
		tools: make(map[string]llm.Tool),
	}

	err := r.RegisterCustomTools([]agents.CustomToolDef{
		{Name: "shell", Description: "bad", Script: "shell.sh"},
	}, "/agent")
	if err == nil {
		t.Fatal("expected error for built-in name collision")
	}
}

func TestRegisterCustomTools_DuplicateCustomNameOverwrites(t *testing.T) {
	r := &LocalToolRegistry{
		tools: make(map[string]llm.Tool),
	}

	first := []agents.CustomToolDef{{Name: "job_run", Description: "first", Script: "first.sh"}}
	second := []agents.CustomToolDef{{Name: "job_run", Description: "second", Script: "second.sh"}}
	if err := r.RegisterCustomTools(first, "/agent"); err != nil {
		t.Fatalf("first RegisterCustomTools() error = %v", err)
	}
	if err := r.RegisterCustomTools(second, "/agent"); err != nil {
		t.Fatalf("second RegisterCustomTools() error = %v", err)
	}

	tool, ok := r.tools["job_run"].(*CustomScriptTool)
	if !ok {
		t.Fatalf("registered tool = %T, want *CustomScriptTool", r.tools["job_run"])
	}
	if tool.def.Description != "second" || tool.def.Script != "second.sh" {
		t.Fatalf("tool was not overwritten: %+v", tool.def)
	}
}

func TestRegisterCustomTools_InvalidName(t *testing.T) {
	r := &LocalToolRegistry{
		tools: make(map[string]llm.Tool),
	}

	err := r.RegisterCustomTools([]agents.CustomToolDef{
		{Name: "Bad-Name", Description: "x", Script: "x.sh"},
	}, "/agent")
	if err == nil {
		t.Fatal("expected error for invalid name")
	}
}

func TestRegisterCustomTools_WarnsMissingScript(t *testing.T) {
	dir := t.TempDir()
	r := &LocalToolRegistry{
		tools:  make(map[string]llm.Tool),
		limits: DefaultOutputLimits(),
	}

	// Should succeed (missing script is a warning, not an error)
	err := r.RegisterCustomTools([]agents.CustomToolDef{
		{Name: "my_tool", Description: "x", Script: "nonexistent.sh"},
	}, dir)
	if err != nil {
		t.Fatalf("unexpected error for missing script: %v", err)
	}
	if _, ok := r.tools["my_tool"]; !ok {
		t.Error("expected my_tool to be registered")
	}
}

// containsStr is a simple substring check helper.
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
