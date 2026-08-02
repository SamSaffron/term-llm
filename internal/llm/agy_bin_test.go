package llm

import (
	"context"
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

func (f *fakeAgyIsolation) EnsureStarted() error { f.started = true; return nil }
func (f *fakeAgyIsolation) BeginTurn(v bool)     { f.require = v }
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
	if !caps.ToolCalls || !caps.InlineToolLoop || !caps.OrderedInlineToolEvents {
		t.Fatalf("capabilities = %+v", caps)
	}
	if caps.ManagesOwnContext {
		t.Fatal("agy-bin should let term-llm manage transcript context")
	}
}

func TestAgyBuildArgs(t *testing.T) {
	p := NewAgyBinProvider("gemini-3.6-flash-high", nil)
	args := p.buildArgs(Request{})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--dangerously-skip-permissions", "--disable-slash-commands", "--output-format stream-json", "--model gemini-3.6-flash-high"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
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
