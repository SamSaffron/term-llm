package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
)

func TestMCPStartupFeedbackWithoutTerminal(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		name := "ready"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			server := mcp.ServerConfig{
				Command: executable,
				Env:     map[string]string{runServeMCPHandlerTestServerEnv: "1"},
			}
			if failed {
				server.Command = filepath.Join(t.TempDir(), "missing-server")
			}
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"test": server})
			var output bytes.Buffer
			manager, err := enableMCPServersWithFeedback(context.Background(), "test", llm.NewEngine(nil, nil), &output, nil)
			if manager != nil {
				defer manager.StopAll()
			}
			if failed {
				if err == nil {
					t.Fatal("expected startup failure")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			log := output.String()
			if strings.Contains(log, "\r") || strings.Count(log, "Starting MCP:") != 1 {
				t.Fatalf("startup feedback contains redraws: %q", log)
			}
			want := "Starting MCP: test\n"
			if !failed {
				want += "✓ MCP ready: 1 tools from test\n\n"
			}
			if log != want {
				t.Fatalf("feedback = %q, want %q", log, want)
			}
		})
	}
}
