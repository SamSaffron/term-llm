package clibin

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// boundaryForkTranscript is a small main-run transcript. Indices 0..5 stand in
// for the prefix claude-bin already delivered to its session.
func boundaryForkTranscript() []core.Message {
	return []core.Message{
		core.SystemText("term-llm system prompt"),
		core.UserText("Refactor the widget loader."),
		core.AssistantText("Extracted loadWidget into widget_loader.go."),
		core.UserText("Now add a test."),
		core.AssistantText("Added TestLoadWidget."),
		core.UserText("Now handle the nil case."),
	}
}

func boundaryForkState(t *testing.T, sessionID string, messagesSent int, messages []core.Message) []byte {
	t.Helper()
	state, err := json.Marshal(claudeBinProviderState{
		SessionID:        sessionID,
		MessagesSent:     messagesSent,
		TranscriptDigest: claudeTranscriptDigest(messages, messagesSent),
	})
	if err != nil {
		t.Fatalf("marshal provider state: %v", err)
	}
	return state
}

func drainBoundaryForkStream(t *testing.T, stream core.Stream) {
	t.Helper()
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == core.EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestClaudeBinBoundaryForkResumesAndDeliversOnlyTheUndeliveredDelta(t *testing.T) {
	transcript := boundaryForkTranscript()
	const delivered = 4 // indices 0..3 already crossed stdin

	live := NewClaudeBinProvider("opus-max", nil)
	live.sessionID = "live-session"
	live.messagesSent = delivered
	live.transcriptDigest = claudeTranscriptDigest(transcript, delivered)

	request := append(append([]core.Message(nil), transcript...),
		core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "side question policy"}}},
		core.UserText("what file did the loader move to?"))

	forked, ok := forkHelperAtBoundary(live, boundaryForkState(t, "live-session", delivered, transcript), request)
	if !ok {
		t.Fatal("expected a boundary fork for matching captured state")
	}
	clone, ok := forked.(*ClaudeBinProvider)
	if !ok {
		t.Fatalf("forked provider = %T, want *ClaudeBinProvider", forked)
	}
	if clone == live {
		t.Fatal("boundary fork returned the live provider")
	}
	// --fork-session is what makes Claude Code answer on a session of its own
	// rather than appending to the parent's.
	if !clone.forkSession || clone.sessionID != live.sessionID || clone.messagesSent != delivered {
		t.Fatalf("clone = {session:%q sent:%d fork:%v}, want the captured boundary branched",
			clone.sessionID, clone.messagesSent, clone.forkSession)
	}

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	stdinFile := filepath.Join(dir, "stdin.txt")
	workingDir := t.TempDir()
	capturedWorkingDir := ""
	clone.commandRunner = func(_ context.Context, args []string, _, prompt, wd string, _ bool, send eventSender, _, _ bool) error {
		capturedWorkingDir = wd
		if err := os.WriteFile(argsFile, []byte(strings.Join(args, "\n")+"\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(stdinFile, []byte(prompt), 0o644); err != nil {
			return err
		}
		return send.Send(core.Event{Type: core.EventTextDelta, Text: "side answer"})
	}

	stream, err := clone.Stream(context.Background(), core.Request{
		Model:      "opus-max",
		Messages:   request,
		WorkingDir: workingDir,
		MaxTurns:   1,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainBoundaryForkStream(t, stream)

	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(argsBytes)
	if !strings.Contains(args, "--resume\nlive-session\n") {
		t.Fatalf("boundary fork did not resume the captured session:\n%s", args)
	}
	if !strings.Contains(args, "--fork-session\n") {
		t.Fatalf("boundary fork did not branch the session:\n%s", args)
	}
	if capturedWorkingDir != workingDir {
		t.Fatalf("working dir = %q, want %q", capturedWorkingDir, workingDir)
	}

	stdinBytes, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	stdin := string(stdinBytes)
	for _, delivered := range []string{"Refactor the widget loader.", "Now add a test.", "Extracted loadWidget"} {
		if strings.Contains(stdin, delivered) {
			t.Fatalf("stdin replayed content the resumed session already holds (%q):\n%s", delivered, stdin)
		}
	}
	for _, want := range []string{"Now handle the nil case.", "side question policy", "what file did the loader move to?"} {
		if !strings.Contains(stdin, want) {
			t.Fatalf("stdin missing undelivered content %q:\n%s", want, stdin)
		}
	}
	if strings.Contains(stdin, "Added TestLoadWidget.") {
		t.Fatalf("stdin re-delivered an assistant turn the session already holds:\n%s", stdin)
	}

	if live.sessionID != "live-session" || live.messagesSent != delivered ||
		live.transcriptDigest != claudeTranscriptDigest(transcript, delivered) {
		t.Fatalf("live provider mutated: session=%q sent=%d digest=%q",
			live.sessionID, live.messagesSent, live.transcriptDigest)
	}
	if clone.messagesSent != len(request) {
		t.Fatalf("clone messagesSent = %d, want %d", clone.messagesSent, len(request))
	}
}

// The branch owns the session Claude Code hands back to it. Keeping
// --fork-session would branch again on every later turn, leaving an orphan
// session per exchange and no accumulating session to promote.
func TestClaudeBinBranchStopsForkingOnceItOwnsASession(t *testing.T) {
	transcript := boundaryForkTranscript()
	const delivered = 4
	live := NewClaudeBinProvider("opus-max", nil)
	live.sessionID = "live-session"
	live.messagesSent = delivered
	live.transcriptDigest = claudeTranscriptDigest(transcript, delivered)

	request := append(append([]core.Message(nil), transcript...), core.UserText("first side question"))
	forked, ok := forkHelperAtBoundary(live, boundaryForkState(t, "live-session", delivered, transcript), request)
	if !ok {
		t.Fatal("expected a boundary fork")
	}
	clone := forked.(*ClaudeBinProvider)

	var turns [][]string
	// Claude Code reports the branched session id on the first turn.
	clone.commandRunner = func(_ context.Context, args []string, _, _, _ string, _ bool, send eventSender, _, _ bool) error {
		turns = append(turns, append([]string(nil), args...))
		clone.sessionID = "branched-session"
		return send.Send(core.Event{Type: core.EventTextDelta, Text: "answer"})
	}

	first, err := clone.Stream(context.Background(), core.Request{Model: "opus-max", Messages: request, MaxTurns: 1})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainBoundaryForkStream(t, first)

	followUp := append(append([]core.Message(nil), request...), core.AssistantText("answer"), core.UserText("second side question"))
	second, err := clone.Stream(context.Background(), core.Request{Model: "opus-max", Messages: followUp, MaxTurns: 1})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainBoundaryForkStream(t, second)

	if len(turns) != 2 {
		t.Fatalf("turns = %d", len(turns))
	}
	firstArgs, secondArgs := strings.Join(turns[0], "\n"), strings.Join(turns[1], "\n")
	if !strings.Contains(firstArgs, "--resume\nlive-session") || !strings.Contains(firstArgs, "--fork-session") {
		t.Fatalf("lane creation did not branch the parent session:\n%s", firstArgs)
	}
	if !strings.Contains(secondArgs, "--resume\nbranched-session") {
		t.Fatalf("follow-up did not resume the lane's own session:\n%s", secondArgs)
	}
	if strings.Contains(secondArgs, "--fork-session") {
		t.Fatalf("follow-up branched again instead of continuing the lane:\n%s", secondArgs)
	}
	if clone.forkSession {
		t.Fatal("branch still requests a fork after taking ownership of its session")
	}
}

func TestClaudeBinBoundaryForkRefusesUnverifiableState(t *testing.T) {
	transcript := boundaryForkTranscript()
	request := append(append([]core.Message(nil), transcript...), core.UserText("side question"))

	rewritten := append([]core.Message(nil), transcript...)
	rewritten[2] = core.AssistantText("a different answer was written into history")

	tests := []struct {
		name  string
		state []byte
	}{
		{"offset past the end of the request", boundaryForkState(t, "s", len(request)+1, request)},
		{"digest mismatch after a history rewrite", boundaryForkState(t, "s", 4, rewritten)},
		{"legacy state with no digest", mustJSON(t, claudeBinProviderState{SessionID: "s", MessagesSent: 4})},
		{"zero offset", boundaryForkState(t, "s", 0, transcript)},
		{"missing session id", mustJSON(t, claudeBinProviderState{MessagesSent: 4, TranscriptDigest: "x"})},
		{"corrupt state", []byte("{not json")},
		{"empty state", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live := NewClaudeBinProvider("opus-max", nil)
			live.sessionID = "live-session"
			live.messagesSent = 4
			live.transcriptDigest = claudeTranscriptDigest(transcript, 4)
			if forked, ok := forkHelperAtBoundary(live, tc.state, request); ok || forked != nil {
				t.Fatalf("forkHelperAtBoundary = (%v, %v), want refusal so the caller falls back to bounded replay", forked, ok)
			}
		})
	}
}

func TestClaudeBinBoundaryForkRefusesEmptyRequest(t *testing.T) {
	transcript := boundaryForkTranscript()
	live := NewClaudeBinProvider("opus-max", nil)
	live.sessionID = "live-session"
	if _, ok := forkHelperAtBoundary(live, boundaryForkState(t, "s", 4, transcript), nil); ok {
		t.Fatal("expected refusal for an empty request")
	}
}

// The two fork contracts must stay separate. Handover's suffix-only fork hands
// the clone a 2-3 message request; importing a parent offset there would fail
// resumeBoundaryValid, reset the clone, drop --resume, and send the trigger with
// no conversation history at all.
func TestHandoverSuffixForkStillIgnoresTheParentBoundaryOffset(t *testing.T) {
	transcript := boundaryForkTranscript()
	live := NewClaudeBinProvider("opus-max", nil)
	live.sessionID = "live-session"
	live.messagesSent = len(transcript)
	live.transcriptDigest = claudeTranscriptDigest(transcript, len(transcript))

	forked, ok := live.forkHelperConversation()
	if !ok {
		t.Fatal("expected a suffix-only handover fork")
	}
	clone, ok := forked.(*ClaudeBinProvider)
	if !ok {
		t.Fatalf("forked provider = %T, want *ClaudeBinProvider", forked)
	}
	if clone.messagesSent != 0 || clone.transcriptDigest != "" {
		t.Fatalf("suffix fork carried a parent offset: sent=%d digest=%q", clone.messagesSent, clone.transcriptDigest)
	}
	suffix := []core.Message{core.SystemText("system"), core.UserText("handover policy"), core.UserText("trigger")}
	if !clone.resumeBoundaryValid(suffix) {
		t.Fatal("suffix fork would reset its resumed session and lose all conversation history")
	}
	if clone.sessionID != "live-session" || !clone.forkSession {
		t.Fatalf("suffix fork = {session:%q fork:%v}, want the parent session branched", clone.sessionID, clone.forkSession)
	}
}

func TestForkHelperConversationUsesForkSessionWithoutMutatingLive(t *testing.T) {
	live := NewClaudeBinProvider("opus-max", nil)
	live.sessionID = "session-live"
	live.messagesSent = 4

	forked, ok := live.ForkHelperConversation()
	if !ok {
		t.Fatal("expected claude-bin conversation fork")
	}
	clone, ok := forked.(*ClaudeBinProvider)
	if !ok {
		t.Fatalf("fork provider = %T, want ClaudeBinProvider", forked)
	}
	args, _ := clone.buildArgs(context.Background(), core.Request{}, eventSender{})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume session-live") || !strings.Contains(joined, "--fork-session") {
		t.Fatalf("fork args = %q, want resume plus fork-session", joined)
	}
	if clone.messagesSent != 0 {
		t.Fatalf("fork messagesSent = %d, want helper-local boundary 0", clone.messagesSent)
	}
	if live.sessionID != "session-live" || live.messagesSent != 4 {
		t.Fatalf("live provider mutated: session=%q messages=%d", live.sessionID, live.messagesSent)
	}
}
