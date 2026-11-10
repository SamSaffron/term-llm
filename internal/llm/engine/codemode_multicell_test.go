package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestCodeModeEightPerSessionIndependentStop(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	release := make(chan struct{})
	e.RegisterTool(codeModeBlockingTool(make(chan struct{}, 32), release))
	owners := map[string][]core.CodeModeCallResult{}
	for _, id := range []string{"eight-a", "eight-b"} {
		ctx := asyncCodeContext(id, "spawn")
		for i := range 8 {
			r, err := e.ExecCodeMode(ctx, fmt.Sprintf(`text(%d);await tools.hold({});text('done')`, i), time.Second, 0)
			if err != nil || r.IsError || r.CellID == "" {
				t.Fatalf("session %s cell%d: %+v %v", id, i, r, err)
			}
			owners[id] = append(owners[id], r)
		}
		full, err := e.ExecCodeMode(ctx, `return 9`, time.Millisecond, 0)
		if err != nil || full.ErrorCode != "session_cell_limit" || len(full.LiveCellIDs) != 8 {
			t.Fatalf("ninth: %+v %v", full, err)
		}
	}
	a, b := owners["eight-a"], owners["eight-b"]
	cancelled, err := e.CancelCodeMode(asyncCodeContext("eight-a", "cancel"), a[0].CellID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if r, _ := e.WaitCodeMode(asyncCodeContext("eight-b", "wait"), a[1].CellID, time.Millisecond, 0); r.Status != "unknown" {
		t.Fatal("cross-session handle leaked")
	}
	for _, r := range e.CodeModeCells("eight-a") {
		if r.CellID != a[0].CellID && !r.Live {
			t.Fatalf("target cancel stopped sibling %+v", r)
		}
	}
	if !e.InterruptCodeModeSession("eight-a") {
		t.Fatal("Stop missed cells")
	}
	for _, r := range a {
		got, _ := e.WaitCodeMode(asyncCodeContext("eight-a", "wait"), r.CellID, time.Second, 0)
		if got.Status != "cancelled" {
			t.Fatalf("stop: %+v", got)
		}
	}
	for _, r := range b {
		got, _ := e.WaitCodeMode(asyncCodeContext("eight-b", "wait"), r.CellID, time.Millisecond, 0)
		if isCodeModeTerminal(got.Status) {
			t.Fatalf("Stop crossed session %+v", got)
		}
	}
	close(release)
	for _, r := range b {
		got := awaitCodeModeTerminal(t, e, asyncCodeContext("eight-b", "wait"), r)
		if got.Status != "completed" || !strings.Contains(partsText(got), "done") {
			t.Fatalf("independent output %+v", got)
		}
	}
	waitCodeModeSlotIdle(t, e)
}
func TestCodeModeConcurrentNinthAdmission(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	release := make(chan struct{})
	defer close(release)
	e.RegisterTool(codeModeBlockingTool(make(chan struct{}, 16), release))
	var wg sync.WaitGroup
	var admitted, busy atomic.Int32
	for range 9 {
		wg.Go(func() {
			r, err := e.ExecCodeMode(asyncCodeContext("race-eight", "spawn"), `text('ready');await tools.hold({})`, time.Second, 0)
			if err != nil {
				t.Error(err)
			}
			if r.ErrorCode == "session_cell_limit" {
				busy.Add(1)
			} else if !r.IsError {
				admitted.Add(1)
			} else {
				t.Errorf("admission: %+v", r)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 8 || busy.Load() != 1 {
		t.Fatalf("admitted=%d busy=%d", admitted.Load(), busy.Load())
	}
}
func TestCodeModeSnapshotWriteSetCompletionOrder(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	ctx := asyncCodeContext("writes", "spawn")
	gates := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	e.RegisterTool(codeTestTool{name: "gate", run: func(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
		var arg struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(args, &arg)
		select {
		case <-gates[arg.Key]:
			return core.TextOutput("ok"), nil
		case <-ctx.Done():
			return core.ToolOutput{}, ctx.Err()
		}
	}})
	x, _ := e.ExecCodeMode(ctx, `store('a',1);store('k','first');text('ready');await tools.gate({key:'a'})`, time.Second, 0)
	y, _ := e.ExecCodeMode(ctx, `store('b',2);store('k','last');text('ready');await tools.gate({key:'b'})`, time.Second, 0)
	close(gates["a"])
	x = awaitCodeModeTerminal(t, e, ctx, x)
	close(gates["b"])
	y = awaitCodeModeTerminal(t, e, ctx, y)
	if x.Status != "completed" || y.Status != "completed" || strings.Join(y.StateOverwrittenKeys, ",") != "k" {
		t.Fatalf("x=%+v y=%+v", x, y)
	}
	if got := string(e.codeModeSessions.store.State("writes")); got != `{"a":1,"b":2,"k":"last"}` {
		t.Fatal(got)
	}
	failed, _ := e.ExecCodeMode(ctx, `store('a',99);throw Error('fail')`, time.Second, 0)
	failed = awaitCodeModeTerminal(t, e, ctx, failed)
	if failed.StateCommitted {
		t.Fatal("failure committed")
	}
	deleted, _ := e.ExecCodeMode(ctx, `store('a',undefined)`, time.Second, 0)
	deleted = awaitCodeModeTerminal(t, e, ctx, deleted)
	if deleted.Status != "completed" || strings.Contains(string(e.codeModeSessions.store.State("writes")), `"a"`) {
		t.Fatal("delete lost")
	}
	// Request-only roster is regenerated even if the transcript has been compacted away.
	prepared := e.prepareProviderRequest(core.Request{SessionID: "writes", Messages: []core.Message{core.UserText("next")}})
	found := false
	for _, m := range prepared.Messages {
		for _, p := range m.Parts {
			found = found || strings.Contains(p.Text, "<live_code_cells>")
		}
	}
	if !found {
		t.Fatal("next-turn roster missing")
	}
}
func TestCodeModeSessionToolConcurrencyShared(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	release := make(chan struct{})
	defer close(release)
	var live, maxLive atomic.Int32
	started := make(chan struct{}, 64)
	e.RegisterTool(codeTestTool{name: "limited", run: func(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
		n := live.Add(1)
		defer live.Add(-1)
		for old := maxLive.Load(); n > old && !maxLive.CompareAndSwap(old, n); old = maxLive.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
			return core.TextOutput("ok"), nil
		case <-ctx.Done():
			return core.ToolOutput{}, ctx.Err()
		}
	}})
	for range 8 {
		r, _ := e.ExecCodeMode(asyncCodeContext("shared", "spawn"), `text('ready');await Promise.all([1,2,3,4].map(()=>tools.limited({})))`, time.Second, 0)
		if r.IsError {
			t.Fatalf("spawn %+v", r)
		}
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("four tools not dispatched")
		}
	}
	if maxLive.Load() != 4 {
		t.Fatalf("session executing tools multiplied: %d", maxLive.Load())
	}
}
