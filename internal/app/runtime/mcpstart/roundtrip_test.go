package mcpstart

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLeafMCPSubprocessRoundTrip(t *testing.T) {
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"greeter": {Command: os.Args[0], Env: map[string]string{mcptest.ChildEnv: "1"}}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	manager, err := EnableMCPServersWithFeedback(ctx, "greeter", engine.NewEngine(nil, engine.NewToolRegistry()), io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manager == nil {
		t.Fatal("missing manager")
	}
	defer manager.StopAll()
	result, err := manager.CallTool(ctx, "greeter__greet", json.RawMessage(`{"name":"independent C1"}`))
	if err != nil || result.IsError || !strings.Contains(result.Content, "hi independent C1") {
		t.Fatalf("round trip result=%+v err=%v", result, err)
	}
}
