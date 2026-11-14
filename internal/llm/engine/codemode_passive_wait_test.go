package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestCodeModePassiveWaitsDoNotStarveAnotherCell(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	e.RegisterTool(codeTestTool{name: "passive", run: func(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
		return core.WithCodeModeWait(ctx, func() (core.ToolOutput, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return core.TextOutput("finished"), nil
			case <-ctx.Done():
				return core.ToolOutput{}, ctx.Err()
			}
		})
	}})
	e.RegisterTool(codeTestTool{name: "quick", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.TextOutput("other cell made progress"), nil
	}})
	ctx := asyncCodeContext("wait-fairness", "wait-cell")
	waiting, err := e.ExecCodeMode(ctx, `await Promise.all([0,1,2,3].map(()=>tools.passive({})));`, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("passive waits did not enter")
		}
	}
	quick, err := e.ExecCodeMode(ctx, `text((await tools.quick({})).text)`, 2*time.Second, 0)
	if err != nil || quick.Status != "completed" || partsText(quick) != "other cell made progress" {
		t.Fatalf("waiters starved independent tool call: %+v, %v", quick, err)
	}
	unblock()
	waiting = awaitCodeModeTerminal(t, e, ctx, waiting)
	if waiting.Status != "completed" {
		t.Fatalf("waiters failed after resuming: %+v", waiting)
	}
}
