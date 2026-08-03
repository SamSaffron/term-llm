package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

type fakeAgyIsolation struct {
	started bool
	require bool
	env     map[string]string
}

func (f *fakeAgyIsolation) EnsureStarted(string) error { f.started = true; return nil }
func (f *fakeAgyIsolation) BeginTurn(v bool)           { f.require = v }
func (f *fakeAgyIsolation) FilteredGenerations() int64 {
	if f.started {
		return 1
	}
	return 0
}
func (f *fakeAgyIsolation) Environment() map[string]string { return f.env }
func (f *fakeAgyIsolation) Stop(context.Context) error     { f.started = false; return nil }

func TestAgyBinCapabilities(t *testing.T) {
	caps := NewAgyBinProvider("", nil).Capabilities()
	if !caps.ToolCalls || !caps.ManagesOwnContext || !caps.InlineToolLoop || !caps.OrderedInlineToolEvents {
		t.Fatalf("capabilities = %+v, want managed context with inline tool calls", caps)
	}
}

func TestAgyBuildArgs(t *testing.T) {
	p := NewAgyBinProvider("gemini-3.6-flash-high", nil)
	args := p.buildArgs(Request{}, "")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--dangerously-skip-permissions", "--disable-slash-commands", "--output-format stream-json", "--model gemini-3.6-flash-high"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
}

func TestAgyBuildArgsResume(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	joined := strings.Join(p.buildArgs(Request{}, "conversation-1"), " ")
	if !strings.Contains(joined, "--conversation conversation-1") {
		t.Fatalf("resume args = %q", joined)
	}
	joined = strings.Join(p.buildArgs(Request{Ephemeral: true}, "conversation-1"), " ")
	if strings.Contains(joined, "--conversation") {
		t.Fatalf("ephemeral args resumed live conversation: %q", joined)
	}
}

func TestAgyProxyVerificationFailureResetsResumedState(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	p.isolation = &fakeAgyIsolation{}
	p.conversationID = "conversation-1"
	p.messagesSent = 3
	p.transcriptHash = "hash"

	err := p.requireFilteredGeneration("conversation-1")
	if err == nil || !strings.Contains(err.Error(), "did not route generation") {
		t.Fatalf("verification error = %v", err)
	}
	if p.conversationID != "" || p.messagesSent != 0 || p.transcriptHash != "" {
		t.Fatalf("failed resumed turn retained state (%q,%d,%q)", p.conversationID, p.messagesSent, p.transcriptHash)
	}
}

func TestAgyMessagesForResumedRequest(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	p.conversationID = "conversation-1"
	p.messagesSent = 2
	messages := []Message{SystemText("system"), UserText("first"), AssistantText("answer"), UserText("second")}
	p.transcriptHash, _ = agyTranscriptHash(messages[:p.messagesSent])
	got, err := p.messagesForRequest(Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Role != RoleUser || got[0].Parts[0].Text != "second" {
		t.Fatalf("resume messages = %#v, want only the new user message", got)
	}

	p.messagesSent = len(messages) + 1
	got, err = p.messagesForRequest(Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(messages) || p.conversationID != "" || p.messagesSent != 0 || p.transcriptHash != "" {
		t.Fatalf("truncated transcript did not reset resume state: messages=%d state=(%q,%d,%q)", len(got), p.conversationID, p.messagesSent, p.transcriptHash)
	}

	p.conversationID = "conversation-2"
	p.messagesSent = 2
	p.transcriptHash, _ = agyTranscriptHash(messages[:p.messagesSent])
	diverged := append([]Message(nil), messages...)
	diverged[1] = UserText("edited first message")
	got, err = p.messagesForRequest(Request{Messages: diverged})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(diverged) || p.conversationID != "" {
		t.Fatalf("diverged transcript did not reset continuation: messages=%d conversation=%q", len(got), p.conversationID)
	}
}

func TestAgyProviderStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base, err := agyBinCacheBase()
	if err != nil {
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
	conversationID := "conversation-1"
	conversationDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(conversationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conversationDir, conversationID+".db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := NewAgyBinProvider("", nil)
	p.agyHome, p.conversationID, p.messagesSent = home, conversationID, 4
	p.transcriptHash = "transcript-hash"
	state, ok := p.ExportProviderState()
	if !ok {
		t.Fatal("ExportProviderState returned false")
	}
	restored := NewAgyBinProvider("", nil)
	if err := restored.ImportProviderState(state); err != nil {
		t.Fatal(err)
	}
	if restored.agyHome != home || restored.conversationID != conversationID || restored.messagesSent != 4 || restored.transcriptHash != "transcript-hash" {
		t.Fatalf("restored state = (%q,%q,%d,%q)", restored.agyHome, restored.conversationID, restored.messagesSent, restored.transcriptHash)
	}

	malicious, _ := json.Marshal(agyBinProviderState{AgyHome: filepath.Join(base, "..", id), ConversationID: "attacker", MessagesSent: 1, TranscriptHash: "hash"})
	if err := restored.ImportProviderState(malicious); err == nil {
		t.Fatal("ImportProviderState accepted path traversal")
	}
}

func TestAgyProviderStateRejectsSymlinkedHome(t *testing.T) {
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
	link := filepath.Join(base, id)
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	state, _ := json.Marshal(agyBinProviderState{AgyHome: link, ConversationID: "conversation", MessagesSent: 1, TranscriptHash: "hash"})
	if err := NewAgyBinProvider("", nil).ImportProviderState(state); err == nil {
		t.Fatal("ImportProviderState accepted symlinked home")
	}
}

func TestValidAgyConversationID(t *testing.T) {
	if !validAgyConversationID("487f86b0-efdd-4072-9fbc-5e1ef2c3cdb7") {
		t.Fatal("valid agy conversation ID was rejected")
	}
	for _, id := range []string{"", ".", "..", "../config", "foo/bar", `foo\bar`, "/absolute", "line\nbreak", strings.Repeat("a", 129)} {
		if validAgyConversationID(id) {
			t.Fatalf("unsafe conversation ID %q was accepted", id)
		}
	}
}

func TestAgyProviderStateRejectsUnsafeConversationID(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base, err := agyBinCacheBase()
	if err != nil {
		t.Fatal(err)
	}
	id, err := newGrokHomeID()
	if err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(agyBinProviderState{
		AgyHome:        filepath.Join(base, id),
		ConversationID: "../config",
		MessagesSent:   1,
		TranscriptHash: "hash",
	})
	if err := NewAgyBinProvider("", nil).ImportProviderState(state); err == nil {
		t.Fatal("ImportProviderState accepted traversing conversation ID")
	}
}

func TestAgyProviderStateMissingConversationResetsResume(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base, err := agyBinCacheBase()
	if err != nil {
		t.Fatal(err)
	}
	id, err := newGrokHomeID()
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, id)
	state, _ := json.Marshal(agyBinProviderState{AgyHome: home, ConversationID: "missing", MessagesSent: 3, TranscriptHash: "hash"})
	p := NewAgyBinProvider("", nil)
	if err := p.ImportProviderState(state); err != nil {
		t.Fatal(err)
	}
	if p.conversationID != "" || p.messagesSent != 0 || p.transcriptHash != "" {
		t.Fatalf("missing conversation retained resume state (%q,%d,%q)", p.conversationID, p.messagesSent, p.transcriptHash)
	}
}

func TestAgyBuildCommandEnvCannotBypassIsolation(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://inherited.invalid")
	p := NewAgyBinProvider("", map[string]string{"HTTPS_PROXY": "http://configured.invalid", "HOME": "/bad"})
	p.agyHome = t.TempDir()
	p.isolation = &fakeAgyIsolation{env: map[string]string{"HTTPS_PROXY": "http://127.0.0.1:1234", "SSL_CERT_FILE": "/tmp/ca"}}
	env := envSliceMap(p.buildCommandEnv())
	if env["HTTPS_PROXY"] != "http://127.0.0.1:1234" {
		t.Fatalf("HTTPS_PROXY = %q", env["HTTPS_PROXY"])
	}
	if env["HOME"] != p.agyHome {
		t.Fatalf("HOME = %q", env["HOME"])
	}
}

func TestHandleAgyStreamLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan Event, 4)
	send := eventSender{ch: ch, ctx: ctx}
	state := &agyStreamState{}
	if err := handleAgyStreamLine(`{"event":"step_update","step_update":{"conversation_id":"c1","step_type":"agent_response","text_delta":"hello"}}`, send, state); err != nil {
		t.Fatal(err)
	}
	e := <-ch
	if e.Type != EventTextDelta || e.Text != "hello" {
		t.Fatalf("event = %+v", e)
	}
	if err := handleAgyStreamLine(`{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"hello","usage":{"input_tokens":10,"output_tokens":2,"cache_read_tokens":3}}}`, send, state); err != nil {
		t.Fatal(err)
	}
	if !state.sawResult || state.usage.InputTokens != 10 || state.usage.CachedInputTokens != 3 {
		t.Fatalf("state = %+v", state)
	}
}

func TestHandleAgyStreamLineRejectsResumeMismatchBeforeOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan Event, 1)
	send := eventSender{ch: ch, ctx: ctx}
	state := &agyStreamState{expectedConversationID: "expected"}

	err := handleAgyStreamLine(`{"event":"step_update","step_update":{"conversation_id":"different","step_type":"agent_response","text_delta":"must not leak"}}`, send, state)
	if !errors.Is(err, errAgyConversationMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	if len(ch) != 0 || state.sawText {
		t.Fatalf("mismatched continuation emitted output: events=%d sawText=%v", len(ch), state.sawText)
	}
}

func TestHandleAgyStreamLineAllowsOmittedIDAfterResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan Event, 1)
	send := eventSender{ch: ch, ctx: ctx}
	state := &agyStreamState{conversationID: "expected", expectedConversationID: "expected"}

	if err := handleAgyStreamLine(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hello"}}`, send, state); err != nil {
		t.Fatal(err)
	}
	if event := <-ch; event.Type != EventTextDelta || event.Text != "hello" {
		t.Fatalf("event = %+v", event)
	}
	if err := handleAgyStreamLine(`{"event":"result","result":{"status":"SUCCESS","response":"hello"}}`, send, state); err != nil {
		t.Fatal(err)
	}
	if !state.sawResult || state.conversationID != "expected" {
		t.Fatalf("state = %+v", state)
	}
}

func TestAgyPathReplacerMapsPrivateScratchToWorkspace(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "tmp", "term-llm-agy-home-123")
	workspace := filepath.Join(string(filepath.Separator), "src", "term-llm")
	replacer := agyPathReplacer(home, workspace)
	input := "See [embed.go](file://" + filepath.ToSlash(filepath.Join(home, ".gemini", "antigravity-cli", "scratch", "internal", "serveui", "embed.go")) + ")"
	want := "See [embed.go](file://" + filepath.ToSlash(filepath.Join(workspace, "internal", "serveui", "embed.go")) + ")"
	if got := replacer.Replace(input); got != want {
		t.Fatalf("rewritten path = %q, want %q", got, want)
	}
}

func TestBuildAgyPromptIdentifiesActualWorkspace(t *testing.T) {
	workspace := filepath.Join(string(filepath.Separator), "src", "term-llm")
	prompt := buildAgyPrompt([]Message{UserText("inspect files")}, workspace)
	if !strings.Contains(prompt, workspace) || !strings.Contains(prompt, "temporary Antigravity scratch") {
		t.Fatalf("prompt does not anchor workspace: %q", prompt)
	}
}

func TestAgyMCPConfigIsPrivate(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	p.agyHome = t.TempDir()
	p.mcpURL = "http://127.0.0.1:1234/mcp"
	p.mcpToken = "secret"
	for _, d := range []string{"config", "antigravity", "antigravity-cli"} {
		if err := os.MkdirAll(filepath.Join(p.agyHome, ".gemini", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.writeMCPConfigs(true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.agyHome, ".gemini", "config", "mcp_config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "serverUrl") || !strings.Contains(string(data), "Bearer secret") {
		t.Fatalf("config = %s", data)
	}
}

func TestCreateProviderFromConfigAgyBin(t *testing.T) {
	provider, err := createProviderFromConfig("agy-bin", &configProviderForAgyTest)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*AgyBinProvider); !ok {
		t.Fatalf("provider = %T", provider)
	}
}

var configProviderForAgyTest = func() (cfg config.ProviderConfig) { cfg.Model = "gemini-3.6-flash-high"; return }()
