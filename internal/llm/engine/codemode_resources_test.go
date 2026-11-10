package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestCodeModeMergedStateLimitIsAtomicAndMetadataBounded(t *testing.T) {
	store := newCodeModeStore()
	value, _ := json.Marshal(strings.Repeat("x", 250<<10))
	for _, key := range []string{"a", "b", "c", "d"} {
		delta, _ := json.Marshal(codemode.StateDelta{Set: map[string]json.RawMessage{key: value}})
		if _, _, err := store.Apply("state", delta, nil); err != nil {
			t.Fatal(err)
		}
	}
	before := string(store.State("state"))
	delta, _ := json.Marshal(codemode.StateDelta{Set: map[string]json.RawMessage{"extra": value}, Delete: []string{"missing"}})
	if _, _, err := store.Apply("state", delta, nil); err == nil {
		t.Fatal("merged overflow committed")
	}
	if string(store.State("state")) != before {
		t.Fatal("partial merge")
	}
	_, versions := store.Snapshot("state")
	if _, _, err := store.Apply("state", json.RawMessage(`{"set":{},"delete":["a"]}`), versions); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.versions["state"]["a"]; exists {
		t.Fatal("deleted-key version tombstones retain unbounded history")
	}
}
func TestCodeModeTerminalRetentionIsBounded(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	ctx := asyncCodeContext("retention", "spawn")
	var ids []string
	for range codemode.MaxTerminalCells + 2 {
		r, err := e.ExecCodeMode(ctx, `text('bounded')`, time.Second, 0)
		if err != nil {
			t.Fatal(err)
		}
		r = awaitCodeModeTerminal(t, e, ctx, r)
		ids = append(ids, r.CellID)
		waitCodeModeSlotIdle(t, e)
	}
	if cells := e.CodeModeCells("retention"); len(cells) != codemode.MaxTerminalCells {
		t.Fatalf("retention count %d", len(cells))
	}
	old, _ := e.WaitCodeMode(ctx, ids[0], time.Millisecond, 0)
	if old.Status != "unknown" {
		t.Fatal("old terminal did not expire")
	}
	current, _ := e.WaitCodeMode(ctx, ids[len(ids)-1], time.Millisecond, 0)
	if current.Status != "completed" {
		t.Fatal("latest terminal expired")
	}
}
func TestCodeModeCancelledEffectsKeepOnlyTheirOwnAdmissionPlaces(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	e.RegisterTool(codeTestTool{name: "stuck", run: func(context.Context, json.RawMessage) (output core.ToolOutput, err error) {
		started <- struct{}{}
		<-release
		return core.TextOutput("late"), nil
	}})
	ctx := asyncCodeContext("quarantine-eight", "spawn")
	first, _ := e.ExecCodeMode(ctx, `text('ready');await tools.stuck({})`, time.Second, 0)
	<-started
	cancelled, _ := e.CancelCodeMode(ctx, first.CellID)
	if cancelled.Status != "cancelled" || len(cancelled.UnsettledCalls) == 0 {
		t.Fatalf("cancel %+v", cancelled)
	}
	// Seven more live cells fit; the quarantined effect keeps the eighth place.
	gate := make(chan struct{})
	e.RegisterTool(codeModeBlockingTool(make(chan struct{}, 8), gate))
	for range 7 {
		r, err := e.ExecCodeMode(ctx, `text('ready');await tools.hold({})`, time.Second, 0)
		if err != nil || r.IsError {
			t.Fatalf("sibling admission %+v %v", r, err)
		}
	}
	ninth, _ := e.ExecCodeMode(ctx, `return 9`, time.Millisecond, 0)
	if ninth.ErrorCode != "session_cell_limit" {
		t.Fatalf("quarantine place released: %+v", ninth)
	}
	close(release)
	close(gate)
	waitCodeModeSlotIdle(t, e)
	next, _ := e.ExecCodeMode(ctx, `return 10`, time.Second, 0)
	next = awaitCodeModeTerminal(t, e, ctx, next)
	if next.Status != "completed" {
		t.Fatalf("place not reclaimed %+v", next)
	}
}
