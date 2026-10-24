package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const childOverloaded = "API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment. If it persists, check https://status.claude.com."

// The fake CLI uses the real local MCP tool bridge, parser, engine, child runner
// and SQLite persistence. No provider requests leave the process's local host.
func TestFailedClaudeChildContinuesAfterReload(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake Claude CLI")
	}
	for _, mode := range []string{"native", "transcript_fallback", "initialization_failure", "crash_after_mcp"} {
		t.Run(mode, func(t *testing.T) {
			native := mode == "native"
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			dir := t.TempDir()
			sideEffect := filepath.Join(dir, "effect")
			logPath := filepath.Join(dir, "invocations")
			script := `#!/usr/bin/env python3
import json, os, sys, urllib.request
args = sys.argv[1:]
user = json.loads(sys.stdin.readline())
with open(os.environ["FAKE_CLAUDE_LOG"], "a") as f:
    f.write(json.dumps({"args":args,"input":user}) + "\n")
if os.environ["FAKE_CLAUDE_MODE"] == "initialization_failure":
    print(json.dumps({"type":"result","is_error":True,"result":"authentication failed"}), flush=True)
    sys.exit(0)
print(json.dumps({"type":"system","session_id":"native-child"}), flush=True)
if not os.path.exists(os.environ["FAKE_CLAUDE_EFFECT"]):
    cfg = json.load(open(args[args.index("--mcp-config")+1]))["mcpServers"]["term-llm"]
    body = json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{"command":"printf x >> " + os.environ["FAKE_CLAUDE_EFFECT"] + "; printf committed-marker"}}}).encode()
    headers = dict(cfg["headers"], **{"Content-Type":"application/json","Accept":"application/json, text/event-stream"})
    response = urllib.request.urlopen(urllib.request.Request(cfg["url"], data=body, headers=headers)).read().decode()
    if not os.path.exists(os.environ["FAKE_CLAUDE_EFFECT"]):
        raise Exception("tool failed: " + response)
    if os.environ["FAKE_CLAUDE_MODE"] == "crash_after_mcp":
        print("fake Claude crashed after committed MCP tool", file=sys.stderr, flush=True)
        sys.exit(2)
    print(json.dumps({"type":"result","is_error":True,"result":os.environ["FAKE_CLAUDE_ERROR"]}), flush=True)
else:
    print(json.dumps({"type":"result","subtype":"success","result":"recovered","usage":{"input_tokens":1,"output_tokens":1}}), flush=True)
`
			if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_CLAUDE_LOG", logPath)
			t.Setenv("FAKE_CLAUDE_EFFECT", sideEffect)
			t.Setenv("FAKE_CLAUDE_ERROR", childOverloaded)
			t.Setenv("FAKE_CLAUDE_MODE", mode)
			agentDir := filepath.Join(dir, "agents", "recovery-child")
			if err := os.MkdirAll(agentDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: recovery-child\nmodel: claude-bin:haiku\nskills: none\nmax_turns: 4\ntools:\n  enabled: [shell]\n"), 0644); err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(dir, "sessions.db")
			cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "fast", FastModel: "fast"}, "claude-bin": {Type: "claude-bin", Model: "haiku"}}, Agents: config.AgentsConfig{SearchPaths: []string{filepath.Join(dir, "agents")}}, Sessions: config.SessionsConfig{Enabled: true, Path: dbPath}}
			ctx := core.ContextWithCallID(core.ContextWithSessionID(context.Background(), "recovery-parent"), "recovery-spawn")
			openEnv := func() (session.Store, *cmdRunEnvironment) {
				store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: dbPath})
				if err != nil {
					t.Fatal(err)
				}
				defaults := cmdRunnerOptions{ConfigSet: true, Store: store, Yolo: true, Tools: "spawn_agent", ToolsSet: true}
				env, err := newCmdRunner(cfg, defaults).(*cmdRunner).prepare(ctx, runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: "recovery-parent", Provider: "debug", Model: "fast", Cwd: dir, DeferSession: true}, nil)
				if err != nil {
					store.Close()
					t.Fatal(err)
				}
				return store, env
			}
			store, env := openEnv()
			defer func() { env.Close(); store.Close() }()
			if err := store.Create(ctx, &session.Session{ID: "recovery-parent", Provider: "debug", Model: "fast"}); err != nil {
				t.Fatal(err)
			}
			out, err := env.runtime.toolMgr.GetSpawnAgentTool().Execute(ctx, json.RawMessage(`{"agent_name":"recovery-child","prompt":"Perform one side effect then finish","wait":10}`))
			if err != nil {
				t.Fatal(err)
			}
			var first tools.SpawnAgentResult
			if err := json.Unmarshal([]byte(out.Content), &first); err != nil {
				t.Fatal(err)
			}
			expectedError := "claude API error: " + childOverloaded
			if mode == "initialization_failure" {
				expectedError = "claude API error: authentication failed"
			}
			if mode == "crash_after_mcp" {
				expectedError = "fake Claude crashed after committed MCP tool"
			}
			if first.Status != "failed" || !strings.Contains(first.Error, expectedError) {
				t.Fatalf("failure = %s", out.Content)
			}
			if mode == "initialization_failure" {
				if first.Resumable {
					t.Fatalf("initialization failure resumable: %s", out.Content)
				}
				continueTool, _ := env.runtime.toolMgr.Registry.Get(tools.ContinueAgentToolName)
				blocked, err := continueTool.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"wait":10}`, first.AgentID)))
				if err != nil || !blocked.IsError || !strings.Contains(blocked.Content, "no saved assistant/tool history") {
					t.Fatalf("initialization failure continued: %s, %v", blocked.Content, err)
				}
				calls, err := os.ReadFile(logPath)
				if err != nil || strings.Count(string(calls), "\n") != 1 {
					t.Fatalf("initialization provider re-invoked: %s, %v", calls, err)
				}
				return
			}
			if !first.Resumable {
				t.Fatalf("failed child with committed tool is not resumable: %s", out.Content)
			}
			id := first.AgentID
			messages, err := store.GetMessages(ctx, id, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			results := 0
			for _, msg := range messages {
				for _, part := range msg.Parts {
					if part.ToolResult != nil {
						results++
					}
				}
			}
			if results != 1 {
				t.Fatalf("persisted tool results = %d; messages = %+v", results, messages)
			}
			stateStore := store.(session.ProviderStateStore)
			state, err := stateStore.LoadProviderState(ctx, id, "claude-bin")
			if err != nil || (len(state) == 0) != (mode == "crash_after_mcp") {
				t.Fatalf("failed child provider state = %s, %v", state, err)
			}
			if mode == "transcript_fallback" {
				if err := stateStore.DeleteProviderState(ctx, id, "claude-bin"); err != nil {
					t.Fatal(err)
				}
			}
			// Close both runtime and SQLite handle, then build a fresh manager/provider.
			env.Close()
			store.Close()
			store, env = openEnv()
			continueTool, ok := env.runtime.toolMgr.Registry.Get(tools.ContinueAgentToolName)
			if !ok {
				t.Fatal("missing continue_agent")
			}
			args := json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"instructions":"Finish","wait":10}`, id))
			for name, blockedCtx := range map[string]context.Context{
				"wrong parent":         core.ContextWithSessionID(ctx, "another-parent"),
				"recovery notice":      tools.ContextWithAgentRecoveryNotice(ctx),
				"restricted lifecycle": tools.ContextWithAgentLifecycleRestricted(ctx),
			} {
				blocked, err := continueTool.Execute(blockedCtx, args)
				if err != nil || !blocked.IsError {
					t.Fatalf("%s admitted failed child: %s, %v", name, blocked.Content, err)
				}
			}
			listTool, _ := env.runtime.toolMgr.Registry.Get(tools.ListAgentsToolName)
			listed, err := listTool.Execute(ctx, json.RawMessage(`{"status":"failed"}`))
			if err != nil || !strings.Contains(listed.Content, `"resumable":true`) {
				t.Fatalf("failed list metadata = %s, %v", listed.Content, err)
			}
			out, err = continueTool.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"instructions":"Finish without repeating the completed tool","wait":10}`, id)))
			var continued tools.SpawnAgentResult
			if err != nil || json.Unmarshal([]byte(out.Content), &continued) != nil || continued.Status != "completed" || continued.AgentID != id || !continued.Resumable {
				t.Fatalf("continuation = %s, err = %v", out.Content, err)
			}
			effect, err := os.ReadFile(sideEffect)
			if err != nil || string(effect) != "x" {
				t.Fatalf("side effect = %q, %v", effect, err)
			}
			calls, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			if len(lines) != 2 {
				t.Fatalf("provider invoked %d times (committed failure must not retry): %s", len(lines), calls)
			}
			var second struct {
				Args  []string        `json:"args"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
				t.Fatal(err)
			}
			resumed := strings.Contains(strings.Join(second.Args, " "), "--resume ")
			if resumed != native {
				t.Fatalf("native resume = %t, want %t; args=%v", resumed, native, second.Args)
			}
			if native && strings.Contains(string(second.Input), "Perform one side effect") {
				t.Fatalf("native resume replayed original prompt: %s", second.Input)
			}
			if !native && !strings.Contains(string(second.Input), "committed-marker") {
				t.Fatalf("fallback lost committed tool history: %s", second.Input)
			}
			messages, err = store.GetMessages(ctx, id, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			toolCalls, toolResults := 0, 0
			for _, msg := range messages {
				for _, part := range msg.Parts {
					if part.ToolCall != nil {
						toolCalls++
					}
					if part.ToolResult != nil {
						toolResults++
						if !strings.Contains(part.ToolResult.Content, "committed-marker") {
							t.Fatalf("real result replaced by repair: %+v", part.ToolResult)
						}
					}
				}
			}
			if toolCalls != 1 || toolResults != 1 {
				t.Fatalf("continued transcript has %d calls and %d results", toolCalls, toolResults)
			}
			record, err := session.AsAgentRunStore(store).GetAgentRun(ctx, id)
			if err != nil || record.Status != "completed" || record.Error != "" || record.RunGeneration != 2 {
				t.Fatalf("durable run = %+v, %v", record, err)
			}
		})
	}
}
