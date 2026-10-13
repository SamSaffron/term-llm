package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcpsession"
)

func TestShellToolExportsSessionMCPSocket(t *testing.T) {
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	t.Setenv(mcpsession.EnvVar, "/inherited/should-be-shadowed.sock")
	unregister := mcpsession.Register("sess-mcp", "/run/test/mcp-sess-mcp.sock")
	defer unregister()

	run := func(sessionID string, env EnvMap) string {
		t.Helper()
		ctx := llm.ContextWithSessionID(context.Background(), sessionID)
		out, err := tool.Execute(ctx, mustMarshalShellArgs(ShellArgs{Command: "echo \"[$" + mcpsession.EnvVar + "]\"", Env: env}))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return out.Content
	}

	if got := run("sess-mcp", nil); !strings.Contains(got, "[/run/test/mcp-sess-mcp.sock]") {
		t.Fatalf("session shell should see its MCP socket, got %q", got)
	}
	if got := run("other-session", nil); !strings.Contains(got, "[]") {
		t.Fatalf("a session without MCP must not see an inherited socket, got %q", got)
	}
	if got := run("sess-mcp", EnvMap{mcpsession.EnvVar: "/explicit.sock"}); !strings.Contains(got, "[/explicit.sock]") {
		t.Fatalf("explicit env must win, got %q", got)
	}
}
