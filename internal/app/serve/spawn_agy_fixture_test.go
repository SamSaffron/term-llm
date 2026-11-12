package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/agyproxy"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

// The subprocess uses the production authenticated CONNECT/TLS filtering proxy.
// That proxy pins its upstream to Cloud Code:443, so reject outbound DNS rather
// than contact Google. Filtering/evidence occurs before upstream forwarding; the
// fake CLI explicitly checks the resulting 502. This tests production isolation,
// not a successful upstream generation (which requires a separate smoke test).
const agyLifecycleOfflineError = "agy-lifecycle-offline-upstream"

const agyLifecycleCLI = `#!/usr/bin/env python3
import base64, http.client, json, os, pathlib, shlex, ssl, sys, urllib.parse, urllib.request
args = sys.argv[1:]
if args == ["--version"]:
    print("1.5.6")
    sys.exit(0)
assert "--print" not in args, args
assert args[args.index("--input-format")+1] == "stream-json", args
lines = sys.stdin.read().splitlines()
assert len(lines) == 1, len(lines)
user = json.loads(lines[0])
assert user["event"] == "user" and isinstance(user["message"]["content"], str), user
home = pathlib.Path(os.environ["HOME"])
resume = args[args.index("--conversation")+1] if "--conversation" in args else ""
native = resume or ("native-first" if not os.path.exists(os.environ["FAKE_AGY_LOG"]) else "native-fallback")
db = home / ".gemini/antigravity-cli/conversations" / (native + ".db")
history = json.loads(db.read_text()) if resume else []
assert isinstance(history, list), str(db)
history.append(user["message"]["content"])
db.parent.mkdir(parents=True, exist_ok=True)
db.write_text(json.dumps(history))
with open(os.environ["FAKE_AGY_LOG"], "a") as f:
    f.write(json.dumps({"args":args,"input":user,"home":str(home),"native":native}) + "\n")
print(json.dumps({"event":"init","conversation_id":native}), flush=True)
# Send a real bridge-bearing request through the forced isolation proxy. No
# credentials are needed. The bogus native tool must be removed by the filter.
proxy = urllib.parse.urlparse(os.environ["HTTPS_PROXY"])
auth = base64.b64encode((urllib.parse.unquote(proxy.username)+":"+urllib.parse.unquote(proxy.password)).encode()).decode()
conn = http.client.HTTPSConnection(proxy.hostname, proxy.port, context=ssl.create_default_context(cafile=os.environ["SSL_CERT_FILE"]), timeout=10)
conn.set_tunnel("daily-cloudcode-pa.googleapis.com", 443, headers={"Proxy-Authorization":"Basic " + auth})
def native_prompt_text(prompt):
    # Reproduce agy 1.5.6's model-facing byte limit, including its notice. The
    # real filtering proxy must restore the staged original on this and every
    # resumed-history generation, not merely transport the full NDJSON input.
    raw = prompt.encode("utf-8")
    if len(raw) > 192000:
        return raw[:192000].decode("utf-8", "replace") + "\n<truncated " + str(len(raw)-192000) + " bytes>"
    return prompt
body = json.dumps({"request":{"tools":[{"functionDeclarations":[{"name":"run_command"},{"name":"mcp_term-llm_spawn_script"}]}],"contents":[{"role":"user","parts":[{"text":native_prompt_text(prompt)}]} for prompt in history]}})
conn.request("POST", "/v1internal:streamGenerateContent?alt=sse", body=body, headers={"Content-Type":"application/json"})
response = conn.getresponse()
error = response.read().decode()
assert response.status == 502 and "agy-lifecycle-offline-upstream" in error, (response.status,error)
conn.close()
def run_script(command, marker):
    cfg = json.load(open(home / ".gemini/config/mcp_config.json"))["mcpServers"]["term-llm"]
    code = "text(await tools.shell({command:" + json.dumps(command) + "}))"
    body = json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_script","arguments":{"code":code,"max_wait":10}}}).encode()
    headers = dict(cfg["headers"], **{"Content-Type":"application/json","Accept":"application/json, text/event-stream"})
    # The bridge remains local and bypasses all inherited ambient proxies.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    result = opener.open(urllib.request.Request(cfg["url"],data=body,headers=headers), timeout=15).read().decode()
    assert os.path.exists(os.environ["FAKE_AGY_EFFECT"]) and marker in result and "cell_id" in result, result
if not os.path.exists(os.environ["FAKE_AGY_EFFECT"]):
    run_script("printf x >> " + shlex.quote(os.environ["FAKE_AGY_EFFECT"]) + "; printf AGY_OLD_TOOL_MARKER", "AGY_OLD_TOOL_MARKER")
    if os.environ.get("FAKE_AGY_GATE_URL"):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        opener.open(os.environ["FAKE_AGY_GATE_URL"], timeout=30).read()
mode = os.environ.get("FAKE_AGY_FAILURE_MODE", "")
if resume and mode in ("partial_failure", "cancelled"):
    run_script("printf y >> " + shlex.quote(os.environ["FAKE_AGY_EFFECT"]) + "; printf AGY_PARTIAL_TOOL_MARKER", "AGY_PARTIAL_TOOL_MARKER")
    if mode == "cancelled":
        # The local handler signals readiness, then holds this request until
        # cancel_agent kills the real subprocess. No polling or timing sleeps.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        opener.open(os.environ["FAKE_AGY_READY_URL"], timeout=120).read()
        raise AssertionError("cancelled fake agy unexpectedly released")
    print("fake agy crashed after committed MCP tool", file=sys.stderr, flush=True)
    sys.exit(2)
print(json.dumps({"event":"result","result":{"conversation_id":native,"status":"SUCCESS","response":"agy child completed","usage":{"input_tokens":1,"output_tokens":1}}}), flush=True)
`

type agyLifecycleCall struct {
	Args   []string `json:"args"`
	Home   string   `json:"home"`
	Native string   `json:"native"`
	Input  struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"input"`
}

type agyLifecycleFixture struct {
	dir, logPath, effectPath, tracePath, dbPath string
	cfg                                         *config.Config
	ctx                                         context.Context
	dnsAttempts                                 atomic.Int32
}

func newAgyLifecycleFixture(t *testing.T) *agyLifecycleFixture {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake agy CLI")
	}
	f := &agyLifecycleFixture{dir: t.TempDir(), ctx: core.ContextWithCallID(core.ContextWithSessionID(context.Background(), "agy-parent"), "agy-spawn")}
	f.logPath, f.effectPath = filepath.Join(f.dir, "invocations"), filepath.Join(f.dir, "effect")
	f.tracePath, f.dbPath = filepath.Join(f.dir, "proxy-trace.jsonl"), filepath.Join(f.dir, "sessions.db")
	for key, value := range map[string]string{
		"HOME": filepath.Join(f.dir, "home"), "XDG_CONFIG_HOME": filepath.Join(f.dir, "config"), "XDG_CACHE_HOME": filepath.Join(f.dir, "cache"),
		"PATH": f.dir + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_AGY_LOG": f.logPath, "FAKE_AGY_EFFECT": f.effectPath,
		"FAKE_AGY_FAILURE_MODE": "", "FAKE_AGY_READY_URL": "", "FAKE_AGY_GATE_URL": "",
		agyproxy.GenerationTraceFileEnv: f.tracePath, "TERM_LLM_CLI_WIRE_TRACE": "",
	} {
		t.Setenv(key, value)
	}
	// Test-only credentials in a private HOME also avoid native keychain access
	// on macOS; never inspect or copy the developer's credentials.
	tokenDir := filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "antigravity-oauth-token"), []byte("offline-test-only"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "agy"), []byte(agyLifecycleCLI), 0755); err != nil {
		t.Fatal(err)
	}
	oldResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		f.dnsAttempts.Add(1)
		return nil, errors.New(agyLifecycleOfflineError)
	}}
	t.Cleanup(func() { net.DefaultResolver = oldResolver })
	agentsDir := filepath.Join(f.dir, "agents")
	agentDir := filepath.Join(agentsDir, "agy-child")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: agy-child\nmodel: agy-bin:gemini-3-flash\nskills: none\nmax_turns: 4\ntools:\n  enabled: [spawn_script, shell]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f.cfg = &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "fast", FastModel: "fast"}, "agy-bin": {Type: "agy-bin", Model: "gemini-3-flash"}}, Agents: config.AgentsConfig{SearchPaths: []string{agentsDir}}, Sessions: config.SessionsConfig{Enabled: true, Path: f.dbPath}}
	return f
}

func (f *agyLifecycleFixture) open(t *testing.T) (session.Store, *cmdRunEnvironment) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: f.dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defaults := cmdRunnerOptions{ConfigSet: true, Store: store, Yolo: true, Tools: "spawn_agent", ToolsSet: true}
	env, err := newCmdRunner(f.cfg, defaults).(*cmdRunner).prepare(f.ctx, runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: "agy-parent", Provider: "debug", Model: "fast", Cwd: f.dir, DeferSession: true}, nil)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, env
}

func TestAgyLifecycleOfflineProxyFixture(t *testing.T) {
	f := newAgyLifecycleFixture(t)
	// Validate the offline transport independently, before relying on it for
	// lifecycle assertions. The lifecycle test additionally uses the real provider.
	server := &agyproxy.Server{}
	proxyURL, caPath, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	server.BeginTurn(true)
	if err := os.WriteFile(f.effectPath, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(f.dir, "agy"), "--input-format", "stream-json")
	cmd.Env = append(os.Environ(), "HTTPS_PROXY="+proxyURL, "SSL_CERT_FILE="+caPath)
	cmd.Stdin = strings.NewReader("{\"event\":\"user\",\"message\":{\"content\":\"local fixture probe\"}}\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fake agy proxy probe: %v\n%s", err, output)
	}
	if server.FilteredGenerations() != 1 || f.dnsAttempts.Load() == 0 || !bytes.Contains(output, []byte(`"status": "SUCCESS"`)) {
		t.Fatalf("missing real filtering evidence: generations=%d dns=%d output=%s", server.FilteredGenerations(), f.dnsAttempts.Load(), output)
	}
}

// agyLifecycleGate gives a subprocess a deterministic ready/release handshake.
// Cancellation also releases its handler, so a killed CLI cannot strand cleanup.
func agyLifecycleGate(t *testing.T) (string, <-chan struct{}, func()) {
	t.Helper()
	ready := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { unblock(); server.Close() })
	return server.URL, ready, unblock
}

func (f *agyLifecycleFixture) calls(t *testing.T) []agyLifecycleCall {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatal(err)
	}
	var calls []agyLifecycleCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var call agyLifecycleCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}
