package llm

import (
	"context"
	"encoding/json"
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
	provider := NewMockProvider("mock").AddTextResponse("hello")
	engine := NewEngine(provider, nil)
	recorder := &capturingFileRunLifecycle{}
	engine.SetFileTrackingRunLifecycle(recorder)
	stream, err := engine.Stream(context.Background(), Request{SessionID: "session", Messages: []Message{UserText("hi")}})
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
		if event.Type == EventDone {
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
			stream, err := engine.Stream(context.Background(), Request{
				SessionID: "session", Messages: []Message{UserText("write a file")},
				Tools: []ToolSpec{tool.Spec()}, ParallelToolCalls: parallel,
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

func (t *fileTrackingContextTool) Execute(ctx context.Context, _ json.RawMessage) (ToolOutput, error) {
	t.contexts <- [3]string{SessionIDFromContext(ctx), ToolRunIDFromContext(ctx), CallIDFromContext(ctx)}
	return TextOutput("ok"), nil
}

type fileTrackingSyncProvider struct{ syncSnapshotProvider }

func (p *fileTrackingSyncProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	for _, message := range req.Messages {
		if message.Role == RoleTool {
			return NewMockProvider("done").AddTextResponse("done").Stream(ctx, req)
		}
	}
	return p.syncSnapshotProvider.Stream(ctx, req)
}
