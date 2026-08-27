package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestAwaitAskUserPausesResponseTimeoutWhileWaiting(t *testing.T) {
	clock := newFakeResponseRunClock()
	runCtx, runTimer := newResponseRunTimerWithClock(500*time.Millisecond, clock)
	defer runTimer.stop()
	rt := &serveRuntime{pauseResponseTimeout: runTimer.pause}
	callCtx := llm.ContextWithCallID(runCtx, "call-ask")
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
