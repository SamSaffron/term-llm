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

func TestAgyChildRosterContinuationAfterReload(t *testing.T) {
	f := newAgyLifecycleFixture(t)
	gateURL, ready, release := agyLifecycleGate(t)
	t.Setenv("FAKE_AGY_GATE_URL", gateURL)
	store, env := f.open(t)
	defer func() { release(); env.Close(); store.Close() }()
	if err := store.Create(f.ctx, &session.Session{ID: "agy-parent", Provider: "debug", Model: "fast"}); err != nil {
		t.Fatal(err)
	}
	spawn := env.runtime.toolMgr.GetSpawnAgentTool()
	out, err := spawn.Execute(f.ctx, json.RawMessage(`{"agent_name":"agy-child","prompt":"AGY_OLD_USER_MARKER Create one script receipt","wait":0}`))
	var first tools.SpawnAgentResult
	if err != nil || json.Unmarshal([]byte(out.Content), &first) != nil || first.AgentID == "" {
		t.Fatalf("spawn = %s, %v", out.Content, err)
	}
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("fake agy did not reach committed-script gate")
	}
	continueTool, _ := env.runtime.toolMgr.Registry.Get(tools.ContinueAgentToolName)
	out, err = continueTool.Execute(f.ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"instructions":"AGY_INTERMEDIATE_STEERING Finish this first run","wait":0}`, first.AgentID)))
	if err != nil || !strings.Contains(out.Content, `"intervention_disposition":"queued"`) {
		t.Fatalf("live steering = %s, %v", out.Content, err)
	}
	release()
	waitTool, _ := env.runtime.toolMgr.Registry.Get(tools.WaitAgentToolName)
	out, err = waitTool.Execute(f.ctx, json.RawMessage(fmt.Sprintf(`{"agent_ids":[%q],"max_wait":20}`, first.AgentID)))
	var waited []tools.SpawnAgentResult
	if err != nil || json.Unmarshal([]byte(out.Content), &waited) != nil || len(waited) != 1 || waited[0].Status != "completed" {
		t.Fatalf("first run = %s, %v", out.Content, err)
	}
	calls := f.calls(t)
	if len(calls) != 2 || !strings.Contains(calls[1].Input.Message.Content, "<live_code_cells>") || !strings.Contains(calls[1].Input.Message.Content, "AGY_INTERMEDIATE_STEERING") {
		t.Fatal("first run did not persist a boundary from a real second Stream containing the script roster")
	}
	assertAgyChildToolHistory(t, store, f, first.AgentID)
	env.Close()
	store.Close()
	store, env = f.open(t)
	continueTool, _ = env.runtime.toolMgr.Registry.Get(tools.ContinueAgentToolName)
	out, err = continueTool.Execute(f.ctx, json.RawMessage(fmt.Sprintf(`{"agent_id":%q,"instructions":"AGY_RELOADED_FOLLOWUP Finish without repeating tools","wait":20}`, first.AgentID)))
	var resumed tools.SpawnAgentResult
	if err != nil || json.Unmarshal([]byte(out.Content), &resumed) != nil || resumed.Status != "completed" || resumed.AgentID != first.AgentID {
		t.Fatalf("reloaded continuation = %s, %v", out.Content, err)
	}
	calls = f.calls(t)
	if len(calls) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(calls))
	}
	for i, call := range calls {
		if call.Native != "native-first" || call.Home != calls[0].Home || (i > 0 && !strings.Contains(strings.Join(call.Args, " "), "--conversation native-first")) {
			t.Fatalf("call %d lost native conversation: args=%v native=%s home=%s", i, call.Args, call.Native, call.Home)
		}
	}
	last := calls[2].Input.Message.Content
	if !strings.Contains(last, "AGY_RELOADED_FOLLOWUP") {
		t.Fatal("reloaded followup missing")
	}
	for _, marker := range []string{"AGY_OLD_USER_MARKER", "AGY_OLD_TOOL_MARKER", "AGY_INTERMEDIATE_STEERING", "<live_code_cells>"} {
		if strings.Contains(last, marker) {
			t.Fatalf("reloaded native continuation replayed historical/transient marker %s", marker)
		}
	}
	assertAgyChildIdentity(t, store, f, first.AgentID)
	assertAgyChildToolHistory(t, store, f, first.AgentID)
	effect, err := os.ReadFile(f.effectPath)
	if err != nil || string(effect) != "x" {
		t.Fatalf("side effect repeated: %q, %v", effect, err)
	}
	assertAgyFilteringTrace(t, f.tracePath, 3)
}
