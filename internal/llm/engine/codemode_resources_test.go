package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/codemode"
	"github.com/term-llm/term-llm/internal/llm/core"
	corestream "github.com/term-llm/term-llm/internal/llm/core/stream"
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

func TestCodeModeResetPrunesSettledSessionOwners(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	for _, id := range []string{"prune-a", "prune-b", "prune-c"} {
		ctx := asyncCodeContext(id, "spawn")
		r, _ := e.ExecCodeMode(ctx, `store('key',42)`, time.Second, 0)
		awaitCodeModeTerminal(t, e, ctx, r)
		waitCodeModeSlotIdle(t, e)
		e.ResetCodeModeSession(id)
	}
	e.codeModeSessions.mu.Lock()
	count := len(e.codeModeSessions.sessions)
	e.codeModeSessions.mu.Unlock()
	if count != 0 {
		t.Fatalf("reset retained %d settled owners", count)
	}
}
func TestLegacyCodeModeCannotBypassAsyncAdmissionAndToolBudgets(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	ctx := asyncCodeContext("mixed", "spawn")
	legacy, err := e.admitCodeCell(ctx, "legacy", corestream.Sender{})
	if err != nil {
		t.Fatal(err)
	}
	busy, _ := e.ExecCodeMode(ctx, `return 'bypass'`, time.Millisecond, 0)
	if busy.Status != "busy" {
		t.Fatalf("async bypassed legacy lane: %+v", busy)
	}
	legacy.Close()
	gate := make(chan struct{})
	defer close(gate)
	e.RegisterTool(codeModeBlockingTool(make(chan struct{}, 8), gate))
	running, _ := e.ExecCodeMode(ctx, `text('ready');await tools.hold({})`, time.Second, 0)
	if running.IsError {
		t.Fatal(running)
	}
	if legacy, err := e.admitCodeCell(ctx, "extra", corestream.Sender{}); err == nil {
		legacy.Close()
		t.Fatal("legacy bypassed async session budgets")
	}
}
func TestCodeModeRosterPrecedesLatestUserWithoutMutatingTranscript(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	ctx := asyncCodeContext("roster", "spawn")
	r, _ := e.ExecCodeMode(ctx, `return 1`, time.Second, 0)
	awaitCodeModeTerminal(t, e, ctx, r)
	messages := []core.Message{core.Message{Role: core.RoleSystem, Parts: []core.Part{{Type: core.PartText, Text: "policy"}}}, core.UserText("earlier"), core.AssistantText("answer"), core.UserText("latest")}
	request := e.prepareProviderRequest(core.Request{SessionID: "roster", Messages: messages})
	if len(request.Messages) != 5 || request.Messages[3].Role != core.RoleDeveloper || core.MessageText(request.Messages[4]) != "latest" {
		t.Fatalf("roster placement %+v", request.Messages)
	}
	if len(messages) != 4 || core.MessageText(messages[3]) != "latest" {
		t.Fatal("roster mutated transcript")
	}
}

func TestCodeModeTerminalEvictionPrefersDrained(t *testing.T) {
	owner := &codeModeSessionCells{}
	for i := 0; i < codemode.MaxTerminalCells+1; i++ {
		cell := newCodeModeAsyncCell(nil, fmt.Sprint(i), "session", nil)
		cell.done = true
		cell.observed = true
		cell.emitText("unread")
		owner.terminal = append(owner.terminal, cell)
	}
	// The oldest receipt still has unread text; a newer drained receipt expires.
	owner.terminal[3].drainPartsLocked(100)
	owner.evictTerminal()
	if len(owner.terminal) != codemode.MaxTerminalCells || owner.terminal[0].id != "0" || owner.terminal[3].id != "4" {
		t.Fatal("did not prefer drained receipt")
	}
	// With no drained receipts, the oldest unread receipt must expire.
	oldest := owner.terminal[0]
	owner.evictTerminal()
	if owner.terminal[0].id != "1" || len(oldest.parts) != 0 || oldest.pendingBytes != 0 {
		t.Fatal("FIFO fallback did not release output")
	}
}

func TestCodeModeNinthCompletionKeepsUnreadOutput(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	ctx := asyncCodeContext("unread-retention", "spawn")
	first, err := e.ExecCodeMode(ctx, `text('x'.repeat(1000))`, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	for first.Status != "completed" {
		first, err = e.WaitCodeMode(ctx, first.CellID, time.Second, 1)
		if err != nil || first.IsError {
			t.Fatalf("%+v %v", first, err)
		}
	}
	if !first.HasMoreOutput {
		t.Fatal("first output already drained")
	}
	waitCodeModeSlotIdle(t, e)
	var drainedID string
	for i := 0; i < codemode.MaxTerminalCells; i++ {
		r, err := e.ExecCodeMode(ctx, `text('drained')`, time.Second, 0)
		if err != nil {
			t.Fatal(err)
		}
		r = awaitCodeModeTerminal(t, e, ctx, r)
		if i == 0 {
			drainedID = r.CellID
		}
		waitCodeModeSlotIdle(t, e)
	}
	if len(e.CodeModeCells("unread-retention")) != codemode.MaxTerminalCells {
		t.Fatal("terminal count grew")
	}
	expired, _ := e.WaitCodeMode(ctx, drainedID, 0, 0)
	if expired.Status != "unknown" {
		t.Fatal("old drained receipt survived")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	untouched, _ := e.WaitCodeMode(cancelled, first.CellID, 0, 1)
	if len(untouched.Parts) != 0 || !untouched.HasMoreOutput {
		t.Fatal("cancelled observer drained output")
	}
	rest, _ := e.WaitCodeMode(ctx, first.CellID, 0, 0)
	if rest.Status != "completed" || len(partsText(rest)) < 990 || rest.HasMoreOutput || rest.OutputTruncated {
		t.Fatalf("unread output lost: %+v", rest)
	}
}
