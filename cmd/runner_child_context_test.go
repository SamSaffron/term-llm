package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Exercise the same shared runner boundary as spawn_agent, with a real SQLite
// fence. A mock store cannot catch a parent fence used for a child transcript.
func TestCmdRunnerChildDoesNotInheritParentResponseOwnership(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const parentID = "parent-session"
	if err := store.Create(ctx, &session.Session{ID: parentID, Provider: "mock", Model: "mock-model"}); err != nil {
		t.Fatal(err)
	}
	parent := newResponseRun("parent-response", parentID, "", "mock-model", time.Now().Unix(), nil)
	lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: parent.id, SessionID: parentID, OwnerInstanceID: "parent-owner", RunEpoch: parent.runEpoch})
	if err != nil {
		t.Fatal(err)
	}
	server := &serveServer{store: store}
	server.configureResponseRunLifecycle(parent, store, "parent-owner", lease)
	server.configureResponseRunRevision(parent, parentID)
	parentCtx := withResponseRunContext(ctx, parent)
	// Include a low-level fence too: children must not inherit either form of
	// ownership, even if invoked from inside a response-scoped persistence call.
	parentCtx = session.WithResponseRunFence(parentCtx, session.ResponseRunFence{ResponseID: parent.id, OwnerInstanceID: "parent-owner", FencingToken: lease.FencingToken})
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store})
	includeTools := false
	var streamed strings.Builder
	result, err := runner.Run(parentCtx, runpkg.Request{
		Platform: runpkg.PlatformConsole, Prompt: "do child work", SessionID: "child-session", ParentSessionID: parentID,
		IsSubagent: true, Persist: true, Cwd: t.TempDir(), IncludeConfiguredTools: &includeTools,
		ProviderInstance: llm.NewMockProvider("mock").AddTextResponse("child result"),
	}, eventSinkFunc(func(event llm.Event) {
		if event.Type == llm.EventTextDelta {
			streamed.WriteString(event.Text)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "child result" || streamed.String() != "child result" {
		t.Fatalf("result/stream = %q/%q", result.Response, streamed.String())
	}
	messages, err := store.GetMessages(ctx, "child-session", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundAnswer := false
	for _, msg := range messages {
		if msg.ResponseID == parent.id {
			t.Fatalf("child message attributed to parent: %+v", msg)
		}
		if msg.Role == llm.RoleAssistant && msg.TextContent == "child result" {
			foundAnswer = true
		}
	}
	if !foundAnswer {
		t.Fatalf("child answer not persisted: %+v", messages)
	}
	handoff := parent.readDurableHandoff()
	if !handoff.Valid || handoff.OutputCount != 0 || handoff.FinalRev != 0 {
		t.Fatalf("child contaminated parent handoff: %+v", handoff)
	}
	if err := store.ValidateResponseRunLease(ctx, parent.id, "parent-owner", lease.FencingToken); err != nil {
		t.Fatalf("parent lease changed: %v", err)
	}
	if responseRunFromContext(parentCtx) != parent {
		t.Fatal("original parent context was changed")
	}
	// The parent must still persist and terminalize using its own ownership.
	assistant := tagResponseRunMessage(parentCtx, llm.AssistantText("parent result"), 0)
	if _, err := runResponseRunPersistence(parentCtx, []llm.Message{assistant}, func(fence session.ResponseRunFence) (int64, error) {
		return addResponseRunMessage(session.WithResponseRunFence(ctx, fence), store, parentID, session.NewMessage(parentID, assistant, -1))
	}); err != nil {
		t.Fatal(err)
	}
	if err := parent.complete(map[string]any{"response": map[string]any{"id": parent.id}}, llm.Usage{}, llm.Usage{}); err != nil {
		t.Fatal(err)
	}
	if parent.status != "completed" || !parent.durableHandoff || parent.durableOutputCount != 1 {
		t.Fatalf("parent completion = status:%s durable:%v count:%d error:%s", parent.status, parent.durableHandoff, parent.durableOutputCount, parent.errorMessage)
	}
}

func TestWithoutResponseRunOwnershipPreservesExecutionContext(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	base, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	parent := newResponseRun("parent", "parent-session", "", "mock", time.Now().Unix(), nil)
	ctx := withResponseRunContext(base, parent)
	fence := session.ResponseRunFence{ResponseID: parent.id, OwnerInstanceID: "owner", FencingToken: 1}
	ctx = session.WithResponseRunFence(ctx, fence)
	ctx = llm.ContextWithApprovalTranscript(ctx, []llm.Message{llm.UserText("authorized task")})
	progressCalls := 0
	ctx = tools.ContextWithSubagentEventCallback(ctx, func(string, tools.SubagentEvent) { progressCalls++ })
	// A second isolation boundary models a nested child: both must preserve the
	// original cancellation and trusted values without recovering parent ownership.
	child := withoutResponseRunOwnership(ctx)
	nested := withoutResponseRunOwnership(child)
	for _, isolated := range []context.Context{child, nested} {
		if responseRunFromContext(isolated) != nil {
			t.Fatal("inherited parent response run")
		}
		if _, ok := session.ResponseRunFenceFromContext(isolated); ok {
			t.Fatal("inherited parent persistence fence")
		}
		if got, ok := isolated.Deadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("deadline = %v/%v", got, ok)
		}
		transcript := llm.ApprovalTranscriptFromContext(isolated)
		if len(transcript) != 1 || transcript[0].Parts[0].Text != "authorized task" {
			t.Fatalf("approval context = %+v", transcript)
		}
		callback := tools.SubagentEventCallbackFromContext(isolated)
		if callback == nil {
			t.Fatal("lost progress callback")
		}
		callback("child-call", tools.SubagentEvent{})
		msg := tagResponseRunMessage(isolated, llm.AssistantText("child"), 0)
		if msg.ResponseID != "" {
			t.Fatalf("child response identity = %q", msg.ResponseID)
		}
		// Even a failed child write containing parent-tagged input must never poison
		// the parent's durable handoff or be fenced by its ownership.
		writeErr := errors.New("child write failed")
		_, err := runResponseRunPersistence(isolated, []llm.Message{tagResponseRunMessage(ctx, llm.AssistantText("parent-tagged history"), 0)}, func(got session.ResponseRunFence) (int64, error) {
			if got.ResponseID != "" {
				t.Fatalf("child write fence = %+v", got)
			}
			return 0, writeErr
		})
		if !errors.Is(err, writeErr) {
			t.Fatalf("child write error = %v", err)
		}
	}
	if progressCalls != 2 {
		t.Fatalf("progress calls = %d", progressCalls)
	}
	if got, ok := session.ResponseRunFenceFromContext(ctx); !ok || got != fence {
		t.Fatalf("parent fence changed: %+v", got)
	}
	if handoff := parent.readDurableHandoff(); !handoff.Valid || handoff.OutputCount != 0 {
		t.Fatalf("child error contaminated parent: %+v", handoff)
	}
	cancel()
	for _, isolated := range []context.Context{child, nested} {
		select {
		case <-isolated.Done():
		default:
			t.Fatal("parent cancellation did not reach child")
		}
		if !errors.Is(isolated.Err(), context.Canceled) {
			t.Fatalf("child cancellation = %v", isolated.Err())
		}
	}
}
