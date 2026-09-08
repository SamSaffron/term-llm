package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/spf13/cobra"
)

func TestMCPStatusReportsStaticAuthWithoutSecrets(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfg := &mcp.Config{Servers: map[string]mcp.ServerConfig{
		"static": {Type: "http", URL: "https://mcp.example/mcp", Headers: map[string]string{"Authorization": "Bearer must-not-print"}},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := mcpStatus(cmd, []string{"static"}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "authentication: not needed") || strings.Contains(got, "must-not-print") {
		t.Fatalf("status output = %q", got)
	}
}

func TestMCPRunArgCompletionDoesNotStartServerOnCacheMiss(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	marker := filepath.Join(configHome, "server-started")
	script := filepath.Join(configHome, "start-server.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho started > \"$1\"\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &mcp.Config{Servers: map[string]mcp.ServerConfig{
		"demo": {
			Command: script,
			Args:    []string{marker},
		},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	completions, directive := MCPRunArgCompletion(nil, []string{"demo"}, "")
	if len(completions) != 0 {
		t.Fatalf("expected no completions on cache miss, got %v", completions)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v, want %v", directive, cobra.ShellCompDirectiveNoFileComp)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		if err != nil {
			t.Fatalf("stat marker: %v", err)
		}
		t.Fatalf("expected completion not to start server, but marker %q was created", marker)
	}
}

func TestMCPRunArgCompletionUsesCachedTools(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "term-llm"), 0755); err != nil {
		t.Fatal(err)
	}

	mcp.CacheTools("demo", []mcp.ToolSpec{
		{
			Name: "fetch",
			Schema: map[string]any{
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"pattern": map[string]any{"type": "string"},
				},
			},
		},
		{Name: "format", Schema: map[string]any{}},
	})

	completions, directive := MCPRunArgCompletion(nil, []string{"demo", "fetch"}, "p")
	want := []string{"path=", "pattern="}
	if len(completions) != len(want) {
		t.Fatalf("len(completions) = %d, want %d (%v)", len(completions), len(want), completions)
	}
	for i := range want {
		if completions[i] != want[i] {
			t.Fatalf("completions[%d] = %q, want %q (all=%v)", i, completions[i], want[i], completions)
		}
	}
	wantDirective := cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	if directive != wantDirective {
		t.Fatalf("directive = %v, want %v", directive, wantDirective)
	}
}
func TestMCPRunIsErrorRendersThenFails(t *testing.T) {
	result := llm.ToolOutput{
		Content: "tool failed usefully",
		IsError: true,
	}
	var output bytes.Buffer
	if err := renderMCPToolResult(&output, result); err != nil {
		t.Fatalf("renderMCPToolResult() error = %v", err)
	}
	if !strings.Contains(output.String(), "tool failed usefully") {
		t.Fatalf("rendered output = %q, want useful error content", output.String())
	}
	if err := mcpToolResultError("failure", result); err == nil {
		t.Fatal("IsError result must retain non-zero command behavior")
	}
}

func TestRenderMCPToolResultImageOnly(t *testing.T) {
	result := llm.ToolOutput{ContentParts: []llm.ToolContentPart{{
		Type:      llm.ToolContentPartImageData,
		ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="},
	}}}
	var output bytes.Buffer
	if err := renderMCPToolResult(&output, result); err != nil {
		t.Fatalf("renderMCPToolResult() error = %v", err)
	}
	if !strings.Contains(output.String(), `"type": "image_data"`) || !strings.Contains(output.String(), `"base64": "aGVsbG8="`) {
		t.Fatalf("rendered image output = %q", output.String())
	}
}

func TestParseValue(t *testing.T) {
	tests := []struct {
		input string
		want  any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"42", int64(42)},
		{"3.14", 3.14},
		{"hello", "hello"},
		{"", ""},
	}
	for _, tt := range tests {
		got := parseValue(tt.input)
		if got != tt.want {
			t.Errorf("parseValue(%q) = %v (%T), want %v (%T)", tt.input, got, got, tt.want, tt.want)
		}
	}
}

func TestReadFileArg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("file contents here"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := readFileArg(path)
	if err != nil {
		t.Fatalf("readFileArg(%q) error: %v", path, err)
	}
	if got != "file contents here" {
		t.Errorf("readFileArg(%q) = %q, want %q", path, got, "file contents here")
	}
}

func TestReadFileArgMissing(t *testing.T) {
	_, err := readFileArg("/nonexistent/path/file.txt")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestFormatSchemaParams(t *testing.T) {
	tests := []struct {
		name      string
		schema    map[string]any
		maxParams int
		want      string
	}{
		{
			name:      "empty",
			schema:    map[string]any{},
			maxParams: 5,
			want:      "",
		},
		{
			name: "with required",
			schema: map[string]any{
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
					"mode": map[string]any{"type": "string"},
				},
				"required": []any{"path"},
			},
			maxParams: 5,
			want:      "(mode, path*)",
		},
		{
			name: "truncated",
			schema: map[string]any{
				"properties": map[string]any{
					"a": map[string]any{},
					"b": map[string]any{},
					"c": map[string]any{},
				},
			},
			maxParams: 2,
			want:      "(a, b, ...)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSchemaParams(tt.schema, tt.maxParams)
			if got != tt.want {
				t.Errorf("formatSchemaParams() = %q, want %q", got, tt.want)
			}
		})
	}
}

func newMCPAddTestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: mcpAddCmd.Use, Args: mcpAddCmd.Args, RunE: mcpAddCmd.RunE}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd
}

func TestMCPAddCommand(t *testing.T) {
	for _, command := range [][]string{
		{"/path/to/binary", "mcp"},
		{"binary"},
		{"/path with spaces/binary", "mcp", "--port", "1234", "--help", "--", "", "a b", "$HOME", "a;b"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			cmd := newMCPAddTestCommand()
			cmd.SetArgs(append([]string{"local", "--"}, command...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			cfg, err := mcp.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			server, ok := cfg.Servers["local"]
			if !ok || server.Command != command[0] || server.TransportType() != "stdio" {
				t.Fatalf("saved server = %+v", server)
			}
			if !slices.Equal(server.Args, command[1:]) {
				t.Fatalf("saved args = %q, want %q", server.Args, command[1:])
			}
		})
	}
}

func TestMCPAddArgs(t *testing.T) {
	for _, tt := range []struct {
		args  []string
		valid bool
	}{
		{[]string{"playwright"}, true},
		{[]string{"@playwright/mcp"}, true},
		{[]string{"https://example.com/mcp"}, true},
		{[]string{"local", "--", "binary", "mcp"}, true},
		{nil, false},
		{[]string{"local", "binary", "mcp"}, false},
		{[]string{"local", "--"}, false},
		{[]string{"--", "binary", "mcp"}, false},
		{[]string{"local", "extra", "--", "binary"}, false},
		{[]string{"", "--", "binary"}, false},
		{[]string{"local", "--", " "}, false},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cmd := newMCPAddTestCommand()
			// Exercise Cobra parsing and validation without registry/network access.
			cmd.RunE = func(*cobra.Command, []string) error { return nil }
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if (err == nil) != tt.valid {
				t.Fatalf("error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestMCPAddCommandPreservesConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := &mcp.Config{Servers: map[string]mcp.ServerConfig{
		"existing": {Command: "original", Args: []string{"mcp"}},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	path, err := mcp.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newMCPAddTestCommand()
	cmd.SetArgs([]string{"existing", "--", "replacement"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("duplicate registration changed config")
	}
	cmd = newMCPAddTestCommand()
	cmd.SetArgs([]string{"new", "--", "another"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	loaded, err := mcp.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Servers) != 2 || loaded.Servers["existing"].Command != "original" || !slices.Equal(loaded.Servers["existing"].Args, []string{"mcp"}) {
		t.Fatalf("existing server not preserved: %+v", loaded.Servers)
	}
}
