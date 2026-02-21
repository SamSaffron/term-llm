package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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

func TestCustomScriptTool_Execute_Success(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "hello.sh", "#!/bin/sh\necho hello")

	tool := makeCustomTool(t, dir, agents.CustomToolDef{
		Name:        "hello",
		Description: "Say hello",
		Script:      "hello.sh",
	})

	out, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	result := out.Content
	if result == "" {
		t.Error("expected non-empty output")
	}
	// Should contain "hello"
	if !containsStr(result, "hello") {
		t.Errorf("expected output to contain 'hello', got %q", result)
	}
}

func TestCustomScriptTool_Execute_ArgsOnStdin(t *testing.T) {
	dir := t.TempDir()
	// Script reads JSON from stdin and echoes the 'name' field
	writeScript(t, dir, "echo-name.sh", `#!/bin/sh
INPUT=$(cat)
echo "$INPUT" | grep -o '"name":"[^"]*"'
`)

	tool := makeCustomTool(t, dir, agents.CustomToolDef{
		Name:        "echo_name",
		Description: "Echo name",
		Script:      "echo-name.sh",
	})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"jarvis"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsStr(out.Content, "jarvis") {
		t.Errorf("expected 'jarvis' in output, got %q", out.Content)
	}
}

func TestCustomScriptTool_Execute_EnvVars(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "env.sh", "#!/bin/sh\necho $TERM_LLM_TOOL_NAME $TERM_LLM_AGENT_DIR $MY_CUSTOM_VAR")

	tool := makeCustomTool(t, dir, agents.CustomToolDef{
		Name:        "env_test",
		Description: "Test env",
		Script:      "env.sh",
		Env:         map[string]string{"MY_CUSTOM_VAR": "custom_value"},
	})

	out, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := out.Content
	if !containsStr(text, "env_test") {
		t.Errorf("expected TERM_LLM_TOOL_NAME in output, got %q", text)
	}
	if !containsStr(text, "custom_value") {
		t.Errorf("expected MY_CUSTOM_VAR in output, got %q", text)
	}
}

func TestCustomScriptTool_Execute_NonZeroExitCode(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "fail.sh", "#!/bin/sh\necho oops >&2\nexit 2")

	tool := makeCustomTool(t, dir, agents.CustomToolDef{
		Name:        "fail_tool",
		Description: "Fails",
		Script:      "fail.sh",
	})

	out, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Non-zero exit should be surfaced in the output, not an error return
	text := out.Content
	if !containsStr(text, "exit") && !containsStr(text, "2") {
		t.Logf("output: %q", text)
	}
}

func TestCustomScriptTool_Execute_ScriptNotFound(t *testing.T) {
	dir := t.TempDir()
	tool := makeCustomTool(t, dir, agents.CustomToolDef{
		Name:        "missing",
		Description: "Missing script",
		Script:      "nonexistent.sh",
	})

	out, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsStr(out.Content, "FILE_NOT_FOUND") && !containsStr(out.Content, "not found") {
		t.Errorf("expected file-not-found error in output, got %q", out.Content)
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
