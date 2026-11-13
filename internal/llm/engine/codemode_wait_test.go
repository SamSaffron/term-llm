package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode"
)

func TestCodeModeZeroMaxWaitPollsWithoutCancellation(t *testing.T) {
	e := newCodeEngine()
	defer e.CloseCodeMode()
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	started := make(chan struct{}, 1)
	e.RegisterTool(codeModeBlockingTool(started, release))
	ctx, cancel := context.WithTimeout(asyncCodeContext("poll", "exec"), 2*time.Second)
	defer cancel()
	first, err := e.ExecCodeMode(ctx, `await tools.hold({});`, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || first.Yield != 0 || first.Status != "running" || first.IsError {
		t.Fatalf("zero-wait exec did not poll: %+v; context: %v", first, ctx.Err())
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("host call did not start", ctx.Err())
	}
	polled, err := e.WaitCodeMode(ctx, first.CellID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || polled.Yield != 0 || polled.Status != "running" || polled.IsError {
		t.Fatalf("zero-wait observation did not poll: %+v; context: %v", polled, ctx.Err())
	}
	// An observation deadline must leave the cell and its host work running.
	active := e.codeModeSessions.lookup(first.CellID, "poll")
	if active == nil {
		t.Fatal("poll ended the active cell")
	}
	// The production teardown deliberately does not wait for external calls.
	// This fixture owns its blocker, so settle its bridge before the next test
	// changes package-wide compaction environment callbacks.
	unblock()
	final := awaitCodeModeTerminal(t, e, ctx, polled)
	if final.Status != "completed" {
		t.Fatalf("released poll fixture did not settle: %+v", final)
	}
}

func TestCodeModeObservationWaitLimits(t *testing.T) {
	if codemode.SessionLimits.Yield != 60*time.Second || codemode.SessionLimits.MaxYield != 600*time.Second {
		t.Fatalf("observation limits: default=%v max=%v", codemode.SessionLimits.Yield, codemode.SessionLimits.MaxYield)
	}
}
