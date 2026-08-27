package llm

import (
	"context"
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
