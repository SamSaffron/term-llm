package doctor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPCheckMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	check := MCPCheck{Path: path}
	findings := check.Run(context.Background())

	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityInfo {
		t.Fatalf("severity = %s, want %s", findings[0].Severity, SeverityInfo)
	}
}

func TestMCPCheckInvalidJSON(t *testing.T) {
	path := mcpConfigFile(t, []byte(`{"servers":`))
	check := MCPCheck{Path: path}
	findings := check.Run(context.Background())

	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityError {
		t.Fatalf("severity = %s, want %s", findings[0].Severity, SeverityError)
	}
}

func TestMCPCheckStdioCommands(t *testing.T) {
	foundCommand, err := exec.LookPath(os.Args[0])
	if err != nil {
		t.Fatalf("locate test executable %q: %v", os.Args[0], err)
	}
	bogusCommand := "term-llm-doctor-command-that-cannot-exist-7f41c9"
	t.Setenv("PATH", t.TempDir())

	data, err := json.Marshal(map[string]any{
		"servers": map[string]any{
			"available": map[string]string{"command": foundCommand},
			"missing":   map[string]string{"command": bogusCommand},
		},
	})
	if err != nil {
		t.Fatalf("marshal mcp config: %v", err)
	}
	check := MCPCheck{Path: mcpConfigFile(t, data)}
	findings := check.Run(context.Background())

	if len(findings) != 1 {
		t.Fatalf("got %d findings, want only the missing command finding: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityError {
		t.Fatalf("severity = %s, want %s", findings[0].Severity, SeverityError)
	}
	if !strings.Contains(findings[0].Title, bogusCommand) {
		t.Fatalf("title %q does not mention command %q", findings[0].Title, bogusCommand)
	}
	if strings.Contains(findings[0].Title, "available") {
		t.Fatalf("available command unexpectedly produced a finding: %+v", findings[0])
	}
}

func TestMCPCheckDuplicateServerNames(t *testing.T) {
	data := []byte(`{"servers":{"a":{"type":"http","url":"https://example.com"},"a":{"type":"http","url":"https://example.net"}}}`)
	check := MCPCheck{Path: mcpConfigFile(t, data)}
	findings := check.Run(context.Background())

	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityWarn {
		t.Fatalf("severity = %s, want %s", findings[0].Severity, SeverityWarn)
	}
	if !strings.Contains(findings[0].Title, `server "a" more than once`) {
		t.Fatalf("title %q does not identify duplicate server", findings[0].Title)
	}
}

func TestMCPCheckNonHTTPURL(t *testing.T) {
	data := []byte(`{"servers":{"remote":{"type":"http","url":"ftp://example.com"}}}`)
	check := MCPCheck{Path: mcpConfigFile(t, data)}
	findings := check.Run(context.Background())

	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != SeverityWarn {
		t.Fatalf("severity = %s, want %s", findings[0].Severity, SeverityWarn)
	}
	if !strings.Contains(findings[0].Title, "non-HTTP url") {
		t.Fatalf("unexpected title %q", findings[0].Title)
	}
}

func mcpConfigFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write mcp config: %v", err)
	}
	return path
}
