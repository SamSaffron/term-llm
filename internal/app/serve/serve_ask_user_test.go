package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestAwaitAskUserPausesResponseTimeoutWhileWaiting(t *testing.T) {
	clock := newFakeResponseRunClock()
	runCtx, runTimer := newResponseRunTimerWithClock(500*time.Millisecond, clock)
	defer runTimer.stop()
	rt := &serveRuntime{pauseResponseTimeout: runTimer.pause}
	callCtx := core.ContextWithCallID(runCtx, "call-ask")
	questions := []tools.AskUserQuestion{{
		Header:   "Choice",
		Question: "Pick one",
		Options:  []tools.AskUserOption{{Label: "A", Description: "Option A"}},
	}}

	done := make(chan struct{})
	var answers []tools.AskUserAnswer
	var awaitErr error
	go func() {
		answers, awaitErr = rt.awaitAskUser(callCtx, questions)
		close(done)
	}()
	waitForServeCondition(t, time.Second, func() bool {
		return len(rt.pendingAskUserPrompts()) == 1
	}, "pending ask_user prompt")

	clock.Advance(600 * time.Millisecond)
	if err := runCtx.Err(); err != nil {
		t.Fatalf("response timeout elapsed during ask_user wait: %v", err)
	}
	if _, err := rt.submitAskUser("call-ask", []tools.AskUserAnswer{{
		QuestionIndex: 0,
		Header:        "Choice",
		Selected:      "A",
	}}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ask_user did not complete")
	}
	if awaitErr != nil || len(answers) != 1 || answers[0].Selected != "A" {
		t.Fatalf("ask_user result = %#v, %v", answers, awaitErr)
	}
}

func TestResponseRunTimeoutCauseUsesConfiguredDeadlineMessage(t *testing.T) {
	timeout := 45 * time.Minute
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errResponseRunTimeout)
	if got := responseRunDeadlineMessage(ctx, timeout); got != responseRunTimeoutMessage(timeout) {
		t.Fatalf("timeout cause message = %q, want %q", got, responseRunTimeoutMessage(timeout))
	}
}

func TestSessionStateOmitsResolvedAskUserBeforeWorkerCleanup(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "answered"
		if cancelled {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			rt := &serveRuntime{}
			questions := []tools.AskUserQuestion{{
				Header: "Color", Question: "Choose a color?",
				Options: []tools.AskUserOption{{Label: "Red"}, {Label: "Blue"}},
			}}
			pending, _ := rt.prepareAskUser("ask-1", questions)
			aliasRT := &serveRuntime{}
			aliasRT.aliasAskUser("child:ask-1", pending, serveAskUserOrigin{ChildSessionID: "child"})
			srv := &serveServer{sessionMgr: &serveSessionManager{
				sessions: map[string]*serveRuntime{"s1": rt},
			}}
			body := `{"call_id":"ask-1","answers":[{"question_index":0,"selected":"Red"}]}`
			if cancelled {
				body = `{"call_id":"ask-1","cancelled":true}`
			}
			rr := httptest.NewRecorder()
			srv.handleSessionAskUser(rr, httptest.NewRequest(http.MethodPost, "/v1/sessions/s1/ask_user", strings.NewReader(body)), "s1")
			if rr.Code != http.StatusOK {
				t.Fatalf("submit = %d %s", rr.Code, rr.Body.String())
			}
			// The worker has not consumed its answer or removed either alias yet.
			// Returning to the session must not turn these retained gates into forms.
			for _, runtime := range []*serveRuntime{rt, aliasRT} {
				state := map[string]any{}
				srv.addSessionStateRuntimePrompts(state, runtime)
				if _, ok := state["pending_ask_user"]; ok {
					encoded, _ := json.Marshal(state)
					t.Errorf("resolved question restored as pending: %s", encoded)
				}
				if _, ok := state["pending_ask_users"]; ok {
					t.Error("resolved question remained in pending_ask_users")
				}
			}
			submission := <-pending.gate.responseC
			if !cancelled && (len(submission.Answers) != 1 || submission.Answers[0].Selected != "Red") {
				t.Fatalf("worker lost accepted answer: %#v", submission)
			}
		})
	}
}

func TestPendingAskUserPromptsKeepsUnansweredQuestions(t *testing.T) {
	rt := &serveRuntime{}
	questions := []tools.AskUserQuestion{{Question: "Choose?", Options: []tools.AskUserOption{{Label: "Red"}}}}
	older, _ := rt.prepareAskUser("older", questions)
	resolved, _ := rt.prepareAskUser("answered", questions)
	newer, _ := rt.prepareAskUserFrom("newer", questions, serveAskUserOrigin{ChildSessionID: "child"})
	older.CreatedAt = time.UnixMilli(1)
	resolved.CreatedAt = time.UnixMilli(2)
	newer.CreatedAt = time.UnixMilli(3)
	if _, err := rt.submitAskUser("answered", []tools.AskUserAnswer{{QuestionIndex: 0, Selected: "Red"}}, false); err != nil {
		t.Fatal(err)
	}
	prompts := rt.pendingAskUserPrompts()
	if len(prompts) != 2 || prompts[0].CallID != "older" || prompts[1].CallID != "newer" {
		t.Fatalf("pending prompts = %#v, want older then newer", prompts)
	}
	if prompts[1].Origin == nil || prompts[1].Origin.ChildSessionID != "child" {
		t.Fatalf("lost delegated origin: %#v", prompts[1])
	}
	prompts[0].Questions[0].Options[0].Label = "Changed"
	if got := older.snapshot().Questions[0].Options[0].Label; got != "Red" {
		t.Fatalf("snapshot mutated pending question: %q", got)
	}
	if rt.pendingAskUsers["answered"] != resolved {
		t.Fatal("snapshot removed the gate before its worker could consume the answer")
	}
}
