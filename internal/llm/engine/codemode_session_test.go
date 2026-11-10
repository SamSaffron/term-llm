package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
)

func asyncCodeContext(sessionID, callID string) context.Context {
	ctx := core.ContextWithSessionID(context.Background(), sessionID)
	if callID != "" {
		ctx = core.ContextWithCallID(ctx, callID)
	}
	return ctx
}

func partsText(result core.CodeModeCallResult) string {
	var b strings.Builder
	for _, part := range result.Parts {
		if part.Type == core.ToolContentPartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func isCodeModeTerminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func awaitCodeModeTerminal(t *testing.T, e *Engine, ctx context.Context, result core.CodeModeCallResult) core.CodeModeCallResult {
	t.Helper()
	for attempts := 0; attempts < 10 && !isCodeModeTerminal(result.Status); attempts++ {
		next, err := e.WaitCodeMode(ctx, result.CellID, 3*time.Second, 0)
		if err != nil {
			t.Fatal(err)
		}
		next.Parts = append(result.Parts, next.Parts...)
		next.Calls = append(result.Calls, next.Calls...)
		result = next
	}
	if !isCodeModeTerminal(result.Status) {
		t.Fatalf("cell did not become terminal: %+v", result)
	}
	return result
}

func waitCodeModeSlotIdle(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		idle := !e.HasAnyCodeModeCells()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("code-mode actual-effect lease never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func codeModeBlockingTool(started chan<- struct{}, release <-chan struct{}) codeTestTool {
	return codeTestTool{name: "hold", run: func(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return core.TextOutput("held"), nil
		case <-ctx.Done():
			return core.ToolOutput{}, ctx.Err()
		}
	}}
}

func TestCodeModeExecYieldWaitResume(t *testing.T) {
	e := newCodeEngine()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.RegisterTool(codeModeBlockingTool(started, release))
	ctx := asyncCodeContext("sess-yield", "call-exec")

	first, err := e.ExecCodeMode(ctx, `text('start'); await tools.hold({}); text('end'); store('k',42);`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "running" || first.IsError {
		t.Fatalf("first observation: %+v", first)
	}
	if got := partsText(first); got != "start" {
		t.Fatalf("first text %q", got)
	}
	if first.StateCommitted {
		t.Fatal("state committed before completion")
	}
	<-started

	yielded, err := e.WaitCodeMode(ctx, first.CellID, 20*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if yielded.Status != "yielded" || yielded.IsError {
		t.Fatalf("wait timeout must not cancel: %+v", yielded)
	}
	if len(yielded.Parts) != 0 {
		t.Fatalf("duplicate output on yield: %+v", yielded.Parts)
	}

	close(release)
	final, err := e.WaitCodeMode(ctx, first.CellID, 3*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "completed" || !final.StateCommitted {
		t.Fatalf("final: %+v", final)
	}
	if got := partsText(final); got != "\nend" {
		t.Fatalf("incremental final text %q", got)
	}
	if len(final.UnsettledCalls) != 0 {
		t.Fatalf("unsettled terminal calls: %+v", final.UnsettledCalls)
	}
	found := false
	for _, call := range final.Calls {
		if call.Name == "hold" && call.Status == "completed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("completed audit for hold missing: %+v", final.Calls)
	}

	loaded, err := e.ExecCodeMode(ctx, `text(load('k'))`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	loaded = awaitCodeModeTerminal(t, e, ctx, loaded)
	if loaded.Status != "completed" || partsText(loaded) != "42" || !loaded.StateCommitted {
		t.Fatalf("automatic state: %+v", loaded)
	}
}

func TestCodeModeCancelRetainsUnresolvedLease(t *testing.T) {
	e := newCodeEngine()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.RegisterTool(codeTestTool{name: "stubborn", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return core.TextOutput("late"), nil
	}})
	ctx := asyncCodeContext("sess-cancel", "call-exec")

	first, err := e.ExecCodeMode(ctx, `await tools.stubborn({}); text('done')`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "yielded" && first.Status != "running" {
		t.Fatalf("unexpected first status: %+v", first)
	}
	<-started

	cancelled, err := e.CancelCodeMode(ctx, first.CellID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" || !cancelled.IsError {
		t.Fatalf("cancel result: %+v", cancelled)
	}
	if cancelled.StateCommitted {
		t.Fatal("cancelled cell committed state")
	}
	if len(cancelled.UnsettledCalls) == 0 {
		t.Fatalf("expected unresolved external effect: %+v", cancelled)
	}

	busy, err := e.ExecCodeMode(ctx, `text('other')`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	busy = awaitCodeModeTerminal(t, e, ctx, busy)
	if busy.Status != "completed" {
		t.Fatalf("unrelated cell must proceed while one place is quarantined: %+v", busy)
	}

	close(release)
	waitCodeModeSlotIdle(t, e)
	next, err := e.ExecCodeMode(ctx, `text('ok')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	next = awaitCodeModeTerminal(t, e, ctx, next)
	if next.Status != "completed" || partsText(next) != "ok" {
		t.Fatalf("exec after lease release: %+v", next)
	}
}

func TestCodeModeCancelIsNonDestructiveAfterCompletion(t *testing.T) {
	e := newCodeEngine()
	ctx := asyncCodeContext("sess-cancel-finished", "call-exec")
	result, err := e.ExecCodeMode(ctx, `text('done')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	result = awaitCodeModeTerminal(t, e, ctx, result)
	if result.Status != "completed" {
		t.Fatalf("setup: %+v", result)
	}
	cancelled, err := e.CancelCodeMode(ctx, result.CellID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "completed" || !cancelled.StateCommitted {
		t.Fatalf("cancel rewrote terminal status: %+v", cancelled)
	}
}

func TestCodeModeStateCommitRollbackAndIsolation(t *testing.T) {
	e := newCodeEngine()
	ctxA := asyncCodeContext("sess-a", "")
	ctxB := asyncCodeContext("sess-b", "")

	committed, err := e.ExecCodeMode(ctxA, `store('k',42); text('ok')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	committed = awaitCodeModeTerminal(t, e, ctxA, committed)
	if committed.Status != "completed" || !committed.StateCommitted || committed.StateBytes == 0 {
		t.Fatalf("commit: %+v", committed)
	}

	failed, err := e.ExecCodeMode(ctxA, `text(load('k')); store('bad',9); throw Error('boom')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	failed = awaitCodeModeTerminal(t, e, ctxA, failed)
	if failed.Status != "failed" || failed.StateCommitted {
		t.Fatalf("failed cell committed: %+v", failed)
	}
	if got := partsText(failed); got != "42" {
		t.Fatalf("failed cell output %q", got)
	}

	rollback, err := e.ExecCodeMode(ctxA, `text(load('bad')===undefined); text(load('k'))`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	rollback = awaitCodeModeTerminal(t, e, ctxA, rollback)
	if rollback.Status != "completed" || partsText(rollback) != "true\n42" {
		t.Fatalf("rollback: %+v text=%q", rollback, partsText(rollback))
	}

	isolated, err := e.ExecCodeMode(ctxB, `text(load('k')===undefined)`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	isolated = awaitCodeModeTerminal(t, e, ctxB, isolated)
	if isolated.Status != "completed" || partsText(isolated) != "true" {
		t.Fatalf("cross-session leak: %+v text=%q", isolated, partsText(isolated))
	}

	stillA, err := e.ExecCodeMode(ctxA, `text(load('k'))`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	stillA = awaitCodeModeTerminal(t, e, ctxA, stillA)
	if stillA.Status != "completed" || partsText(stillA) != "42" {
		t.Fatalf("session A state lost: %+v", stillA)
	}

	perValue, err := e.ExecCodeMode(ctxA, `store('big','x'.repeat(300*1024)); text('done')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	perValue = awaitCodeModeTerminal(t, e, ctxA, perValue)
	if perValue.Status != "failed" || perValue.StateCommitted {
		t.Fatalf("per-value limit: %+v", perValue)
	}
	afterPerValue, err := e.ExecCodeMode(ctxA, `text(load('big')===undefined); text(load('k'))`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterPerValue = awaitCodeModeTerminal(t, e, ctxA, afterPerValue)
	if afterPerValue.Status != "completed" || partsText(afterPerValue) != "true\n42" {
		t.Fatalf("state after per-value failure: %+v", afterPerValue)
	}

	total, err := e.ExecCodeMode(ctxA, `for(let i=0;i<5;i++)store('v'+i,'x'.repeat(250*1024)); text('done')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	total = awaitCodeModeTerminal(t, e, ctxA, total)
	if total.Status != "failed" || total.StateCommitted {
		t.Fatalf("total state limit: %+v", total)
	}
	afterTotal, err := e.ExecCodeMode(ctxA, `text(load('k'))`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterTotal = awaitCodeModeTerminal(t, e, ctxA, afterTotal)
	if afterTotal.Status != "completed" || partsText(afterTotal) != "42" {
		t.Fatalf("state after total failure: %+v", afterTotal)
	}
}

func testImageTool(t *testing.T) codeTestTool {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	return codeTestTool{name: "pic", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.ToolOutput{Content: "image", ContentParts: []core.ToolContentPart{{
			Type:      core.ToolContentPartImageData,
			ImageData: &core.ToolImageData{MediaType: "image/png", Base64: data},
		}}}, nil
	}}
}

func TestCodeModeIncrementalImagesAndStateContamination(t *testing.T) {
	e := newCodeEngine()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.RegisterTool(testImageTool(t))
	e.RegisterTool(codeModeBlockingTool(started, release))
	ctx := asyncCodeContext("sess-image", "call-exec")

	first, err := e.ExecCodeMode(ctx, `const r=await tools.pic({}); image(r.content[0].handle); await tools.hold({});`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "running" || len(first.Parts) != 1 || first.Parts[0].Type != core.ToolContentPartImageData || first.Parts[0].ImageData == nil {
		t.Fatalf("image observation: %+v", first)
	}
	<-started
	close(release)
	final, err := e.WaitCodeMode(ctx, first.CellID, 3*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	final = awaitCodeModeTerminal(t, e, ctx, final)
	if final.Status != "completed" {
		t.Fatalf("image cell final: %+v", final)
	}
	for _, part := range final.Parts {
		if part.Type == core.ToolContentPartImageData {
			t.Fatalf("duplicate image delivery: %+v", final.Parts)
		}
	}

	contaminated, err := e.ExecCodeMode(ctx, `const r=await tools.pic({}); store('h',r.content[0].handle); text('done')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	contaminated = awaitCodeModeTerminal(t, e, ctx, contaminated)
	if contaminated.Status != "failed" || contaminated.StateCommitted {
		t.Fatalf("image handle entered state: %+v", contaminated)
	}
	clean, err := e.ExecCodeMode(ctx, `text(load('h')===undefined)`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	clean = awaitCodeModeTerminal(t, e, ctx, clean)
	if clean.Status != "completed" || partsText(clean) != "true" {
		t.Fatalf("state after image rejection: %+v", clean)
	}
}

func TestCodeModeOutputBudgetSplitsWithoutDuplication(t *testing.T) {
	e := newCodeEngine()
	ctx := asyncCodeContext("sess-output", "call-exec")
	first, err := e.ExecCodeMode(ctx, `text('a'.repeat(100)); text('b')`, 2*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "completed" || !first.OutputLimited || !first.HasMoreOutput || first.OutputTruncated {
		t.Fatalf("first limited observation: %+v", first)
	}
	if first.MaxOutputTokens != 1 || len(first.Parts) != 1 || len(first.Parts[0].Text) != 4 {
		t.Fatalf("first chunk: %+v", first)
	}
	second, err := e.WaitCodeMode(ctx, first.CellID, time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "completed" || second.HasMoreOutput || second.OutputLimited {
		t.Fatalf("second observation: %+v", second)
	}
	if got := partsText(first) + partsText(second); got != strings.Repeat("a", 100)+"\nb" {
		t.Fatalf("duplicated or lost output: %d", len(got))
	}
}

func TestCodeModeSessionMismatchFailsClosedWithoutLeak(t *testing.T) {
	e := newCodeEngine()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.RegisterTool(codeTestTool{name: "stubborn", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return core.TextOutput("late"), nil
	}})
	ctxA := asyncCodeContext("sess-bind-a", "call-a")
	ctxB := asyncCodeContext("sess-bind-b", "call-b")

	if result, err := e.ExecCodeMode(ctxA, `store('k',42); text('ok')`, 2*time.Second, 0); err != nil || result.Status != "completed" {
		t.Fatalf("commit A: %+v %v", result, err)
	}
	first, err := e.ExecCodeMode(ctxA, `await tools.stubborn({})`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	busy, err := e.ExecCodeMode(ctxB, `text('x')`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	busy = awaitCodeModeTerminal(t, e, ctxB, busy)
	if busy.Status != "completed" || busy.CellID == first.CellID {
		t.Fatalf("separate session admission must be isolated: %+v", busy)
	}
	unknownEmpty, err := e.WaitCodeMode(context.Background(), first.CellID, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if unknownEmpty.Status != "unknown" {
		t.Fatalf("empty SessionID must not wildcard-match: %+v", unknownEmpty)
	}
	cancelledA, err := e.CancelCodeMode(ctxA, first.CellID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelledA.Status != "cancelled" {
		t.Fatalf("owner cancel: %+v", cancelledA)
	}
	close(release)
	waitCodeModeSlotIdle(t, e)

	isolated, err := e.ExecCodeMode(ctxB, `text(load('k')===undefined)`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	isolated = awaitCodeModeTerminal(t, e, ctxB, isolated)
	if isolated.Status != "completed" || partsText(isolated) != "true" {
		t.Fatalf("cross-session state: %+v", isolated)
	}
	unknown, err := e.WaitCodeMode(ctxB, first.CellID, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Status != "unknown" || !unknown.IsError {
		t.Fatalf("cross-session cell access: %+v", unknown)
	}
	stillA, err := e.ExecCodeMode(ctxA, `text(load('k'))`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	stillA = awaitCodeModeTerminal(t, e, ctxA, stillA)
	if stillA.Status != "completed" || partsText(stillA) != "42" {
		t.Fatalf("session A lost state: %+v", stillA)
	}
}

func TestCodeModeSessionTeardownClearsStateAndRetainsLease(t *testing.T) {
	e := newCodeEngine()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.RegisterTool(codeTestTool{name: "stubborn", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return core.TextOutput("late"), nil
	}})
	ctx := asyncCodeContext("sess-teardown", "call-exec")
	setup, err := e.ExecCodeMode(ctx, `store('k',42); text('ok')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	setup = awaitCodeModeTerminal(t, e, ctx, setup)
	if setup.Status != "completed" {
		t.Fatalf("setup commit: %+v", setup)
	}
	if result, err := e.ExecCodeMode(ctx, `await tools.stubborn({})`, 10*time.Millisecond, 0); err != nil || result.Status == "completed" {
		t.Fatalf("setup running cell: %+v %v", result, err)
	}
	<-started
	e.CloseCodeMode()

	if state := string(e.codeModeSessions.store.State("sess-teardown")); state != "{}" {
		t.Fatalf("teardown retained state: %s", state)
	}
	leaseHeld := e.HasAnyCodeModeCells()
	if !leaseHeld {
		t.Fatal("teardown released an unresolved external-effect lease")
	}
	closed, err := e.ExecCodeMode(ctx, `text('blocked')`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "failed" || closed.ErrorCode != "engine_closed" || closed.CellID != "" {
		t.Fatalf("teardown did not latch admission: %+v", closed)
	}
	close(release)
	waitCodeModeSlotIdle(t, e)
	stillClosed, err := e.ExecCodeMode(ctx, `text('still blocked')`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stillClosed.Status != "failed" || stillClosed.ErrorCode != "engine_closed" {
		t.Fatalf("teardown latch did not persist: %+v", stillClosed)
	}
}

func TestCodeModeBorrowedCancelKeepsEngineReusable(t *testing.T) {
	e := newCodeEngine()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.RegisterTool(codeModeBlockingTool(started, release))
	ctx := asyncCodeContext("sess-borrowed", "")
	first, err := e.ExecCodeMode(ctx, `await tools.hold({})`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	e.CancelCodeModeCells()
	close(release)
	waitCodeModeSlotIdle(t, e)
	if first.CellID == "" {
		t.Fatal("missing cell id")
	}
	next, err := e.ExecCodeMode(ctx, `text('reusable')`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	next = awaitCodeModeTerminal(t, e, ctx, next)
	if next.Status != "completed" || partsText(next) != "reusable" {
		t.Fatalf("borrowed cancel latched the engine: %+v", next)
	}
}

func TestCodeModeCloseExecRaceNeverStartsAfterClose(t *testing.T) {
	for i := 0; i < 12; i++ {
		e := newCodeEngine()
		ctx := asyncCodeContext("sess-race", "")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = e.ExecCodeMode(ctx, `await sleep(30); text('late')`, 20*time.Millisecond, 0)
		}()
		go func() {
			defer wg.Done()
			e.CloseCodeMode()
		}()
		wg.Wait()
		closed, err := e.ExecCodeMode(ctx, `text('after')`, 10*time.Millisecond, 0)
		if err != nil {
			t.Fatal(err)
		}
		if closed.Status != "failed" || closed.ErrorCode != "engine_closed" {
			t.Fatalf("iteration %d reopened closed engine: %+v", i, closed)
		}
		waitCodeModeSlotIdle(t, e)
	}
}

func TestCodeModeResetSessionStateClearsOnlyThatSession(t *testing.T) {
	e := newCodeEngine()
	ctxA := asyncCodeContext("sess-reset-a", "")
	ctxB := asyncCodeContext("sess-reset-b", "")
	for _, tc := range []struct {
		ctx   context.Context
		value int
	}{
		{ctxA, 1},
		{ctxB, 2},
	} {
		result, err := e.ExecCodeMode(tc.ctx, `store('k',`+strconv.Itoa(tc.value)+`)`, 2*time.Second, 0)
		if err != nil {
			t.Fatal(err)
		}
		result = awaitCodeModeTerminal(t, e, tc.ctx, result)
		if result.Status != "completed" {
			t.Fatalf("commit %d: %+v", tc.value, result)
		}
	}
	e.ResetSessionState("sess-reset-a")
	cleared, err := e.ExecCodeMode(ctxA, `text(load('k')===undefined)`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	cleared = awaitCodeModeTerminal(t, e, ctxA, cleared)
	if cleared.Status != "completed" || partsText(cleared) != "true" {
		t.Fatalf("session A not cleared: %+v", cleared)
	}
	kept, err := e.ExecCodeMode(ctxB, `text(load('k'))`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	kept = awaitCodeModeTerminal(t, e, ctxB, kept)
	if kept.Status != "completed" || partsText(kept) != "2" {
		t.Fatalf("session B lost or leaked: %+v", kept)
	}
}

func waitAsyncCellDone(t *testing.T, e *Engine, cellID, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		cell := e.codeModeSessions.lookup(cellID, sessionID)
		if cell == nil {
			t.Fatal("async cell vanished")
		}
		cell.mu.Lock()
		done := cell.done
		cell.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("async cell never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCodeModeStartupCancellationRaceCancelsMachine(t *testing.T) {
	machine, err := codemode.StartCell(context.Background(), codemode.CellSpec{Input: codemode.Input{Code: `while(true){}`}})
	if err != nil {
		t.Fatal(err)
	}
	cell := newCodeModeAsyncCell(&codeModeSessions{}, "cm_race", "sess-race", nil)
	cell.requestCancel()
	cell.attachMachine(machine)
	select {
	case <-machine.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("machine was never cancelled after late attachment")
	}
	result, ok := machine.Result()
	if !ok || result.Status != "cancelled" {
		t.Fatalf("late-attached cancellation: %+v", result)
	}
}

func TestCodeModeDeferredTextKeepsImagesBehindIt(t *testing.T) {
	e := newCodeEngine()
	e.RegisterTool(testImageTool(t))
	ctx := asyncCodeContext("sess-order", "call-exec")
	first, err := e.ExecCodeMode(ctx, `const r=await tools.pic({}); text('a'.repeat(10)); image(r.content[0].handle); text('b');`, 2*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	first = awaitCodeModeTerminal(t, e, ctx, first)
	if !first.OutputLimited || !first.HasMoreOutput {
		t.Fatalf("expected deferred output: %+v", first)
	}
	if len(first.Parts) != 1 || first.Parts[0].Type != core.ToolContentPartText || first.Parts[0].Text != "aaaa" {
		t.Fatalf("first observation reordered deferred items: %+v", first.Parts)
	}
	second, err := e.WaitCodeMode(ctx, first.CellID, time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "completed" || second.HasMoreOutput || second.OutputLimited {
		t.Fatalf("second observation: %+v", second)
	}
	if len(second.Parts) != 3 {
		t.Fatalf("deferred order: %+v", second.Parts)
	}
	if second.Parts[0].Type != core.ToolContentPartText || second.Parts[0].Text != "aaaaaa" {
		t.Fatalf("deferred text: %+v", second.Parts[0])
	}
	if second.Parts[1].Type != core.ToolContentPartImageData {
		t.Fatalf("image overtook deferred text: %+v", second.Parts)
	}
	if second.Parts[2].Type != core.ToolContentPartText || second.Parts[2].Text != "\nb" {
		t.Fatalf("trailing text: %+v", second.Parts[2])
	}
}

func TestCodeModeCanceledObserverRetainsOutputAndAudit(t *testing.T) {
	e := newCodeEngine()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.RegisterTool(codeModeBlockingTool(started, release))
	ctx := asyncCodeContext("sess-canceled-observer", "call-exec")
	first, err := e.ExecCodeMode(ctx, `await tools.hold({}); text('done')`, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "yielded" && first.Status != "running" {
		t.Fatalf("setup status: %+v", first)
	}
	<-started
	close(release)
	waitAsyncCellDone(t, e, first.CellID, "sess-canceled-observer")

	canceledCtx, cancel := context.WithCancel(asyncCodeContext("sess-canceled-observer", "call-canceled"))
	cancel()
	canceledView, err := e.WaitCodeMode(canceledCtx, first.CellID, 10*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(canceledView.Parts) != 0 || len(canceledView.Calls) != 0 {
		t.Fatalf("canceled observer consumed terminal output/audit: %+v", canceledView)
	}

	final, err := e.WaitCodeMode(ctx, first.CellID, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "completed" || partsText(final) != "done" {
		t.Fatalf("live observer lost output: %+v", final)
	}
	found := false
	for _, call := range final.Calls {
		if call.Name == "hold" && call.Status == "completed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("live observer lost audit cursor: %+v", final.Calls)
	}
}

func TestCodeModeControlToolsRequireDirectEngineContext(t *testing.T) {
	e := newCodeEngine()
	calls := 0
	e.RegisterTool(codeTestTool{name: codemode.ExecToolName, run: func(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
		calls++
		if core.CodeModeControllerFromContext(ctx) == nil {
			return core.ToolOutput{}, fmt.Errorf("missing engine controller")
		}
		return core.TextOutput("ok"), nil
	}})
	outcome := e.executeSingleToolCallOutcome(context.Background(), core.ToolCall{ID: "call-direct", Name: codemode.ExecToolName, Arguments: json.RawMessage(`{}`)}, corestream.Sender{}, false, false)
	if outcome.err != nil || calls != 1 {
		t.Fatalf("direct control execution: %+v calls=%d", outcome.err, calls)
	}
	cell := admitTestCell(t, e)
	if _, err := cell.Call(context.Background(), codemode.ExecToolName, json.RawMessage(`{}`)); err == nil {
		t.Fatal("nested control tool executed")
	}
	if calls != 1 {
		t.Fatalf("nested control reached dispatcher: %d", calls)
	}
}

func TestCodeModeLaterWaitRoutesChildEvents(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	started, release := make(chan struct{}, 1), make(chan struct{})
	e.RegisterTool(codeModeBlockingTool(started, release))
	oldCtx, retire := context.WithCancel(asyncCodeContext("route", "spawn"))
	oldEvents := make(chan core.Event, 100)
	first, err := e.codeModeSessions.exec(oldCtx, `text('start'); await tools.hold({}); text('end');`, time.Second, 0, corestream.NewSender(oldCtx, oldEvents))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	retire()
	close(oldEvents)
	ctx := asyncCodeContext("route", "later-wait")
	events := make(chan core.Event, 100)
	controller := &codeModeController{engine: e, send: corestream.NewSender(ctx, events)}
	done := make(chan core.CodeModeCallResult, 1)
	go func() { r, _ := controller.WaitCodeMode(ctx, first.CellID, time.Second, 0); done <- r }()
	cell := e.codeModeSessions.lookup(first.CellID, "route")
	deadline := time.Now().Add(time.Second)
	for cell.capability.currentSender().Chan() != events {
		if time.Now().After(deadline) {
			t.Fatal("wait did not bind its event transport")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	result := <-done
	result = awaitCodeModeTerminal(t, e, ctx, result)
	if result.Status != "completed" {
		t.Fatalf("%+v", result)
	}
	found := false
	for len(events) > 0 {
		ev := <-events
		if ev.Type == core.EventToolExecEnd && ev.ToolName == "hold" {
			found = true
		}
	}
	if !found {
		t.Fatal("later wait missed the nested tool end event")
	}
}

func TestCodeModeHumanInterruptAndScopedReset(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	_, _, err := e.codeModeSessions.store.Apply("owner", json.RawMessage(`{"set":{"saved":42},"delete":[]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	e.RegisterTool(codeModeBlockingTool(started, make(chan struct{})))
	ctx := asyncCodeContext("owner", "spawn")
	first, err := e.ExecCodeMode(ctx, `store('saved',99); text('running'); await tools.hold({});`, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	e.ResetSessionState("")
	e.ResetSessionState("other")
	if e.InterruptCodeModeSession("") || e.InterruptCodeModeSession("other") {
		t.Fatal("interrupt crossed session boundary")
	}
	if !e.InterruptCodeModeSession("owner") {
		t.Fatal("human interrupt did not find running cell")
	}
	final := awaitCodeModeTerminal(t, e, ctx, first)
	if final.Status != "cancelled" || final.StateCommitted {
		t.Fatalf("%+v", final)
	}
	if got := string(e.codeModeSessions.store.State("owner")); got != `{"saved":42}` {
		t.Fatalf("committed store changed: %s", got)
	}
	// A terminal receipt can precede actual-call settlement and asynchronous
	// admission-place reclamation; interrupt includes those live leases.
	waitCodeModeSlotIdle(t, e)
	if e.InterruptCodeModeSession("owner") {
		t.Fatal("terminal cell reported as active")
	}
	e.ResetSessionState("owner")
	if got := string(e.codeModeSessions.store.State("owner")); got != `{}` {
		t.Fatalf("reset failed: %s", got)
	}
}

func TestCodeModeUserErrorClassification(t *testing.T) {
	for _, message := range []string{"image", "state_limit"} {
		t.Run(message, func(t *testing.T) {
			e := newCodeEngine()
			defer e.CloseCodeMode()
			ctx := asyncCodeContext("script-error", "exec")
			result, err := e.ExecCodeMode(ctx, "throw Error("+strconv.Quote(message)+")", 2*time.Second, 0)
			if err != nil {
				t.Fatal(err)
			}
			result = awaitCodeModeTerminal(t, e, ctx, result)
			if result.Status != "failed" || result.ErrorCode != "script_error" || !strings.Contains(result.Error, message) {
				t.Fatalf("user error misclassified: %+v", result)
			}
		})
	}
}

func TestCodeModeResourceErrorClassification(t *testing.T) {
	for message, want := range map[string]string{
		"vm_capacity: process VM capacity": "vm_capacity", "state_limit: result too large": "state_limit",
		"state_total_limit": "state_limit", "unawaited_calls": "unawaited_calls",
		"SyntaxError: bad syntax":                                      "script_error",
		"script_error: Error: image":                                   "script_error",
		"script_error: Error: state_limit":                             "script_error",
		"script_error: Error: busy: VM capacity":                       "script_error",
		"script_error: Error: output_limit call_limit unawaited_calls": "script_error",
	} {
		if got := codeModeScriptErrorCode(message); got != want {
			t.Fatalf("%s: got %s, want %s", message, got, want)
		}
	}
}
