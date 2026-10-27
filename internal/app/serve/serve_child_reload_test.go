package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/restart"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

// A hosted response cannot park independently of the runner waiting for its
// settlement. Exercise Run, not just Execute: Run's outer activity ticket is the
// other half of the production deadlock. Lifecycle turns have the same owner.
func TestHostedChildReloadSettlesBeforeReplacement(t *testing.T) {
	for _, tc := range []struct{ lifecycle, interrupt bool }{{}, {lifecycle: true}, {interrupt: true}, {lifecycle: true, interrupt: true}} {
		lifecycle := tc.lifecycle
		t.Run(fmt.Sprintf("lifecycle=%v/interrupt=%v", lifecycle, tc.interrupt), func(t *testing.T) {
			old := restart.Default
			c := &restart.Coordinator{Timeout: 10 * time.Second, InterruptAfter: time.Hour}
			if tc.interrupt {
				c.InterruptAfter = 20 * time.Millisecond
			}
			restart.Default = c
			defer func() { restart.Default = old }()
			f := newChildRunFixture(t, true)
			f.srv.responseRuns = newServeResponseRunManager()
			defer f.srv.responseRuns.Close()
			id := fmt.Sprintf("reload-child-%d", childBarrierTestSeq.Add(1))
			if err := f.store.Create(t.Context(), &session.Session{ID: id, ParentID: f.parent.ID, IsSubagent: true, Provider: "mock", Model: "mock-model", Mode: session.ModeChat}); err != nil {
				t.Fatal(err)
			}
			if lifecycle {
				if err := session.AsAgentRunStore(f.store).PutAgentRun(t.Context(), session.AgentRun{ID: id + "-pending", ParentSessionID: id, AgentName: "developer", Status: "completed", StopReason: "completed", OwnerInstanceID: "previous-host", RunGeneration: 1, UpdatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			handle := f.srv.ensureChildRuns().ChildRunStarted(childRunInfo{ChildSessionID: id, ParentSessionID: f.parent.ID, CallID: "spawn-reload", Agent: "developer", Prompt: "work"}).(*childRunHandle)
			path := filepath.Join(t.TempDir(), "input.txt")
			if err := os.WriteFile(path, []byte("test input"), 0600); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]any{"path": path})
			mock := mockprovider.NewMockProvider("mock")
			if lifecycle {
				mock.AddTextResponse("own turn")
			}
			mock.AddTurn(mockprovider.MockTurn{Text: "work", ToolCalls: []core.ToolCall{{ID: "read", Name: "read_file", Arguments: args}}}).AddTextResponse("finished")
			// Natural completion requests reload from streamed text, before the
			// completed-response boundary. Interruption requests it from inside
			// the tool: the grace period must not expire before tool dispatch.
			sink := &reloadChildSink{handle: handle, coordinator: c, target: 1, interrupt: tc.interrupt}
			if lifecycle {
				sink.target = 2
			}
			runner := newCmdRunner(f.env.cfg, cmdRunnerOptions{Store: f.store, Tools: "read_file", ToolsSet: true}).(*cmdRunner)
			replacement := make(chan struct{}, 1)
			stop, err := c.Bind(t.Context(), func(context.Context) error {
				// Replacement must not run before the response and borrowed runtime settle.
				f.srv.sessionMgr.mu.Lock()
				borrowed := f.srv.sessionMgr.borrowed[id] != nil
				f.srv.sessionMgr.mu.Unlock()
				if borrowed {
					t.Error("replacement before borrowed runtime released")
				}
				replacement <- struct{}{}
				return errors.New("fixture exec failure")
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			done := make(chan error, 1)
			finished := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cwd := t.TempDir()
			blocked := &reloadSettlementTool{onStart: c.Request, interrupted: make(chan struct{}), settle: make(chan struct{})}
			// Model the nested detached owner taken by the agent manager before
			// launching a child. It too must survive until the runner returns.
			owned, releaseParent, err := c.Root(ctx)
			if err != nil {
				t.Fatal(err)
			}
			childCtx, releaseChild, err := restart.Detached(owned)
			if err != nil {
				t.Fatal(err)
			}
			releaseParent()
			defer func() {
				select {
				case <-blocked.settle:
				default:
					close(blocked.settle)
				}
				cancel()
				stop()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("runner leaked after rollback")
				}
			}()
			go func() {
				defer close(finished)
				_, err := runner.Run(childCtx, runpkg.Request{Platform: runpkg.PlatformWeb, SessionID: id, ParentSessionID: f.parent.ID, IsSubagent: true, Persist: true, Cwd: cwd, ProviderInstance: mock, Prompt: "work", OnEngineReady: func(e *engine.Engine) {
					if tc.interrupt {
						e.RegisterTool(blocked)
					}
				}}, sink)
				releaseChild()
				done <- err
			}()
			if tc.interrupt {
				select {
				case <-blocked.interrupted:
				case <-time.After(time.Second):
					t.Fatal("hosted tool did not receive grace cancellation")
				}
				select {
				case <-replacement:
					t.Fatal("replacement before actual tool settlement")
				default:
				}
				close(blocked.settle)
			}
			select {
			case <-replacement:
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("hosted child checkpoint blocked replacement behind its runner")
			}
			select {
			case err := <-done:
				if tc.interrupt {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, restart.ErrInterrupt) {
						t.Fatalf("interrupted child outcome = %v", err)
					}
				} else if err != nil {
					t.Fatalf("child did not finish naturally during grace: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("hosted runner did not settle")
			}
			if sink.turn != sink.target {
				t.Fatalf("reload requested at turn %d, want %d", sink.turn, sink.target)
			}
		})
	}
}

type reloadChildSink struct {
	handle       childRunSession
	coordinator  *restart.Coordinator
	turn, target int
	interrupt    bool
}

func (s *reloadChildSink) ChildRunSession() childRunSession { return s.handle }
func (s *reloadChildSink) Event(ev core.Event) {
	if ev.Type == core.EventTextDelta && ev.Text != "finished" {
		s.turn++
		if s.turn == s.target && !s.interrupt {
			s.coordinator.Request()
		}
	}
}
func (s *reloadChildSink) BeginChildTurn() {}

// Cancellation is not settlement: the runner must retain its ownership while a
// tool joins, even though the tool has already acknowledged the grace signal.
type reloadSettlementTool struct {
	onStart             func()
	interrupted, settle chan struct{}
}

func (*reloadSettlementTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "read_file", Schema: map[string]any{"type": "object"}}
}
func (*reloadSettlementTool) Preview(json.RawMessage) string { return "reload settlement fixture" }
func (t *reloadSettlementTool) Execute(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
	t.onStart()
	<-ctx.Done()
	close(t.interrupted)
	<-t.settle
	return core.ToolOutput{}, ctx.Err()
}
