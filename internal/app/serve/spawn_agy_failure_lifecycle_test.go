package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// A native resumed turn may commit a tool before crashing or being cancelled.
// Reopening must discard its uncertain native cursor, retain both tool receipts,
// and require explicit continuation rather than silently retrying the turn.
func TestAgyInterruptedNativeChildContinuesAfterReload(t *testing.T) {
	for _, mode := range []string{"partial_failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newAgyLifecycleFixture(t)
			gateURL, ready, release := agyLifecycleGate(t)
			store, env := f.open(t)
			defer func() { release(); env.Close(); store.Close() }()
			if err := store.Create(f.ctx, &session.Session{ID: "agy-parent", Provider: "debug", Model: "fast"}); err != nil {
				t.Fatal(err)
			}
			out, err := env.runtime.toolMgr.GetSpawnAgentTool().Execute(f.ctx, json.RawMessage(`{"agent_name":"agy-child","prompt":"AGY_OLD_USER_MARKER Perform the first side effect","wait":20}`))
			var first tools.SpawnAgentResult
			if err != nil || json.Unmarshal([]byte(out.Content), &first) != nil || first.Status != "completed" {
				t.Fatalf("spawn = %s, %v", out.Content, err)
			}
			t.Setenv("FAKE_AGY_FAILURE_MODE", mode)
			t.Setenv("FAKE_AGY_READY_URL", gateURL)
			continueTool, _ := env.runtime.toolMgr.Registry.Get(tools.ContinueAgentToolName)
			wait := 20
			if mode == "cancelled" {
				wait = 0
			}
			out, err = continueTool.Execute(f.ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"instructions":"AGY_FAILURE_FOLLOWUP Commit a second tool before stopping","wait":%d}`, first.AgentID, wait)))
			if mode == "cancelled" {
				if err != nil || out.IsError {
					t.Fatalf("start cancellable continuation = %s, %v", out.Content, err)
				}
				select {
				case <-ready:
				case <-time.After(15 * time.Second):
					t.Fatal("resumed child did not reach committed-tool cancellation gate")
				}
				cancelTool, _ := env.runtime.toolMgr.Registry.Get(tools.CancelAgentToolName)
				out, err = cancelTool.Execute(f.ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q}`, first.AgentID)))
			}
			var stopped tools.SpawnAgentResult
			wantStatus := "failed"
			if mode == "cancelled" {
				wantStatus = "cancelled"
			}
			if err != nil || json.Unmarshal([]byte(out.Content), &stopped) != nil || stopped.Status != wantStatus || stopped.AgentID != first.AgentID || !stopped.Resumable {
				t.Fatalf("stopped native continuation = %s, %v", out.Content, err)
			}
			if mode == "partial_failure" && !strings.Contains(stopped.Error, "fake agy crashed after committed MCP tool") {
				t.Fatalf("lost partial subprocess failure: %s", out.Content)
			}
			assertAgyChildToolHistory(t, store, f, first.AgentID, "AGY_OLD_TOOL_MARKER", "AGY_PARTIAL_TOOL_MARKER")
			state, err := store.(session.ProviderStateStore).LoadProviderState(f.ctx, first.AgentID, "agy-bin")
			if err != nil || len(state) != 0 {
				t.Fatalf("uncertain native cursor retained: %s, %v", state, err)
			}
			if calls := f.calls(t); len(calls) != 2 {
				t.Fatalf("stopped turn retried automatically: %d invocations", len(calls))
			}
			env.Close()
			store.Close()
			store, env = f.open(t)
			continueTool, _ = env.runtime.toolMgr.Registry.Get(tools.ContinueAgentToolName)
			out, err = continueTool.Execute(f.ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"instructions":"AGY_RECOVERY_FOLLOWUP Finish without repeating either committed script","wait":20}`, first.AgentID)))
			var resumed tools.SpawnAgentResult
			if err != nil || json.Unmarshal([]byte(out.Content), &resumed) != nil || resumed.Status != "completed" || resumed.AgentID != first.AgentID {
				t.Fatalf("explicit recovery = %s, %v", out.Content, err)
			}
			calls := f.calls(t)
			if len(calls) != 3 {
				t.Fatalf("provider calls = %d, want 3", len(calls))
			}
			if !strings.Contains(strings.Join(calls[1].Args, " "), "--conversation native-first") || calls[1].Native != "native-first" {
				t.Fatalf("failure was not in native continuation: %v", calls[1].Args)
			}
			if strings.Contains(strings.Join(calls[2].Args, " "), "--conversation") || calls[2].Native == "native-first" {
				t.Fatalf("uncertain native turn was resumed: %v", calls[2].Args)
			}
			for _, marker := range []string{"AGY_OLD_USER_MARKER", "AGY_OLD_TOOL_MARKER", "AGY_FAILURE_FOLLOWUP", "AGY_PARTIAL_TOOL_MARKER", "AGY_RECOVERY_FOLLOWUP"} {
				if !strings.Contains(calls[2].Input.Message.Content, marker) {
					t.Fatalf("recovery fallback lost %s", marker)
				}
			}
			assertAgyChildIdentity(t, store, f, first.AgentID)
			assertAgyChildToolHistory(t, store, f, first.AgentID, "AGY_OLD_TOOL_MARKER", "AGY_PARTIAL_TOOL_MARKER")
			effect, err := os.ReadFile(f.effectPath)
			if err != nil || string(effect) != "xy" {
				t.Fatalf("committed side effects repeated or lost: %q, %v", effect, err)
			}
			assertAgyFilteringTrace(t, f.tracePath, 3)
			record, err := session.AsAgentRunStore(store).GetAgentRun(f.ctx, first.AgentID)
			if err != nil || record.Status != "completed" || record.RunGeneration != 3 {
				t.Fatalf("durable lifecycle = %+v, %v", record, err)
			}
		})
	}
}
