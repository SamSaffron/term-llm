package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

type childAskUserHarness struct {
	srv       *serveServer
	handle    *childRunHandle
	parentRT  *serveRuntime
	childRT   *serveRuntime
	parentRun *responseRun
	childRun  *responseRun
}

func newChildAskUserHarness(t *testing.T, childID string) *childAskUserHarness {
	t.Helper()
	parentRT := &serveRuntime{}
	childRT := &serveRuntime{}
	runs := newServeResponseRunManager()
	t.Cleanup(runs.Close)
	parentRun := newResponseRun("resp-parent-"+childID, "ask-parent", "", "mock", time.Now().Unix(), nil)
	childRun := newResponseRun("resp-child-"+childID, childID, "", "mock", time.Now().Unix(), nil)
	for _, run := range []*responseRun{parentRun, childRun} {
		if err := runs.create(run); err != nil {
			t.Fatal(err)
		}
		runs.setActiveRun(run.sessionID, run.id)
	}
	srv := &serveServer{
		responseRuns: runs,
		sessionMgr: &serveSessionManager{sessions: map[string]*serveRuntime{
			"ask-parent": parentRT,
			childID:      childRT,
		}},
	}
	handle := srv.ensureChildRuns().ChildRunStarted(childRunInfo{
		ChildSessionID: childID, ParentSessionID: "ask-parent", CallID: "spawn-" + childID,
		Agent: "researcher", Prompt: "investigate " + childID,
	}).(*childRunHandle)
	questions := json.RawMessage(`{"questions":[{"header":"Target","question":"Which schema?","options":[{"label":"v2","description":"new"},{"label":"v3","description":"newer"}]}]}`)
	if err := srv.appendResponseToolExecStart(childRT, childRun, newResponseRunStreamState("mock", ""), llm.Event{
		Type: llm.EventToolExecStart, ToolCallID: "call-ask", ToolName: tools.AskUserToolName, ToolArgs: questions,
	}); err != nil {
		t.Fatal(err)
	}
	return &childAskUserHarness{srv: srv, handle: handle, parentRT: parentRT, childRT: childRT, parentRun: parentRun, childRun: childRun}
}

type childAskUserOutcome struct {
	answers []tools.AskUserAnswer
	err     error
}

func (h *childAskUserHarness) start(t *testing.T, ctx context.Context) <-chan childAskUserOutcome {
	t.Helper()
	done := make(chan childAskUserOutcome, 1)
	go func() {
		answers, err := h.handle.AskUser(llm.ContextWithCallID(ctx, "call-ask"), []tools.AskUserQuestion{{
			Header: "Target", Question: "Which schema?",
			Options: []tools.AskUserOption{{Label: "v2", Description: "new"}, {Label: "v3", Description: "newer"}},
		}})
		done <- childAskUserOutcome{answers: answers, err: err}
	}()
	waitForServeCondition(t, time.Second, func() bool {
		qualified := h.handle.childSessionID + ":call-ask"
		parentVisible := false
		for _, prompt := range h.parentRT.pendingAskUserPrompts() {
			if prompt.CallID == qualified {
				parentVisible = true
				break
			}
		}
		return len(h.childRT.pendingAskUserPrompts()) == 1 && parentVisible &&
			h.parentRun.hasPendingInteraction("ask_user", qualified)
	}, "canonical ask_user prompt on child and parent")
	return done
}

func postChildAskUser(t *testing.T, srv *serveServer, sessionID, responseID, callID, selected string, cancelled bool) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"call_id": callID, "response_id": responseID}
	if cancelled {
		body["cancelled"] = true
	} else {
		body["answers"] = []map[string]any{{"question_index": 0, "header": "Target", "selected": selected}}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/ask_user", strings.NewReader(string(encoded)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionAskUser(rr, req, sessionID)
	return rr
}

func TestChildAskUserCanonicalGateAnswersFromEitherResponse(t *testing.T) {
	for _, surface := range []string{"child", "parent"} {
		t.Run(surface, func(t *testing.T) {
			h := newChildAskUserHarness(t, "ask-child-"+surface)
			done := h.start(t, context.Background())
			sessionID, responseID, callID := h.handle.childSessionID, h.childRun.id, "call-ask"
			if surface == "parent" {
				sessionID, responseID, callID = "ask-parent", h.parentRun.id, h.handle.childSessionID+":call-ask"
			}
			rr := postChildAskUser(t, h.srv, sessionID, responseID, callID, "v2", false)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s answer = %d %s", surface, rr.Code, rr.Body.String())
			}
			select {
			case outcome := <-done:
				if outcome.err != nil || len(outcome.answers) != 1 || outcome.answers[0].Selected != "v2" {
					t.Fatalf("worker outcome = %#v, %v", outcome.answers, outcome.err)
				}
			case <-time.After(time.Second):
				t.Fatal("worker remained blocked on a second gate")
			}
			if h.childRun.hasPendingInteraction("ask_user", "call-ask") || h.parentRun.hasPendingInteraction("ask_user", h.handle.childSessionID+":call-ask") {
				t.Fatal("resolved prompt remained in response recovery")
			}
			if resolved, ok := h.childRun.resolvedInteraction("ask_user", "call-ask"); !ok || resolved.Outcome != "answered" {
				t.Fatalf("child resolution = %#v, %v", resolved, ok)
			}
			if resolved, ok := h.parentRun.resolvedInteraction("ask_user", h.handle.childSessionID+":call-ask"); !ok || resolved.Outcome != "answered" {
				t.Fatalf("parent resolution = %#v, %v", resolved, ok)
			}
			// The other response-scoped surface must remain an idempotent retry
			// after the worker has removed both runtime aliases.
			retrySession, retryResponse, retryCall := "ask-parent", h.parentRun.id, h.handle.childSessionID+":call-ask"
			if surface == "parent" {
				retrySession, retryResponse, retryCall = h.handle.childSessionID, h.childRun.id, "call-ask"
			}
			retry := postChildAskUser(t, h.srv, retrySession, retryResponse, retryCall, "v2", false)
			if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"status":"already_resolved"`) {
				t.Fatalf("cross-surface retry = %d %s", retry.Code, retry.Body.String())
			}
		})
	}
}

func TestChildAskUserCanonicalGateEarlyAnswer(t *testing.T) {
	h := newChildAskUserHarness(t, "ask-child-early")
	questions := h.childRT.pendingAskUserPrompts()[0].Questions
	// A client can answer the streamed child prompt before the tool has
	// entered its wait or published the parent's alias.
	rr := postChildAskUser(t, h.srv, h.handle.childSessionID, h.childRun.id, "call-ask", "v2", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("early answer = %d %s", rr.Code, rr.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	answers, err := h.handle.AskUser(llm.ContextWithCallID(ctx, "call-ask"), questions)
	if err != nil || len(answers) != 1 || answers[0].Selected != "v2" {
		t.Fatalf("early answer was lost: answers=%v err=%v", answers, err)
	}
	qualified := h.handle.childSessionID + ":call-ask"
	if h.parentRun.hasPendingInteraction("ask_user", qualified) {
		t.Fatal("late parent alias revived an already answered prompt")
	}
	if resolved, ok := h.parentRun.resolvedInteraction("ask_user", qualified); !ok || resolved.Outcome != "answered" {
		t.Fatalf("parent resolution = %#v, %v", resolved, ok)
	}
}

func TestChildAskUserCanonicalGateConcurrentEndpoints(t *testing.T) {
	h := newChildAskUserHarness(t, "ask-child-race")
	done := h.start(t, context.Background())
	start := make(chan struct{})
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, request := range []struct{ sessionID, responseID, callID, answer string }{
		{h.handle.childSessionID, h.childRun.id, "call-ask", "v2"},
		{"ask-parent", h.parentRun.id, h.handle.childSessionID + ":call-ask", "v3"},
	} {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- postChildAskUser(t, h.srv, request.sessionID, request.responseID, request.callID, request.answer, false)
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	for rr := range responses {
		if rr.Code != http.StatusOK || (!strings.Contains(rr.Body.String(), `"status":"ok"`) && !strings.Contains(rr.Body.String(), `"status":"already_resolved"`)) {
			t.Fatalf("concurrent answer = %d %s", rr.Code, rr.Body.String())
		}
	}
	outcome := <-done
	if outcome.err != nil || len(outcome.answers) != 1 || (outcome.answers[0].Selected != "v2" && outcome.answers[0].Selected != "v3") {
		t.Fatalf("worker consumed invalid outcome = %#v, %v", outcome.answers, outcome.err)
	}
}

func TestChildAskUserCanonicalGateCancelAndCleanup(t *testing.T) {
	t.Run("dismiss", func(t *testing.T) {
		h := newChildAskUserHarness(t, "ask-child-dismiss")
		done := h.start(t, context.Background())
		rr := postChildAskUser(t, h.srv, "ask-parent", h.parentRun.id, h.handle.childSessionID+":call-ask", "", true)
		if rr.Code != http.StatusOK {
			t.Fatalf("dismiss = %d %s", rr.Code, rr.Body.String())
		}
		if outcome := <-done; !errors.Is(outcome.err, errServeAskUserCancelled) {
			t.Fatalf("dismiss error = %v", outcome.err)
		}
		for _, check := range []struct {
			run    *responseRun
			callID string
		}{{h.childRun, "call-ask"}, {h.parentRun, h.handle.childSessionID + ":call-ask"}} {
			if resolved, ok := check.run.resolvedInteraction("ask_user", check.callID); !ok || resolved.Outcome != "cancelled-by-user" {
				t.Fatalf("dismiss resolution for %s = %#v, %v", check.callID, resolved, ok)
			}
		}
	})

	t.Run("run cancellation", func(t *testing.T) {
		h := newChildAskUserHarness(t, "ask-child-cancel")
		ctx, cancel := context.WithCancel(context.Background())
		done := h.start(t, ctx)
		cancel()
		if outcome := <-done; !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("cancel error = %v", outcome.err)
		}
		waitForServeCondition(t, time.Second, func() bool {
			return len(h.childRT.pendingAskUserPrompts()) == 0 && len(h.parentRT.pendingAskUserPrompts()) == 0
		}, "cancelled ask_user aliases cleaned up")
		if h.parentRun.hasPendingInteraction("ask_user", h.handle.childSessionID+":call-ask") {
			t.Fatal("parent replay retained a cancelled child prompt")
		}
	})
}

func TestChildAskUserCanonicalGateSiblingIsolation(t *testing.T) {
	first := newChildAskUserHarness(t, "ask-sibling-a")
	// Reuse the same parent runtime/run so equal raw tool call IDs coexist on the
	// parent only through child-qualified aliases.
	secondChildRT := &serveRuntime{}
	first.srv.sessionMgr.sessions["ask-sibling-b"] = secondChildRT
	secondRun := newResponseRun("resp-child-ask-sibling-b", "ask-sibling-b", "", "mock", time.Now().Unix(), nil)
	if err := first.srv.responseRuns.create(secondRun); err != nil {
		t.Fatal(err)
	}
	first.srv.responseRuns.setActiveRun("ask-sibling-b", secondRun.id)
	secondHandle := first.srv.ensureChildRuns().ChildRunStarted(childRunInfo{ChildSessionID: "ask-sibling-b", ParentSessionID: "ask-parent", CallID: "spawn-b"}).(*childRunHandle)
	args := json.RawMessage(`{"questions":[{"header":"Target","question":"Which schema?","options":[{"label":"v2","description":"new"}]}]}`)
	if err := first.srv.appendResponseToolExecStart(secondChildRT, secondRun, newResponseRunStreamState("mock", ""), llm.Event{Type: llm.EventToolExecStart, ToolCallID: "call-ask", ToolName: tools.AskUserToolName, ToolArgs: args}); err != nil {
		t.Fatal(err)
	}
	second := &childAskUserHarness{srv: first.srv, handle: secondHandle, parentRT: first.parentRT, childRT: secondChildRT, parentRun: first.parentRun, childRun: secondRun}
	firstDone := first.start(t, context.Background())
	secondDone := second.start(t, context.Background())
	if got := len(first.parentRT.pendingAskUserPrompts()); got != 2 {
		t.Fatalf("parent prompts = %d, want isolated sibling prompts", got)
	}
	if rr := postChildAskUser(t, first.srv, "ask-parent", first.parentRun.id, first.handle.childSessionID+":call-ask", "v2", false); rr.Code != http.StatusOK {
		t.Fatalf("first answer = %d %s", rr.Code, rr.Body.String())
	}
	if outcome := <-firstDone; outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if !secondRun.hasPendingInteraction("ask_user", "call-ask") {
		t.Fatal("answer resolved the sibling's response")
	}
	select {
	case outcome := <-secondDone:
		t.Fatalf("answer leaked to sibling: %#v", outcome)
	default:
	}
	if rr := postChildAskUser(t, first.srv, "ask-parent", first.parentRun.id, second.handle.childSessionID+":call-ask", "v2", false); rr.Code != http.StatusOK {
		t.Fatalf("second answer = %d %s", rr.Code, rr.Body.String())
	}
	if outcome := <-secondDone; outcome.err != nil {
		t.Fatal(outcome.err)
	}
}
