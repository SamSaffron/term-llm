package sidequestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/runboundary"
)

// forkableProvider stands in for a CLI provider that can branch from a captured
// boundary. It records what it was asked to fork from and what it refused.
type forkableProvider struct {
	*llm.MockProvider
	state        []byte
	forkRequest  []llm.Message
	forks        int
	refuse       bool
	exportState  []byte
	cleanupCalls int
	branch       *forkableProvider
}

func newForkableProvider(name string, responses ...string) *forkableProvider {
	mock := llm.NewMockProvider(name)
	for _, response := range responses {
		mock = mock.AddTextResponse(response)
	}
	return &forkableProvider{MockProvider: mock, exportState: []byte(`{"session_id":"S2","messages_sent":1}`)}
}

// ForkConversationAtBoundary mirrors the claude-bin contract closely enough for
// the lane's assertions to mean something: it refuses whenever the captured
// offset does not fit inside the request it was handed.
func (p *forkableProvider) ForkConversationAtBoundary(state []byte, requestMessages []llm.Message) (llm.Provider, bool) {
	p.forks++
	if p.refuse || len(state) == 0 || len(requestMessages) == 0 {
		return nil, false
	}
	var decoded struct {
		SessionID        string `json:"session_id"`
		MessagesSent     int    `json:"messages_sent"`
		TranscriptDigest string `json:"transcript_digest"`
	}
	if err := json.Unmarshal(state, &decoded); err != nil {
		return nil, false
	}
	if decoded.SessionID == "" || decoded.TranscriptDigest == "" ||
		decoded.MessagesSent <= 0 || decoded.MessagesSent > len(requestMessages) {
		return nil, false
	}
	// The real seam refuses when the delivered prefix no longer hashes to the
	// captured digest. Model that, so a rewritten anchor falls back to replay here
	// too instead of silently branching.
	if testTranscriptDigest(requestMessages[:decoded.MessagesSent]) != decoded.TranscriptDigest {
		return nil, false
	}
	p.state = append([]byte(nil), state...)
	p.forkRequest = append([]llm.Message(nil), requestMessages...)
	branch := newForkableProvider(p.MockProvider.Name()+"-branch", "forked answer", "second forked answer")
	p.branch = branch
	return branch, true
}

func (p *forkableProvider) ExportProviderState() ([]byte, bool) {
	if len(p.exportState) == 0 {
		return nil, false
	}
	return p.exportState, true
}

func (p *forkableProvider) CleanupMCP() { p.cleanupCalls++ }

// testTranscriptDigest stands in for claude-bin's transcript fingerprint. Only
// its sensitivity to the delivered prefix matters here.
func testTranscriptDigest(messages []llm.Message) string {
	hash := sha256.New()
	for _, message := range messages {
		fmt.Fprintf(hash, "%s\x01%s\x02", message.Role, llm.MessageText(message))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// claudeBinAnchor is a boundary whose captured state describes the first two
// messages of mainTranscript, the way a completed claude-bin turn would.
func claudeBinAnchor(messages []llm.Message) Anchor {
	const delivered = 2
	state := fmt.Sprintf(`{"session_id":"S1","messages_sent":%d,"transcript_digest":%q}`,
		delivered, testTranscriptDigest(mainTranscript()[:delivered]))
	return Anchor{
		Messages: messages,
		Provider: runboundary.ProviderContext{
			State:       []byte(state),
			WorkingDir:  "/workspace",
			ProviderKey: "claude-bin",
			Model:       "opus-max",
		},
		DurableAnchorID: 42,
		Durable:         true,
	}
}

func mainTranscript() []llm.Message {
	return []llm.Message{
		llm.SystemText("system prompt"),
		llm.UserText("Refactor the widget loader."),
		llm.AssistantText("Extracted loadWidget."),
	}
}

func laneTurnRequest(lane *forkableProvider, replay llm.Provider, question string) TurnRequest {
	return TurnRequest{
		Question:    question,
		Anchor:      claudeBinAnchor(mainTranscript()),
		ProviderKey: "claude-bin",
		Model:       "opus-max",
		AllowFork:   true,
		ForkSource:  lane,
		NewProvider: func(string, string) (llm.Provider, error) { return replay, nil },
	}
}

func TestLaneForksWhenEligibleAndDeliversTheWholeAnchor(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	turn, err := lane.PrepareTurn(laneTurnRequest(live, replay, "what changed?"))
	if err != nil {
		t.Fatal(err)
	}
	if turn.Mode() != TurnFork {
		t.Fatalf("mode = %v, want fork", turn.Mode())
	}
	request := turn.Request()
	if len(request.Messages) != len(mainTranscript())+2 {
		t.Fatalf("request carried %d messages, want the whole anchor plus policy and question", len(request.Messages))
	}
	if request.Messages[len(request.Messages)-2].Role != llm.RoleDeveloper {
		t.Fatalf("policy message missing: %#v", request.Messages)
	}
	if request.WorkingDir != "/workspace" {
		t.Fatalf("working dir = %q, want the anchor's project directory", request.WorkingDir)
	}

	result, err := turn.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "forked answer" {
		t.Fatalf("response = %q", result.Response)
	}
	if !lane.Continuing() {
		t.Fatal("lane did not keep its own provider session")
	}
	if string(lane.ProviderState()) == "" {
		t.Fatal("lane did not export the branch state promotion needs")
	}
	if len(live.RecordedRequests()) != 0 {
		t.Fatalf("live provider served the side question: %#v", live.RecordedRequests())
	}
	if len(replay.RecordedRequests()) != 0 {
		t.Fatal("an eligible fork still built a replay provider")
	}
	// The seam must receive the anchor's own state and the whole request it will
	// compute its delta against.
	if string(live.state) != string(claudeBinAnchor(nil).Provider.State) {
		t.Fatalf("forwarded state = %q", live.state)
	}
	if len(live.forkRequest) != len(request.Messages) {
		t.Fatalf("seam saw %d messages, request carried %d", len(live.forkRequest), len(request.Messages))
	}
}

func TestLaneForkTurnIsNotEphemeralAndReplayTurnIs(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	turn, err := lane.PrepareTurn(laneTurnRequest(live, replay, "first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	forkRequests := live.branch.RecordedRequests()
	if len(forkRequests) != 1 || forkRequests[0].Ephemeral {
		t.Fatalf("lane turn must not be ephemeral: %#v", forkRequests)
	}

	// An ineligible anchor must fall back to an isolated, ephemeral request.
	replayLane := &Lane{}
	request := laneTurnRequest(live, replay, "second")
	request.Anchor.Provider.State = nil
	replayTurn, err := replayLane.PrepareTurn(request)
	if err != nil {
		t.Fatal(err)
	}
	if replayTurn.Mode() != TurnReplay {
		t.Fatalf("mode = %v, want replay", replayTurn.Mode())
	}
	if _, err := replayTurn.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	replayRequests := replay.RecordedRequests()
	if len(replayRequests) != 1 || !replayRequests[0].Ephemeral {
		t.Fatalf("replay turn must be ephemeral: %#v", replayRequests)
	}
	if replayLane.Continuing() {
		t.Fatal("a replay turn must not establish a lane session")
	}
}

func TestLaneReplaysWhenIneligible(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*TurnRequest)
		refuses   bool
		wantForks int
	}{
		{"provider key differs", func(r *TurnRequest) { r.ProviderKey = "chatgpt" }, false, 0},
		{"model differs", func(r *TurnRequest) { r.Model = "sonnet" }, false, 0},
		{"no captured state", func(r *TurnRequest) { r.Anchor.Provider.State = nil }, false, 0},
		{"empty anchor", func(r *TurnRequest) { r.Anchor.Messages = nil }, false, 0},
		{"provider has no seam", func(r *TurnRequest) { r.ForkSource = llm.NewMockProvider("plain") }, false, 0},
		{"no fork source", func(r *TurnRequest) { r.ForkSource = nil }, false, 0},
		{"a run started between capture and preparation", func(r *TurnRequest) { r.AllowFork = false }, false, 0},
		{"captured offset is past the end of the request", func(r *TurnRequest) {
			r.Anchor.Provider.State = []byte(`{"session_id":"S1","messages_sent":99,"transcript_digest":"d"}`)
		}, false, 1},
		{"legacy state with no digest", func(r *TurnRequest) {
			r.Anchor.Provider.State = []byte(`{"session_id":"S1","messages_sent":2}`)
		}, false, 1},
		{"digest mismatch after a history rewrite", func(r *TurnRequest) {
			rewritten := append([]llm.Message(nil), r.Anchor.Messages...)
			rewritten[1] = llm.UserText("a different question was written into history")
			r.Anchor.Messages = rewritten
		}, false, 1},
		{"the seam refuses", nil, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live := newForkableProvider("live")
			live.refuse = tc.refuses
			replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
			request := laneTurnRequest(live, replay, "what changed?")
			if tc.mutate != nil {
				tc.mutate(&request)
			}
			lane := &Lane{}
			turn, err := lane.PrepareTurn(request)
			if err != nil {
				t.Fatal(err)
			}
			if turn.Mode() != TurnReplay {
				t.Fatalf("mode = %v, want replay", turn.Mode())
			}
			if _, err := turn.Run(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if len(replay.RecordedRequests()) != 1 {
				t.Fatalf("replay provider requests = %d", len(replay.RecordedRequests()))
			}
			if !replay.RecordedRequests()[0].Ephemeral {
				t.Fatal("replay request was not isolated")
			}
			if lane.Continuing() {
				t.Fatal("replay established a lane session")
			}
			// Ineligibility must be decided before the seam is consulted; only a
			// seam-level refusal should reach the provider at all.
			if live.forks != tc.wantForks {
				t.Fatalf("seam consulted %d times, want %d", live.forks, tc.wantForks)
			}
		})
	}
}

// A main run that starts between preparing a branch and launching it must
// downgrade that branch to the bounded replay path, not resume a session the
// main process is writing.
func TestLaneDowngradesAPreparedForkWhenARunStarts(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	runActive := false
	request := laneTurnRequest(live, replay, "what changed?")
	request.ForkStillAllowed = func() bool { return !runActive }
	turn, err := lane.PrepareTurn(request)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Mode() != TurnFork {
		t.Fatalf("mode = %v, want fork", turn.Mode())
	}

	runActive = true
	if _, err := turn.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if turn.Mode() != TurnReplay {
		t.Fatalf("mode = %v, want the stale branch downgraded to replay", turn.Mode())
	}
	if len(live.branch.RecordedRequests()) != 0 {
		t.Fatal("the stale branch still streamed")
	}
	requests := replay.RecordedRequests()
	if len(requests) != 1 || !requests[0].Ephemeral {
		t.Fatalf("downgraded turn = %#v, want one isolated replay request", requests)
	}
	if lane.Continuing() {
		t.Fatal("a downgraded turn left a lane session behind")
	}
	if live.branch.cleanupCalls != 1 {
		t.Fatalf("branch cleanup calls = %d, want the discarded branch released once", live.branch.cleanupCalls)
	}
}

// A downgrade that cannot build its replacement must refuse rather than stream
// from a branch the gate forbids.
func TestLaneDowngradeFailsClosedWhenReplayCannotBeBuilt(t *testing.T) {
	live := newForkableProvider("live")
	lane := &Lane{}

	provided := 0
	request := laneTurnRequest(live, nil, "what changed?")
	request.NewProvider = func(string, string) (llm.Provider, error) {
		provided++
		if provided > 1 {
			return nil, errors.New("no provider available")
		}
		return llm.NewMockProvider("replay").AddTextResponse("replayed answer"), nil
	}
	runActive := false
	request.ForkStillAllowed = func() bool { return !runActive }

	// Consume the one good provider so the downgrade's own attempt fails.
	if _, err := request.NewProvider("", ""); err != nil {
		t.Fatal(err)
	}
	turn, err := lane.PrepareTurn(request)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Mode() != TurnFork {
		t.Fatalf("mode = %v, want fork", turn.Mode())
	}

	runActive = true
	if _, err := turn.Run(context.Background(), nil); err == nil {
		t.Fatal("expected the turn to refuse rather than stream from the branch")
	}
	if len(live.branch.RecordedRequests()) != 0 {
		t.Fatal("the forbidden branch streamed anyway")
	}
	if lane.Continuing() {
		t.Fatal("a refused turn left a lane session behind")
	}
	if live.branch.cleanupCalls != 1 {
		t.Fatalf("branch cleanup calls = %d, want the discarded branch released once", live.branch.cleanupCalls)
	}
}

// Tearing the lane down between preparing a turn and running it must release the
// prepared provider: there is no completion left to do it.
func TestLaneCloseReleasesATurnThatNeverRan(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	turn, err := lane.PrepareTurn(laneTurnRequest(live, replay, "what changed?"))
	if err != nil {
		t.Fatal(err)
	}
	lane.Close()
	if live.branch.cleanupCalls != 1 {
		t.Fatalf("branch cleanup calls = %d, want the prepared branch released exactly once", live.branch.cleanupCalls)
	}

	// Running afterwards must not stream on a provider that was already released.
	if _, err := turn.Run(context.Background(), nil); err == nil {
		t.Fatal("expected a cancelled turn to refuse to run")
	}
	if len(live.branch.RecordedRequests()) != 0 {
		t.Fatal("a released branch still streamed")
	}
	if live.branch.cleanupCalls != 1 {
		t.Fatalf("branch cleanup calls = %d, want no double release", live.branch.cleanupCalls)
	}
}

// An established lane must refuse to resume its session under a different
// provider or model, not only a different project directory.
func TestLaneDoesNotResumeAcrossAnIdentityChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*TurnRequest)
	}{
		{"model", func(r *TurnRequest) { r.Model = "sonnet" }},
		{"provider", func(r *TurnRequest) { r.ProviderKey = "chatgpt" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := newForkableProvider("live")
			replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
			lane := &Lane{}

			first, err := lane.PrepareTurn(laneTurnRequest(live, replay, "first"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.Run(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if !lane.Continuing() {
				t.Fatal("lane was not established")
			}

			swapped := laneTurnRequest(live, replay, "second")
			tc.mutate(&swapped)
			second, err := lane.PrepareTurn(swapped)
			if err != nil {
				t.Fatal(err)
			}
			if second.Mode() == TurnResume {
				t.Fatalf("lane resumed its session under a different %s", tc.name)
			}
			second.Abandon()
		})
	}
}

// A refused or empty answer leaves the branched session in an unverified place,
// so the lane drops it just as it does for an outright error.
func TestLaneDropsItsSessionAfterAnUnusableAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"empty answer", "   "},
		{"tool attempt", ToolAttemptResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := newForkableProvider("live")
			replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
			lane := &Lane{}

			turn, err := lane.PrepareTurn(laneTurnRequest(live, replay, "what changed?"))
			if err != nil {
				t.Fatal(err)
			}
			live.branch.ResetTurns()
			if tc.name == "tool attempt" {
				live.branch.MockProvider = live.branch.MockProvider.AddToolCall("call-1", "danger", map[string]any{})
			} else {
				live.branch.MockProvider = live.branch.MockProvider.AddTextResponse(tc.answer)
			}
			if _, err := turn.Run(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if lane.Continuing() {
				t.Fatalf("%s left an unverified session in place", tc.name)
			}
			if live.branch.cleanupCalls != 1 {
				t.Fatalf("branch cleanup calls = %d, want the dropped branch released once", live.branch.cleanupCalls)
			}
		})
	}
}

// Claude Code keys sessions by project directory, so a lane whose working
// directory moved cannot resume its own session from the new one.
func TestLaneDoesNotResumeAcrossAWorkingDirectoryChange(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	first, err := lane.PrepareTurn(laneTurnRequest(live, replay, "first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	second, err := lane.PrepareTurn(laneTurnRequest(live, replay, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(lane.Usage()) != 2 {
		t.Fatalf("exchanges = %d, want an anchored lane", len(lane.Usage()))
	}

	moved := laneTurnRequest(live, replay, "third")
	moved.Anchor.Provider.WorkingDir = "/another/worktree"
	third, err := lane.PrepareTurn(moved)
	if err != nil {
		t.Fatal(err)
	}
	if third.Mode() == TurnResume {
		t.Fatal("lane resumed its session from a different project directory")
	}
	if got := third.Request().WorkingDir; got != "/another/worktree" {
		t.Fatalf("working dir = %q, want the new project directory", got)
	}

	branchCleanups := live.branch.cleanupCalls
	third.Abandon()
	if lane.Continuing() {
		t.Fatal("abandoning a prepared branch left a lane session behind")
	}
	if live.branch.cleanupCalls != branchCleanups+1 {
		t.Fatalf("branch cleanup calls = %d, want the abandoned branch released exactly once", live.branch.cleanupCalls)
	}
	// The lane stays usable after an abandoned turn.
	if _, err := lane.PrepareTurn(moved); err != nil {
		t.Fatalf("lane refused a turn after abandon: %v", err)
	}
}

func TestLaneSecondTurnResumesItsOwnSessionInsteadOfReForking(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	first, err := lane.PrepareTurn(laneTurnRequest(live, replay, "what changed?"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	branch := live.branch

	second, err := lane.PrepareTurn(laneTurnRequest(live, replay, "and the nil case?"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Mode() != TurnResume {
		t.Fatalf("mode = %v, want resume", second.Mode())
	}
	if live.forks != 1 {
		t.Fatalf("forks = %d, want the lane to be created exactly once", live.forks)
	}
	if _, err := second.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	requests := branch.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("branch requests = %d, want both lane turns on the same provider", len(requests))
	}
	// The second request must keep the first request as its prefix so the
	// provider's recorded offset still lines up and only the new question crosses
	// the wire.
	if len(requests[1].Messages) != len(requests[0].Messages)+2 {
		t.Fatalf("second request = %d messages, want the first plus reply plus question", len(requests[1].Messages))
	}
	for i := range requests[0].Messages {
		if llm.MessageText(requests[1].Messages[i]) != llm.MessageText(requests[0].Messages[i]) {
			t.Fatalf("lane request prefix changed at %d", i)
		}
	}
	if requests[1].Messages[len(requests[0].Messages)].Role != llm.RoleAssistant {
		t.Fatalf("lane transcript lost its own reply: %#v", requests[1].Messages)
	}
	if got := llm.MessageText(requests[1].Messages[len(requests[1].Messages)-1]); got != "and the nil case?" {
		t.Fatalf("last message = %q", got)
	}
	if len(lane.RequestPrefix()) != len(requests[1].Messages)+1 {
		t.Fatalf("lane request prefix = %d messages, want the second request plus its reply", len(lane.RequestPrefix()))
	}
}

// An established lane is a branch: it pins the boundary it branched at instead
// of re-reading the main conversation. That is the only lossless choice — its
// prior exchanges live in its own session, and a re-anchored branch would be a
// fresh session that has never seen them.
func TestLaneWithItsOwnSessionPinsItsAnchor(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	first, err := lane.PrepareTurn(laneTurnRequest(live, replay, "first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	anchored := lane.Anchor()

	// The main conversation moved on.
	advanced := laneTurnRequest(live, replay, "second")
	advanced.Anchor.Messages = append(mainTranscript(), llm.UserText("Now handle the nil case."))
	second, err := lane.PrepareTurn(advanced)
	if err != nil {
		t.Fatal(err)
	}
	if second.Mode() != TurnResume {
		t.Fatalf("mode = %v, want the anchored lane to continue its own session", second.Mode())
	}
	if _, err := second.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if live.forks != 1 {
		t.Fatalf("forks = %d, want the lane branched exactly once", live.forks)
	}
	if len(lane.Anchor().Messages) != len(anchored.Messages) {
		t.Fatalf("lane anchor = %d messages, want the pinned boundary of %d",
			len(lane.Anchor().Messages), len(anchored.Messages))
	}
}

// A branch is a session that has never seen the panel's earlier exchanges, and
// re-delivering them would mean flattening real answers into text. When there is
// side history to carry, the bounded replay path carries it instead.
func TestLaneReplaysRatherThanBranchingAwayFromPriorSideHistory(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	request := laneTurnRequest(live, replay, "follow up")
	request.History = []Entry{{Question: "earlier side question", Response: "earlier side answer"}}
	turn, err := lane.PrepareTurn(request)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Mode() != TurnReplay {
		t.Fatalf("mode = %v, want replay so the earlier exchange survives", turn.Mode())
	}
	if live.forks != 0 {
		t.Fatalf("forks = %d, want no branch while side history has to be carried", live.forks)
	}
	if _, err := turn.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	joined := messageText(replay.RecordedRequests()[0].Messages)
	for _, want := range []string{"earlier side question", "earlier side answer", "follow up"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("replay request lost %q: %q", want, joined)
		}
	}
}

// Promotion writes the lane's own conversation into a child session created at
// its anchor. Including the main transcript it branched from would duplicate
// what the child already inherits.
func TestLaneTranscriptHoldsOnlyItsOwnConversation(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	first, err := lane.PrepareTurn(laneTurnRequest(live, replay, "what changed?"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	second, err := lane.PrepareTurn(laneTurnRequest(live, replay, "and the nil case?"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	transcript := lane.Transcript()
	wantRoles := []llm.Role{
		llm.RoleDeveloper, llm.RoleUser, llm.RoleAssistant,
		llm.RoleUser, llm.RoleAssistant,
	}
	if len(transcript) != len(wantRoles) {
		t.Fatalf("lane transcript = %d messages, want policy plus two exchanges: %#v", len(transcript), transcript)
	}
	for i, want := range wantRoles {
		if transcript[i].Role != want {
			t.Fatalf("transcript[%d] role = %s, want %s", i, transcript[i].Role, want)
		}
	}
	if joined := messageText(transcript); strings.Contains(joined, "Refactor the widget loader.") {
		t.Fatalf("lane transcript duplicated the main conversation: %q", joined)
	}
	if len(lane.Usage()) != 2 {
		t.Fatalf("lane usage = %d entries, want one per exchange", len(lane.Usage()))
	}
	// The anchor a promoted branch would be created at travels with the lane.
	if anchor := lane.Anchor(); anchor.DurableAnchorID != 42 || !anchor.Durable {
		t.Fatalf("lane anchor branch point = %#v", anchor)
	}
}

func TestLaneDropsItsSessionAfterAFailedTurn(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	turn, err := lane.PrepareTurn(laneTurnRequest(live, replay, "what changed?"))
	if err != nil {
		t.Fatal(err)
	}
	// The branch has no scripted turn left after its two responses are consumed;
	// force a failure by cancelling instead.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := turn.Run(ctx, nil); err == nil {
		t.Fatal("expected the cancelled turn to fail")
	}
	if lane.Continuing() {
		t.Fatal("a failed lane turn left an unverified session in place")
	}
	if live.branch.cleanupCalls == 0 {
		t.Fatal("the abandoned branch was not released")
	}
}

func TestLaneCloseReleasesTheBranchAndAnchor(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	turn, err := lane.PrepareTurn(laneTurnRequest(live, replay, "what changed?"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	lane.Close()
	if lane.Continuing() || len(lane.Transcript()) != 0 || len(lane.Anchor().Messages) != 0 {
		t.Fatal("close did not tear the lane down")
	}
	if live.branch.cleanupCalls != 1 {
		t.Fatalf("branch cleanup calls = %d, want exactly one", live.branch.cleanupCalls)
	}
	if len(lane.ProviderState()) != 0 {
		t.Fatal("close kept the branch state")
	}
}

func TestLaneRejectsAConcurrentTurn(t *testing.T) {
	live := newForkableProvider("live")
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}

	if _, err := lane.PrepareTurn(laneTurnRequest(live, replay, "first")); err != nil {
		t.Fatal(err)
	}
	_, err := lane.PrepareTurn(laneTurnRequest(live, replay, "second"))
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second prepare error = %v, want an already-running refusal", err)
	}
}

func TestLaneReplayTurnIsBoundedByTheHelperBudget(t *testing.T) {
	replay := llm.NewMockProvider("replay").AddTextResponse("replayed answer")
	lane := &Lane{}
	anchor := claudeBinAnchor(nil)
	anchor.Provider.State = nil
	for range 400 {
		anchor.Messages = append(anchor.Messages,
			llm.UserText(strings.Repeat("main question detail ", 400)),
			llm.AssistantText(strings.Repeat("main answer detail ", 400)))
	}
	turn, err := lane.PrepareTurn(TurnRequest{
		Question:    "what changed?",
		Anchor:      anchor,
		ProviderKey: "claude-bin",
		Model:       "opus-max",
		AllowFork:   true,
		NewProvider: func(string, string) (llm.Provider, error) { return replay, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := int(float64(llm.HelperInputLimitForProviderModel("claude-bin", "opus-max")) * sideInputBudgetRatio)
	tokens := estimateSideMessageTokens(turn.Request().Messages)
	if tokens > budget {
		t.Fatalf("replay request = %d tokens, want within the %d-token helper budget", tokens, budget)
	}
	if len(turn.Request().Messages) >= len(anchor.Messages) {
		t.Fatal("an oversized replay request was not trimmed")
	}
	// If the helper budget regressed to the unknown-model fallback, the request
	// would still be "bounded" — just far too small to be useful.
	if fallback := int(fallbackSideInputLimit * sideInputBudgetRatio); tokens <= fallback {
		t.Fatalf("replay request = %d tokens, want more than the %d-token unknown-model fallback allows", tokens, fallback)
	}
}

func TestLaneRefusesWithoutAProviderFactory(t *testing.T) {
	lane := &Lane{}
	if _, err := lane.PrepareTurn(TurnRequest{Question: "q", Anchor: claudeBinAnchor(mainTranscript())}); err == nil {
		t.Fatal("expected a refusal without a provider factory")
	}
	if _, err := lane.PrepareTurn(TurnRequest{
		Anchor:      claudeBinAnchor(mainTranscript()),
		NewProvider: func(string, string) (llm.Provider, error) { return llm.NewMockProvider("m"), nil },
	}); err == nil {
		t.Fatal("expected a refusal for an empty question")
	}
}
