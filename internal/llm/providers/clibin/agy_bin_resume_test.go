package clibin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func agyTestDeveloper(text string) core.Message {
	return core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: text}}}
}

// agyTestStableTranscript is a completed review turn with a tool call and tool
// result that agy already generated natively.
func agyTestStableTranscript() []core.Message {
	return []core.Message{
		core.SystemText("system policy"),
		core.UserText("first request"),
		{Role: core.RoleAssistant, Parts: []core.Part{
			{Type: core.PartText, Text: "working"},
			{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "t1", Name: "read_file"}},
		}},
		{Role: core.RoleTool, Parts: []core.Part{
			{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: "t1", Name: "read_file", Content: "tool evidence"}},
		}},
		core.AssistantText("review complete"),
	}
}

func agyTestClone(messages []core.Message) []core.Message {
	out := make([]core.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Parts = make([]core.Part, len(message.Parts))
		for j, part := range message.Parts {
			out[i].Parts[j] = part
			if part.ToolCall != nil {
				call := *part.ToolCall
				out[i].Parts[j].ToolCall = &call
			}
			if part.ToolResult != nil {
				result := *part.ToolResult
				out[i].Parts[j].ToolResult = &result
			}
		}
	}
	return out
}

func agyTestMessageSummary(messages []core.Message) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		out = append(out, string(message.Role)+":"+core.CollectTextParts(message.Parts))
	}
	return out
}

func TestAgyTranscriptHashIgnoresDisplayMetadata(t *testing.T) {
	stable := agyTestStableTranscript()
	plain, err := agyTranscriptHash(stable)
	if err != nil {
		t.Fatal(err)
	}
	decorated := agyTestClone(stable)
	for i := range decorated {
		decorated[i].AuthorID = "author-canary"
		decorated[i].CacheAnchor = true
		decorated[i].ClientMessageID = "client-canary"
		decorated[i].DisplayText = "display-canary"
		decorated[i].ResponseID = "response-canary"
		decorated[i].ApprovalRole = "policy"
		decorated[i].ApprovalText = "approval-canary"
		decorated[i].AssistantSegmentOrdinal = 4
		decorated[i].SegmentStartSequence = 11
		decorated[i].SegmentEndSequence = 12
		for j := range decorated[i].Parts {
			decorated[i].Parts[j].CreatedAt = 987654321
			decorated[i].Parts[j].ReasoningContent = "reasoning-canary"
			decorated[i].Parts[j].ReasoningItemID = "reasoning-item-canary"
			decorated[i].Parts[j].ReasoningKind = core.ReasoningKindSummary
			decorated[i].Parts[j].ReasoningSummaryTitle = "title-canary"
			if decorated[i].Parts[j].ToolCall != nil {
				decorated[i].Parts[j].ToolCall.ToolInfo = "tool-info-canary"
			}
		}
	}
	// Durable timeline markers are never provider context.
	decorated = append([]core.Message{{Role: core.RoleEvent, Parts: []core.Part{{Type: core.PartText, Text: "timeline event"}}}}, decorated...)
	got, err := agyTranscriptHash(decorated)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Fatalf("display metadata changed the provider transcript hash")
	}
}

func TestAgyTranscriptHashTracksRenderedContent(t *testing.T) {
	base := agyTestStableTranscript()
	baseHash, err := agyTranscriptHash(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]core.Message) []core.Message
	}{
		{name: "system text", mutate: func(m []core.Message) []core.Message { m[0] = core.SystemText("edited policy"); return m }},
		{name: "developer text", mutate: func(m []core.Message) []core.Message {
			return append([]core.Message{agyTestDeveloper("new policy")}, m...)
		}},
		{name: "user text", mutate: func(m []core.Message) []core.Message { m[1] = core.UserText("edited request"); return m }},
		{name: "assistant text", mutate: func(m []core.Message) []core.Message { m[4] = core.AssistantText("edited review"); return m }},
		{name: "tool call name", mutate: func(m []core.Message) []core.Message {
			m[2].Parts[1].ToolCall = &core.ToolCall{ID: "t1", Name: "write_file"}
			return m
		}},
		{name: "tool result", mutate: func(m []core.Message) []core.Message {
			m[3].Parts[0].ToolResult = &core.ToolResult{ID: "t1", Name: "read_file", Content: "edited evidence"}
			return m
		}},
		{name: "image part", mutate: func(m []core.Message) []core.Message {
			m[1].Parts = append(m[1].Parts, core.Part{Type: core.PartImage, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "AAAA"}})
			return m
		}},
		{name: "new user turn", mutate: func(m []core.Message) []core.Message { return append(m, core.UserText("extra")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := test.mutate(agyTestClone(base))
			got, err := agyTranscriptHash(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if got == baseHash {
				t.Fatal("genuine provider-visible content change did not change the hash")
			}
		})
	}
}

func TestAgyNativeResumeKeepsStableTranscriptAcrossTurnContext(t *testing.T) {
	stable := agyTestStableTranscript()
	for _, test := range []struct {
		name    string
		context []core.Message
	}{
		{name: "moved roster", context: []core.Message{agyTestDeveloper("roster: cell-1")}},
		{name: "changed roster", context: []core.Message{agyTestDeveloper("roster: cell-1 cell-2")}},
		{name: "removed roster", context: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewAgyBinProvider("", nil)
			p.conversationID = "native-1"
			p.messagesSent = len(stable)
			p.transcriptHash, _ = agyTranscriptHash(stable)
			followUp := append(agyTestClone(stable), core.UserText("continue with the fix"))
			req := core.Request{
				Messages:             core.WithTurnContext(followUp, test.context),
				ProviderConversation: &core.ProviderConversation{Messages: followUp, TurnContext: test.context},
			}
			plan, err := p.planTurn(req, "")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Mode != agyTurnNative || plan.ResumeID != "native-1" {
				t.Fatalf("plan = mode %s resume %q reason %s", plan.Mode, plan.ResumeID, plan.Reason)
			}
			if plan.SentCount != len(followUp) {
				t.Fatalf("sent count = %d, want %d stable messages", plan.SentCount, len(followUp))
			}
			want := core.WithTurnContext([]core.Message{core.UserText("continue with the fix")}, test.context)
			if got := agyTestMessageSummary(plan.Messages); !reflect.DeepEqual(got, agyTestMessageSummary(want)) {
				t.Fatalf("resumed payload = %v, want %v", got, agyTestMessageSummary(want))
			}
			for _, marker := range []string{"first request", "review complete", "tool evidence"} {
				if strings.Contains(plan.Prompt, marker) {
					t.Fatalf("resumed prompt replayed native history %q: %s", marker, plan.Prompt)
				}
			}
			if !strings.Contains(plan.Prompt, "continue with the fix") {
				t.Fatalf("resumed prompt lost the follow-up: %s", plan.Prompt)
			}
			if len(test.context) > 0 && !strings.Contains(plan.Prompt, "roster: cell-1") {
				t.Fatalf("resumed prompt lost current turn context: %s", plan.Prompt)
			}
			if plan.PromptBytes != len(plan.Prompt) {
				t.Fatalf("prompt bytes = %d, want %d", plan.PromptBytes, len(plan.Prompt))
			}
		})
	}
}

func TestAgyNativeResumePreservesSteeringBeforeNewUser(t *testing.T) {
	stable := agyTestStableTranscript()
	p := NewAgyBinProvider("", nil)
	p.conversationID = "native-1"
	p.messagesSent = len(stable)
	p.transcriptHash, _ = agyTranscriptHash(stable)
	roster := []core.Message{agyTestDeveloper("roster: cell-3")}
	delta := []core.Message{agyTestDeveloper("steering: focus on tests"), core.UserText("continue with the fix")}
	followUp := append(agyTestClone(stable), delta...)
	req := core.Request{
		Messages:             core.WithTurnContext(followUp, roster),
		ProviderConversation: &core.ProviderConversation{Messages: followUp, TurnContext: roster},
	}
	plan, err := p.planTurn(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Messages) != 3 {
		t.Fatalf("resumed payload = %v", agyTestMessageSummary(plan.Messages))
	}
	want := []core.Message{agyTestDeveloper("steering: focus on tests"), agyTestDeveloper("roster: cell-3"), core.UserText("continue with the fix")}
	if got := agyTestMessageSummary(plan.Messages); !reflect.DeepEqual(got, agyTestMessageSummary(want)) {
		t.Fatalf("resumed payload = %v, want %v", got, agyTestMessageSummary(want))
	}
	for _, text := range []string{"steering: focus on tests", "roster: cell-3", "continue with the fix"} {
		if !strings.Contains(plan.Prompt, text) {
			t.Fatalf("resumed prompt lost %q: %s", text, plan.Prompt)
		}
	}
}

func TestAgyNativeResumeRejectsContextOnlySuffix(t *testing.T) {
	stable := []core.Message{core.UserText("first"), core.AssistantText("done")}
	p := NewAgyBinProvider("", nil)
	p.conversationID = "native-1"
	p.messagesSent = len(stable)
	p.transcriptHash, _ = agyTranscriptHash(stable)
	roster := []core.Message{agyTestDeveloper("roster: cell-9")}
	followUp := append(agyTestClone(stable), core.AssistantText("native tail"))
	req := core.Request{
		Messages:             core.WithTurnContext(followUp, roster),
		ProviderConversation: &core.ProviderConversation{Messages: followUp, TurnContext: roster},
	}
	if _, err := p.planTurn(req, ""); err == nil || !strings.Contains(err.Error(), "no new messages") {
		t.Fatalf("context-only suffix error = %v", err)
	}
	if p.conversationID != "native-1" || p.messagesSent != len(stable) {
		t.Fatal("context-only suffix discarded a resumable cursor")
	}
}

func TestAgyNativeResumeKeepsNewSystemInstruction(t *testing.T) {
	stable := []core.Message{core.SystemText("original policy"), core.UserText("first"), core.AssistantText("done")}
	p := NewAgyBinProvider("", nil)
	p.conversationID = "native-1"
	p.messagesSent = len(stable)
	p.transcriptHash, _ = agyTranscriptHash(stable)
	followUp := append(agyTestClone(stable), core.SystemText("new policy"), core.UserText("second"))
	req := core.Request{
		Messages:             followUp,
		ProviderConversation: &core.ProviderConversation{Messages: followUp},
	}
	plan, err := p.planTurn(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agyTurnNative {
		t.Fatalf("plan mode = %s", plan.Mode)
	}
	if got := agyTestMessageSummary(plan.Messages); !reflect.DeepEqual(got, []string{"system:new policy", "user:second"}) {
		t.Fatalf("resumed payload = %v, want the new system instruction and follow-up", got)
	}
	if !strings.Contains(plan.Prompt, "<system>\nnew policy\n</system>") {
		t.Fatalf("resumed prompt lost the new system instruction: %s", plan.Prompt)
	}
}

func TestAgyDebugPrintsBoundedTurnSummary(t *testing.T) {
	dir := t.TempDir()
	stop, err := runtimeoutput.Start(dir)
	if err != nil {
		t.Skipf("diagnostic log unavailable: %v", err)
	}
	t.Cleanup(stop)
	p := NewAgyBinProvider("", nil)
	const promptCanary = "PROMPT-CONTENT-MUST-NOT-APPEAR"
	if _, err := p.planTurn(core.Request{SessionID: "session-7", Debug: true, Messages: []core.Message{core.UserText(promptCanary)}}, ""); err != nil {
		t.Fatal(err)
	}
	stop()
	data, err := os.ReadFile(filepath.Join(dir, "tui.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{"[agy-bin] turn mode=initial", `session_id="session-7"`, "prompt_bytes=", "stable_messages=1"} {
		if !strings.Contains(log, want) {
			t.Fatalf("debug log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, promptCanary) {
		t.Fatal("debug diagnostics leaked prompt text")
	}
}

func TestAgyInitialPlanReplaysPreparedRequest(t *testing.T) {
	stable := []core.Message{core.SystemText("policy"), core.UserText("first")}
	roster := []core.Message{agyTestDeveloper("roster: cell-1")}
	prepared := core.WithTurnContext(stable, roster)
	p := NewAgyBinProvider("", nil)
	plan, err := p.planTurn(core.Request{
		Messages:             prepared,
		ProviderConversation: &core.ProviderConversation{Messages: stable, TurnContext: roster},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agyTurnInitial || plan.ResumeID != "" || plan.SentCount != len(stable) {
		t.Fatalf("initial plan = %+v", plan)
	}
	if !reflect.DeepEqual(plan.Messages, prepared) {
		t.Fatal("initial plan did not replay the complete prepared request")
	}
	for _, text := range []string{"policy", "roster: cell-1", "first"} {
		if !strings.Contains(plan.Prompt, text) {
			t.Fatalf("initial prompt lost %q: %s", text, plan.Prompt)
		}
	}
}

func TestAgyEphemeralTurnNeverResumesStoredState(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	p.conversationID = "native-1"
	p.messagesSent = 2
	p.transcriptHash = "hash"
	messages := []core.Message{core.UserText("one-shot")}
	plan, err := p.planTurn(core.Request{Ephemeral: true, Messages: messages}, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agyTurnInitial || plan.ResumeID != "" || plan.Reason != agyReasonEphemeral {
		t.Fatalf("ephemeral plan = %+v", plan)
	}
	if p.conversationID != "native-1" || p.messagesSent != 2 || p.transcriptHash != "hash" {
		t.Fatal("ephemeral turn mutated stored continuation state")
	}
}

func TestAgyPlanDirectRequestWithoutMetadata(t *testing.T) {
	messages := []core.Message{core.UserText("question")}
	p := NewAgyBinProvider("", nil)
	plan, err := p.planTurn(core.Request{Messages: messages}, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agyTurnInitial || plan.SentCount != 1 || len(plan.Messages) != 1 {
		t.Fatalf("direct initial plan = %+v", plan)
	}

	p.conversationID = "native-1"
	p.messagesSent = 1
	p.transcriptHash, _ = agyTranscriptHash(messages)
	next := append(messages, core.UserText("follow-up"))
	plan, err = p.planTurn(core.Request{Messages: next}, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agyTurnNative || plan.SentCount != 2 {
		t.Fatalf("direct resume plan = %+v", plan)
	}
	if got := agyTestMessageSummary(plan.Messages); !reflect.DeepEqual(got, []string{"user:follow-up"}) {
		t.Fatalf("direct resume payload = %v", got)
	}
}

func TestAgyFailedTurnCursorInvalidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		req      core.Request
		resumeID string
		err      error
		retained bool
	}{
		{name: "pre-exec failure", resumeID: "native-1", err: errors.New("start agy command: exec: no such file"), retained: true},
		{name: "post-start failure", resumeID: "native-1", err: &agyProcessError{err: errors.New("agy command failed")}},
		{name: "conversation mismatch", resumeID: "native-1", err: fmt.Errorf("%w: changed", errAgyConversationMismatch)},
		{name: "ephemeral", resumeID: "native-1", req: core.Request{Ephemeral: true}, err: &agyProcessError{err: errors.New("failed")}, retained: true},
		{name: "fresh turn", resumeID: "", err: &agyProcessError{err: errors.New("failed")}, retained: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewAgyBinProvider("", nil)
			p.conversationID, p.messagesSent, p.transcriptHash = "native-1", 3, "hash"
			p.invalidateResumeAfterFailedTurn(test.req, agyTurnPlan{ResumeID: test.resumeID, SentCount: 4}, test.err)
			if test.retained {
				if p.conversationID != "native-1" || p.messagesSent != 3 || p.transcriptHash != "hash" {
					t.Fatal("pre-exec failure discarded a good cursor")
				}
				return
			}
			if p.conversationID != "" || p.messagesSent != 0 || p.transcriptHash != "" {
				t.Fatalf("uncertain failure retained stale cursor (%q,%d,%q)", p.conversationID, p.messagesSent, p.transcriptHash)
			}
			if p.fallbackReason == "" {
				t.Fatal("uncertain failure did not retain a bounded fallback reason")
			}
		})
	}
}

func TestAgyImportStateFallbackReasons(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base, err := agyBinCacheBase()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := newGrokHomeID()
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, id)
	if err := ensureAgyHomeLayout(home); err != nil {
		t.Fatal(err)
	}
	conversationID := "native-1"
	conversationDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(conversationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conversationDir, conversationID+".db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	encode := func(format int, home, conversation string) []byte {
		data, marshalErr := json.Marshal(agyBinProviderState{
			Format:         format,
			AgyHome:        home,
			ConversationID: conversation,
			MessagesSent:   5,
			TranscriptHash: "hash",
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return data
	}

	t.Run("valid state", func(t *testing.T) {
		p := NewAgyBinProvider("", nil)
		if err := p.ImportProviderState(encode(agyBinStateFormat, home, conversationID)); err != nil {
			t.Fatal(err)
		}
		if p.conversationID != conversationID || p.messagesSent != 5 || p.transcriptHash != "hash" || p.fallbackReason != "" {
			t.Fatalf("restored state = (%q,%d,%q,%q)", p.conversationID, p.messagesSent, p.transcriptHash, p.fallbackReason)
		}
	})

	for _, test := range []struct {
		name       string
		state      []byte
		wantReason string
	}{
		{name: "old format", state: encode(agyBinStateFormat-1, home, conversationID), wantReason: agyStateReasonOldFormat},
		{name: "missing conversation db", state: encode(agyBinStateFormat, home, "missing-db"), wantReason: agyStateReasonMissingDatabase},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewAgyBinProvider("", nil)
			if err := p.ImportProviderState(test.state); err != nil {
				t.Fatal(err)
			}
			if p.conversationID != "" || p.messagesSent != 0 || p.transcriptHash != "" {
				t.Fatalf("unusable state resumed (%q,%d,%q)", p.conversationID, p.messagesSent, p.transcriptHash)
			}
			if p.fallbackReason != test.wantReason {
				t.Fatalf("fallback reason = %q, want %q", p.fallbackReason, test.wantReason)
			}
			plan, err := p.planTurn(core.Request{Messages: []core.Message{core.UserText("replay")}}, "")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Mode != agyTurnFallback || plan.Reason != test.wantReason {
				t.Fatalf("next plan = mode %s reason %s, want transcript_fallback/%s", plan.Mode, plan.Reason, test.wantReason)
			}
		})
	}

	t.Run("missing home", func(t *testing.T) {
		missingID, err := newGrokHomeID()
		if err != nil {
			t.Fatal(err)
		}
		missingHome := filepath.Join(base, missingID)
		p := NewAgyBinProvider("", nil)
		if err := p.ImportProviderState(encode(agyBinStateFormat, missingHome, conversationID)); err != nil {
			t.Fatal(err)
		}
		if p.agyHome != missingHome || p.conversationID != "" {
			t.Fatalf("missing home state = (%q,%q)", p.agyHome, p.conversationID)
		}
		if p.fallbackReason != agyStateReasonMissingHome {
			t.Fatalf("fallback reason = %q, want %q", p.fallbackReason, agyStateReasonMissingHome)
		}
	})
}
