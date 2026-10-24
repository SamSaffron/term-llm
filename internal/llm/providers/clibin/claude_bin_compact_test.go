package clibin

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func resumedClaudeProvider() *ClaudeBinProvider {
	p := NewClaudeBinProvider("haiku", nil)
	p.sessionID = "sess-1"
	p.messagesSent = 3
	p.transcriptDigest = "digest"
	p.lastSystemPrompt = "You are Jarvis."
	p.sessionWorkingDir = "/work/repo"
	return p
}

func TestClaudeNativeCompactUnavailableWithoutSession(t *testing.T) {
	noRun := func(context.Context, []string, string, string, string) ([]byte, error) {
		t.Fatal("runner must not start without a resumable session")
		return nil, nil
	}
	p := NewClaudeBinProvider("haiku", nil)
	p.nativeCompactRunner = noRun
	forked := resumedClaudeProvider()
	forked.forkSession = true
	forked.nativeCompactRunner = noRun
	for _, provider := range []*ClaudeBinProvider{p, forked} {
		if _, err := provider.NativeCompact(context.Background(), core.NativeCompactRequest{}); !errors.Is(err, core.ErrNativeCompactUnavailable) {
			t.Fatalf("err = %v, want ErrNativeCompactUnavailable", err)
		}
	}
}

func TestClaudeNativeCompactRunsSlashCompactOnResumedSession(t *testing.T) {
	p := resumedClaudeProvider()
	var gotArgs []string
	var gotDir, gotInput string
	p.nativeCompactRunner = func(_ context.Context, args []string, _, workingDir, stdin string) ([]byte, error) {
		gotArgs, gotDir, gotInput = args, workingDir, stdin
		return []byte(strings.Join([]string{
			`{"type":"system","subtype":"init","session_id":"sess-1"}`,
			`{"type":"system","subtype":"compact_boundary","session_id":"sess-1","compact_metadata":{"trigger":"manual","pre_tokens":726,"post_tokens":321}}`,
			`{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"sess-1"}`,
		}, "\n")), nil
	}

	res, err := p.NativeCompact(context.Background(), core.NativeCompactRequest{SystemPrompt: "stale caller prompt"})
	if err != nil {
		t.Fatalf("NativeCompact: %v", err)
	}
	if res.PreTokens != 726 || res.PostTokens != 321 {
		t.Fatalf("result = %+v", res)
	}
	if gotDir != "/work/repo" {
		t.Fatalf("working dir = %q, want the session's project dir", gotDir)
	}
	if slices.Contains(gotArgs, "--disable-slash-commands") {
		t.Fatalf("args must enable slash commands: %v", gotArgs)
	}
	for _, want := range [][2]string{{"--resume", "sess-1"}, {"--system-prompt", "You are Jarvis."}, {"--input-format", "stream-json"}, {"--max-turns", "1"}, {"--tools", ""}} {
		i := slices.Index(gotArgs, want[0])
		if i < 0 || i+1 >= len(gotArgs) || gotArgs[i+1] != want[1] {
			t.Fatalf("args missing %s %q: %v", want[0], want[1], gotArgs)
		}
	}
	var msg sdkUserMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(gotInput)), &msg); err != nil {
		t.Fatalf("stdin is not one stream-json message: %v\n%s", err, gotInput)
	}
	if len(msg.Message.Content) != 1 || msg.Message.Content[0].Text != "/compact" {
		t.Fatalf("stdin content = %+v", msg.Message.Content)
	}
	// The transcript is untouched, so resume state must survive compaction.
	if p.sessionID != "sess-1" || p.messagesSent != 3 || p.transcriptDigest != "digest" {
		t.Fatalf("resume state changed: %q %d %q", p.sessionID, p.messagesSent, p.transcriptDigest)
	}
}

func TestClaudeNativeCompactReportsRefusal(t *testing.T) {
	p := resumedClaudeProvider()
	p.nativeCompactRunner = func(context.Context, []string, string, string, string) ([]byte, error) {
		return []byte(`{"type":"result","subtype":"success","is_error":false,"result":"Not enough messages to compact."}`), nil
	}
	_, err := p.NativeCompact(context.Background(), core.NativeCompactRequest{})
	if err == nil || errors.Is(err, core.ErrNativeCompactUnavailable) || !strings.Contains(err.Error(), "Not enough messages to compact.") {
		t.Fatalf("err = %v, want surfaced refusal", err)
	}
}

func TestClaudeNativeCompactChecksBusyBeforeReadingResumeState(t *testing.T) {
	// Resume fields are only stable under the stream gate, so even a
	// session-less provider must report busy rather than unavailable.
	p := NewClaudeBinProvider("haiku", nil)
	p.activeStream.Store(true)
	if _, err := p.NativeCompact(context.Background(), core.NativeCompactRequest{}); err == nil || errors.Is(err, core.ErrNativeCompactUnavailable) {
		t.Fatalf("err = %v, want busy error", err)
	}
}

func TestClaudeNativeCompactWorkingDirSurvivesStateRoundTrip(t *testing.T) {
	state, ok := resumedClaudeProvider().ExportProviderState()
	if !ok {
		t.Fatal("export failed")
	}
	restored := NewClaudeBinProvider("haiku", nil)
	restored.lastSystemPrompt = "other session"
	if err := restored.ImportProviderState(state); err != nil {
		t.Fatal(err)
	}
	if restored.sessionWorkingDir != "/work/repo" || restored.lastSystemPrompt != "" {
		t.Fatalf("restored dir=%q prompt=%q", restored.sessionWorkingDir, restored.lastSystemPrompt)
	}
}

func TestClaudeStreamJSONInputEscapesLeadingSlash(t *testing.T) {
	// Claude Code would otherwise intercept "/remote-control ..." as a command.
	p := NewClaudeBinProvider("haiku", nil)
	var msg sdkUserMessage
	if err := json.Unmarshal([]byte(p.buildStreamJsonInput([]core.Message{core.UserText("/remote-control explain")}, "")), &msg); err != nil {
		t.Fatal(err)
	}
	if got := msg.Message.Content[0].Text; got != " /remote-control explain" {
		t.Fatalf("text = %q, want leading-space escape", got)
	}
}
