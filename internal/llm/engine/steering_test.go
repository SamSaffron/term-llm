package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"testing"
	"time"
)

type steeringBridgeProvider struct {
	core.Provider
	execute func(context.Context, string, json.RawMessage) (core.ToolOutput, error)
}

func (p *steeringBridgeProvider) SetToolExecutor(execute func(context.Context, string, json.RawMessage) (core.ToolOutput, error)) {
	p.execute = execute
}

// Neither synthetic cancellation nor a cancelled native bridge certifies actual
// tool completion. Both still allow the user to request Rush while work is active.
func TestRushWaitsUntilCancelledToolActuallyExits(t *testing.T) {
	for _, bridged := range []bool{false, true} {
		name := "engine"
		if bridged {
			name = "native-bridge"
		}
		t.Run(name, func(t *testing.T) {
			tool := newContextIgnoringTool(1)
			released := false
			defer func() {
				if !released {
					close(tool.release)
				}
			}()
			registry := NewToolRegistry()
			registry.Register(tool)
			provider := &steeringBridgeProvider{Provider: mockprovider.NewMockProvider("custom")}
			e := NewEngine(provider, registry)
			e.beginSteeringRun(true)
			e.QueueSteering(QueuedSteering{ID: "guidance", Message: core.UserText("keep this"), Origin: SteeringOriginUser})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if bridged {
					_, err := provider.execute(ctx, tool.Spec().Name, json.RawMessage(`{}`))
					result <- err
				} else {
					_, err, _ := e.executeToolWithCancellation(ctx, tool, json.RawMessage(`{}`))
					result <- err
				}
			}()
			select {
			case <-tool.started:
			case <-time.After(3 * time.Second):
				t.Fatal("tool did not start")
			}
			e.callbackMu.RLock()
			settled := e.steeringToolsSettled
			e.callbackMu.RUnlock()
			if !e.SteeringAvailability().CanRush {
				t.Fatal("active tools must permit cancellation")
			}
			owner := SteeringTransition{OperationID: "rush", Fence: 1}
			entries, err := e.FreezeSteering(owner)
			if err != nil || len(entries) != 1 {
				t.Fatalf("admission: %v, %+v", err, entries)
			}
			cancel()
			if !bridged {
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("synthetic result: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("synthetic cancellation did not return")
				}
			}
			if err := e.WaitSteeringSettlement(ctx, owner); !errors.Is(err, context.Canceled) {
				t.Fatalf("abandoned execution counted as settled: %v", err)
			}
			if !e.SteeringTransitioning() {
				t.Fatal("cancelled wait released freeze")
			}
			select {
			case <-settled:
				t.Fatal("cancellation closed actual settlement barrier")
			default:
			}
			close(tool.release)
			released = true
			select {
			case <-settled:
			case <-time.After(3 * time.Second):
				t.Fatal("actual completion did not settle")
			}
			if err := e.WaitSteeringSettlement(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			e.ReleaseSteeringFreeze(owner, false)
		})
	}
}

func TestSteeringFreezeOwnsWholeQueueAndRollsBack(t *testing.T) {
	e := NewEngine(&fakeProvider{}, nil)
	e.beginSteeringRun(true)
	for _, id := range []string{"b", "a"} {
		e.QueueSteering(QueuedSteering{ID: id, Message: core.UserText(id), Origin: SteeringOriginUser})
	}
	owner := SteeringTransition{OperationID: "rush", Fence: 1}
	entries, err := e.FreezeSteering(owner)
	if err != nil || len(entries) != 2 {
		t.Fatalf("freeze: %+v %v", entries, err)
	}
	if got := e.DrainSteering(); len(got) != 0 {
		t.Fatal("source drained frozen input")
	}
	if _, status := e.QueueSteeringWithStatus(QueuedSteering{ID: "late", Message: core.UserText("late")}); status != SteeringQueueTransitioning {
		t.Fatalf("late input: %s", status)
	}
	if e.CancelSteering("b") || e.DiscardPendingSteering() != 0 {
		t.Fatal("ordinary cancellation stole frozen input")
	}
	if e.ReleaseSteeringFreeze(SteeringTransition{OperationID: "wrong", Fence: 1}, true) {
		t.Fatal("wrong owner released freeze")
	}
	if !e.ReleaseSteeringFreeze(owner, false) {
		t.Fatal("rollback failed")
	}
	got := e.DrainSteering()
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("rollback changed FIFO: %+v", got)
	}
}
