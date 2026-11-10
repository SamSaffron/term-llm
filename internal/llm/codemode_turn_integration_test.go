package llm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode/tool"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/testutil"
)

type turnGate struct {
	release chan struct{}
	entered chan struct{}
}

func (*turnGate) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "turn_gate", Schema: map[string]interface{}{"type": "object"}}
}
func (*turnGate) Preview(json.RawMessage) string { return "gate" }
func (g *turnGate) Execute(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return core.TextOutput("released"), nil
	case <-ctx.Done():
		return core.ToolOutput{}, ctx.Err()
	}
}
func TestCodeModeFinalPreservesCellAndNextTurnCanWait(t *testing.T) {
	h := testutil.NewEngineHarness()
	t.Cleanup(h.Engine.CloseCodeMode)
	g := &turnGate{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	h.Registry.Register(tool.NewExec())
	h.Registry.Register(tool.NewWait())
	h.Registry.Register(tool.NewCancel())
	h.Registry.Register(g)
	h.Provider.AddToolCall("original-spawn", "spawn_script", map[string]any{"code": `text('ready');await tools.turn_gate({});store('finished',true);text('later')`, "max_wait": 0})
	h.Provider.AddTextResponse("final answer")
	req := core.Request{SessionID: "across-turn", Messages: []core.Message{core.UserText("start")}, Tools: h.Registry.AllSpecs(), MaxTurns: 5}
	text, err := h.Run(context.Background(), req)
	if err != nil || !strings.Contains(text, "final answer") {
		t.Fatalf("first turn %q %v", text, err)
	}
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cell never dispatched")
	}
	cells := h.Engine.CodeModeCells(req.SessionID)
	if len(cells) != 1 || !cells[0].Live {
		t.Fatalf("final killed cell: %+v", cells)
	}
	close(g.release)
	h.Provider.AddToolCall("next-wait", "wait_script", map[string]any{"cell_id": cells[0].CellID, "max_wait": 0000})
	h.Provider.AddTextResponse("collected")
	// Simulate a compacted transcript: no spawn receipt survives in Messages.
	req.Messages = []core.Message{core.UserText("collect")}
	text, err = h.Run(context.Background(), req)
	if err != nil || !strings.Contains(text, "collected") {
		t.Fatalf("second turn %q %v", text, err)
	}
	found := false
	for _, request := range h.Provider.Requests {
		for _, message := range request.Messages {
			found = found || strings.Contains(core.MessageText(message), cells[0].CellID)
		}
	}
	if !found {
		t.Fatal("next-turn handle not discoverable")
	}
	ctx := core.ContextWithSessionID(context.Background(), req.SessionID)
	result, err := h.Engine.WaitCodeMode(ctx, cells[0].CellID, time.Second, 0)
	if err != nil || result.Status != "completed" || !result.StateCommitted {
		t.Fatalf("wait after final: %+v %v", result, err)
	}
}
