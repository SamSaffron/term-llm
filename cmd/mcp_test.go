package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/spf13/cobra"
)

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
