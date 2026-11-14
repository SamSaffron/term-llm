package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/tools"
)

// Exercise the actual spawn/continue tools, child engine, agy provider, MCP
// bridge, subprocess, and SQLite persistence across complete runtime teardown.
func TestAgyChildContinuesAfterReload(t *testing.T) {
	for _, mode := range []string{"native", "missing_provider_state", "missing_native_db"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgyLifecycleFixture(t)
			store, env := f.open(t)
			defer func() { env.Close(); store.Close() }()
			if err := store.Create(f.ctx, &session.Session{ID: "agy-parent", Provider: "debug", Model: "fast"}); err != nil {
				t.Fatal(err)
			}
			// Above the OS per-argument limit: replay must go over stdin intact.
			original := "AGY_OLD_USER_MARKER\n" + strings.Repeat("large review evidence λ\n", 16*1024) + "AGY_OLD_USER_END"
			args, err := json.Marshal(map[string]any{"agent_name": "agy-child", "prompt": original, "wait": 20})
			if err != nil {
				t.Fatal(err)
			}
			out, err := env.runtime.toolMgr.GetSpawnAgentTool().Execute(f.ctx, args)
			var first tools.SpawnAgentResult
			if err != nil || json.Unmarshal([]byte(out.Content), &first) != nil || first.Status != "completed" || !first.Resumable {
				t.Fatalf("spawn = %s, err = %v", out.Content, err)
			}
			id := first.AgentID
			assertAgyChildIdentity(t, store, f, id)
			assertAgyChildToolHistory(t, store, f, id)
			stateStore := store.(session.ProviderStateStore)
			state, err := stateStore.LoadProviderState(f.ctx, id, "agy-bin")
			if err != nil || len(state) == 0 {
				t.Fatalf("provider state = %s, %v", state, err)
			}
			var saved struct {
				Home           string `json:"agy_home"`
				ConversationID string `json:"conversation_id"`
			}
			if err := json.Unmarshal(state, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.ConversationID != "native-first" {
				t.Fatalf("native id = %q", saved.ConversationID)
			}
			switch mode {
			case "missing_provider_state":
				if err := stateStore.DeleteProviderState(f.ctx, id, "agy-bin"); err != nil {
					t.Fatal(err)
				}
			case "missing_native_db":
				if err := os.Remove(filepath.Join(saved.Home, ".gemini", "antigravity-cli", "conversations", saved.ConversationID+".db")); err != nil {
					t.Fatal(err)
				}
			}
			env.Close()
			store.Close()
			store, env = f.open(t)
			continueTool, ok := env.runtime.toolMgr.Registry.Get(tools.ContinueAgentToolName)
			if !ok {
				t.Fatal("missing continue_agent")
			}
			const followup = "AGY_FOLLOWUP_ONLY Finish without repeating the completed script"
			out, err = continueTool.Execute(f.ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"instructions":%q,"wait":20}`, id, followup)))
			var continued tools.SpawnAgentResult
			if err != nil || json.Unmarshal([]byte(out.Content), &continued) != nil || continued.Status != "completed" || continued.AgentID != id || !continued.Resumable {
				t.Fatalf("continue = %s, err = %v", out.Content, err)
			}
			assertAgyChildIdentity(t, store, f, id)
			assertAgyChildToolHistory(t, store, f, id)
			effect, err := os.ReadFile(f.effectPath)
			if err != nil || string(effect) != "x" {
				t.Fatalf("side effect repeated or lost: %q, %v", effect, err)
			}
			calls := f.calls(t)
			if len(calls) != 2 {
				t.Fatalf("provider invocations = %d, want 2", len(calls))
			}
			if !strings.Contains(calls[0].Input.Message.Content, original) {
				t.Fatal("initial >=256KiB prompt was truncated or changed")
			}
			second := calls[1]
			if second.Input.Event != "user" || !strings.Contains(second.Input.Message.Content, followup) {
				t.Fatal("followup missing from NDJSON user event")
			}
			for _, call := range calls {
				argv := strings.Join(call.Args, " ")
				if !strings.Contains(argv, "--model gemini-3-flash") {
					t.Fatalf("child provider did not use persisted model: %v", call.Args)
				}
				if strings.Contains(argv, "--print") || strings.Contains(argv, "AGY_OLD_") || strings.Contains(argv, "AGY_FOLLOWUP") {
					t.Fatalf("prompt in argv: %v", call.Args)
				}
			}
			argv := strings.Join(second.Args, " ")
			if mode == "native" {
				if !strings.Contains(argv, "--conversation native-first") || second.Native != saved.ConversationID || second.Home != saved.Home {
					t.Fatalf("wrong native resume: args=%v, home=%s, native=%s", second.Args, second.Home, second.Native)
				}
				for _, marker := range []string{"AGY_OLD_USER_MARKER", "AGY_OLD_TOOL_MARKER", "AGY_OLD_USER_END"} {
					if strings.Contains(second.Input.Message.Content, marker) {
						t.Fatalf("native continuation replayed %s", marker)
					}
				}
			} else {
				if strings.Contains(argv, "--conversation") || second.Native == saved.ConversationID {
					t.Fatalf("missing state resumed native conversation: %v", second.Args)
				}
				if len(second.Input.Message.Content) < 256*1024 || !strings.Contains(second.Input.Message.Content, original) || !strings.Contains(second.Input.Message.Content, "AGY_OLD_TOOL_MARKER") {
					t.Fatal("large fallback lost original user/tool history")
				}
			}
			if f.dnsAttempts.Load() == 0 {
				t.Fatal("fake CLI did not attempt generation through production proxy")
			}
			forwarded := assertAgyFilteringTrace(t, f.tracePath, 2)
			for i, prompt := range forwarded {
				if !strings.Contains(prompt, original) || strings.Contains(prompt, "<term_llm_prompt sha256=") {
					t.Fatalf("generation %d did not restore the exact large original from native truncation/history", i)
				}
			}
			state, err = store.(session.ProviderStateStore).LoadProviderState(f.ctx, id, "agy-bin")
			if err != nil || json.Unmarshal(state, &saved) != nil || saved.ConversationID != second.Native || saved.Home != second.Home {
				t.Fatalf("continued provider state = %s, %v", state, err)
			}
			record, err := session.AsAgentRunStore(store).GetAgentRun(f.ctx, id)
			if err != nil || record.Status != "completed" || record.RunGeneration != 2 {
				t.Fatalf("durable lifecycle = %+v, %v", record, err)
			}
		})
	}
}

func assertAgyChildIdentity(t *testing.T, store session.Store, f *agyLifecycleFixture, id string) {
	t.Helper()
	child, err := store.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if child.ProviderKey != "agy-bin" || child.Model != "gemini-3-flash" {
		t.Fatalf("persisted child model = %s:%s (label %q)", child.ProviderKey, child.Model, child.Provider)
	}
}

func assertAgyChildToolHistory(t *testing.T, store session.Store, f *agyLifecycleFixture, id string, markers ...string) {
	t.Helper()
	if len(markers) == 0 {
		markers = []string{"AGY_OLD_TOOL_MARKER"}
	}
	messages, err := store.GetMessages(f.ctx, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls, results := 0, 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				calls++
				if part.ToolCall.Name != "spawn_script" {
					t.Fatalf("unexpected tool call: %+v", part.ToolCall)
				}
			}
			if part.ToolResult != nil {
				results++
				content := core.ToolResultTextContent(part.ToolResult)
				if results > len(markers) || !strings.Contains(content, markers[results-1]) || !strings.Contains(content, "cell_id") {
					t.Fatalf("lost script receipt/tool evidence: %+v", part.ToolResult)
				}
			}
		}
	}
	if calls != len(markers) || results != len(markers) {
		t.Fatalf("persisted script calls=%d results=%d, want %d of each", calls, results, len(markers))
	}
}

func assertAgyFilteringTrace(t *testing.T, path string, want int) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != want {
		t.Fatalf("filtered generations = %d, want %d", len(lines), want)
	}
	var prompts []string
	for _, line := range lines {
		var trace struct {
			Original  json.RawMessage `json:"original_request"`
			Forwarded json.RawMessage `json:"forwarded_request"`
		}
		if err := json.Unmarshal([]byte(line), &trace); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(trace.Original), "run_command") || strings.Contains(string(trace.Forwarded), "run_command") || !strings.Contains(string(trace.Forwarded), "mcp_term-llm_spawn_script") {
			t.Fatalf("production tool isolation not exercised: %s", line)
		}
		var generation struct {
			Request struct {
				Contents []struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"contents"`
			} `json:"request"`
		}
		if err := json.Unmarshal(trace.Forwarded, &generation); err != nil {
			t.Fatal(err)
		}
		var prompt strings.Builder
		for _, content := range generation.Request.Contents {
			for _, part := range content.Parts {
				prompt.WriteString(part.Text)
			}
		}
		prompts = append(prompts, prompt.String())
	}
	return prompts
}
