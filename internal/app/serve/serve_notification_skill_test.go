package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestNotificationMainSkillHTTPAdmissionOwnsCompletion(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	provider := mockprovider.NewMockProvider("debug").AddTextResponse("Synthetic main skill completion")
	vector := notificationNativeIdentityVectors(t)[1]
	s, store, rt, origin := newNotificationNativeFixture(t, provider, vector.WindowID)
	s.skillsSetup = setup
	s.cfgRef.Skills.Enabled = true
	s.cfgRef.Skills.IncludeProjectSkills = true
	rt.sessionMeta.CWD = root
	if err := store.Update(context.Background(), rt.sessionMeta); err != nil {
		t.Fatal(err)
	}
	if w := postNotificationNativePresence(t, s, origin); w.Code != http.StatusOK {
		t.Fatalf("presence=%d %s", w.Code, w.Body.String())
	}
	r := notificationNativeRequest(origin, "/v1/sessions/wake-parent/skills/invoke", `{"name":"default","arguments":"synthetic scope","client_message_id":"native-main-skill-1"}`)
	w := httptest.NewRecorder()
	s.handleSessionByID(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("main skill admission=%d %s", w.Code, w.Body.String())
	}
	var accepted struct {
		Execution       string `json:"execution"`
		ResponseID      string `json:"response_id"`
		RunEpoch        int64  `json:"run_epoch"`
		ClientMessageID string `json:"client_message_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Execution != "main" || accepted.ResponseID == "" || accepted.RunEpoch <= 0 || accepted.ClientMessageID != "native-main-skill-1" {
		t.Fatalf("main skill acceptance lost managed identity: %+v", accepted)
	}
	run := waitNotificationNativeRun(t, s, accepted.ResponseID)
	if epoch := run.snapshot()["run_epoch"]; epoch != accepted.RunEpoch {
		t.Fatalf("accepted epoch=%d completed epoch=%v", accepted.RunEpoch, epoch)
	}
	assertNotificationNativeCandidate(t, s, store, origin, run)
	requests := provider.RecordedRequests()
	if len(requests) != 1 {
		t.Fatalf("main skill provider requests=%d, want 1", len(requests))
	}
	// Verify this was the real skill path, not an ordinary human-response
	// fixture that happened to produce a completion with the same owner.
	messages, err := store.GetMessages(context.Background(), "wake-parent", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		for _, part := range message.Parts {
			if p := part.SkillActivation; p != nil && p.Name == "default" && p.Execution == "main" && p.Origin == "user" && p.RawArguments == "synthetic scope" {
				return
			}
		}
	}
	t.Fatal("accepted main skill did not persist its human skill activation")
}

func TestNotificationIsolatedSkillHTTPAdmissionHasNoAutomaticCandidate(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	provider := mockprovider.NewMockProvider("debug")
	vector := notificationNativeIdentityVectors(t)[0]
	s, store, rt, origin := newNotificationNativeFixture(t, provider, vector.WindowID)
	s.skillsSetup = setup
	s.cfgRef.Skills.Enabled = true
	s.cfgRef.Skills.IncludeProjectSkills = true
	rt.sessionMeta.CWD = root
	if err := store.Update(context.Background(), rt.sessionMeta); err != nil {
		t.Fatal(err)
	}
	runner := &fakeServeSkillChildRunner{}
	s.skillChildRunnerFactory = func(_ string, _ *serveRuntime) (runpkg.ChildRunner, error) { return runner, nil }
	t.Cleanup(func() { s.stopServeSkillRuns(context.Background()) })
	if w := postNotificationNativePresence(t, s, origin); w.Code != http.StatusOK {
		t.Fatalf("presence=%d %s", w.Code, w.Body.String())
	}
	// Even a capability-negotiated request carrying the exact native owner
	// cannot grant an isolated background skill automatic OS authority.
	r := notificationNativeRequest(origin, "/v1/sessions/wake-parent/skills/invoke", `{"name":"forked","arguments":"synthetic scope","client_message_id":"native-isolated-skill-1"}`)
	w := httptest.NewRecorder()
	s.handleSessionByID(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("isolated skill admission=%d %s", w.Code, w.Body.String())
	}
	var accepted struct {
		Execution string `json:"execution"`
		RunID     string `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Execution != "isolated" || accepted.RunID == "" {
		t.Fatalf("isolated skill acceptance=%+v", accepted)
	}
	// Registration precedes the ACK, so waiting for the registered worker is
	// deterministic and includes result persistence, without timing sleeps.
	done := make(chan struct{})
	go func() { s.skillRunsWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("isolated skill worker did not finish")
	}
	request := runner.requestSnapshot()
	if runner.callCount() != 1 || request.Kind != runpkg.ChildRunIsolatedSkill || request.ParentSessionID != "wake-parent" || !strings.Contains(request.Prompt, "Review synthetic scope.") {
		t.Fatalf("isolated skill did not execute its real child path: %+v", request)
	}
	rows, err := store.ClaimCompletionNotifications(context.Background(), "native-contract", time.Now().Add(4*time.Second), time.Minute, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("isolated background skill gained automatic candidate: %+v err=%v", rows, err)
	}
	if provider.CurrentTurn() != 0 {
		t.Fatalf("isolated skill ran parent inference: %d", provider.CurrentTurn())
	}
}
