package engine

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"io"
	"sync"
	"testing"
)

type capturingFileRunLifecycle struct {
	mu        sync.Mutex
	starts    [][2]string
	completes [][2]string
}

func (c *capturingFileRunLifecycle) RecordFileTrackingRunStart(_ context.Context, sessionID, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts = append(c.starts, [2]string{sessionID, runID})
	return nil
}
func (c *capturingFileRunLifecycle) RecordFileTrackingRunComplete(_ context.Context, sessionID, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completes = append(c.completes, [2]string{sessionID, runID})
	return nil
}

func TestSimpleConversationalRunPersistsFileTrackingBoundary(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("hello")
	engine := NewEngine(provider, nil)
	recorder := &capturingFileRunLifecycle{}
	engine.SetFileTrackingRunLifecycle(recorder)
	stream, err := engine.Stream(context.Background(), core.Request{SessionID: "session", Messages: []core.Message{core.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		if event.Type == core.EventDone {
			break
		}
	}
	_ = stream.Close()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.starts) != 1 || len(recorder.completes) != 1 {
		t.Fatalf("boundaries starts=%v completes=%v", recorder.starts, recorder.completes)
	}
	if recorder.starts[0][0] != "session" || recorder.starts[0][1] == "" || recorder.starts[0] != recorder.completes[0] {
		t.Fatalf("boundaries starts=%v completes=%v", recorder.starts, recorder.completes)
	}
}

// Synchronous MCP bridges (including claude-bin) must attribute tool effects to
// the same run whose boundaries the engine persists.
func TestSyncToolFileTrackingContext(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "serial", true: "parallel"}[parallel], func(t *testing.T) {
			tool := &fileTrackingContextTool{contexts: make(chan [3]string, 1)}
			registry := NewToolRegistry()
			registry.Register(tool)
			engine := NewEngine(&fileTrackingSyncProvider{}, registry)
			recorder := &capturingFileRunLifecycle{}
			engine.SetFileTrackingRunLifecycle(recorder)
			stream, err := engine.Stream(context.Background(), core.Request{
				SessionID: "session", Messages: []core.Message{core.UserText("write a file")},
				Tools: []core.ToolSpec{tool.Spec()}, ParallelToolCalls: parallel,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if err := drainStreamErr(t, stream); err != nil {
				t.Fatal(err)
			}
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			if len(recorder.starts) != 1 || len(recorder.completes) != 1 || recorder.starts[0] != recorder.completes[0] {
				t.Fatalf("boundaries starts=%v completes=%v", recorder.starts, recorder.completes)
			}
			select {
			case got := <-tool.contexts:
				want := [3]string{"session", recorder.starts[0][1], "sync-call-1"}
				if got != want || got[1] == "" {
					t.Fatalf("tool tracking context = %q, want %q", got, want)
				}
			default:
				t.Fatal("synchronous tool did not execute")
			}
		})
	}
}

type fileTrackingContextTool struct {
	countingTool
	contexts chan [3]string
}

func (t *fileTrackingContextTool) Execute(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
	t.contexts <- [3]string{core.SessionIDFromContext(ctx), core.ToolRunIDFromContext(ctx), core.CallIDFromContext(ctx)}
	return core.TextOutput("ok"), nil
}

type fileTrackingSyncProvider struct{ syncSnapshotProvider }

func (p *fileTrackingSyncProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	for _, message := range req.Messages {
		if message.Role == core.RoleTool {
			return mockprovider.NewMockProvider("done").AddTextResponse("done").Stream(ctx, req)
		}
	}
	return p.syncSnapshotProvider.Stream(ctx, req)
}
