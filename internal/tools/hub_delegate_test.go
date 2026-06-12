package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

// fakeHub fakes the Hub delegation API the node tools talk to.
type fakeHub struct {
	mu         sync.Mutex
	server     *httptest.Server
	lastAuth   string
	lastNodeID string
	lastCreate map[string]any
	// statusSequence is returned by successive GETs (last entry repeats).
	statusSequence []string
	getCalls       int
	response       string
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{statusSequence: []string{"running"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/delegations", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuth = r.Header.Get("Authorization")
		f.lastNodeID = r.Header.Get("X-Term-LLM-Node-ID")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastCreate = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"delegation": hub.Delegation{
			ID: "dlg_test", OriginNode: "alpha", TargetNode: "beta", Status: hub.DelegationStatusRunning,
		}})
	})
	mux.HandleFunc("/api/delegations/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuth = r.Header.Get("Authorization")
		f.lastNodeID = r.Header.Get("X-Term-LLM-Node-ID")
		idx := f.getCalls
		if idx >= len(f.statusSequence) {
			idx = len(f.statusSequence) - 1
		}
		f.getCalls++
		status := f.statusSequence[idx]
		d := hub.Delegation{ID: "dlg_test", OriginNode: "alpha", TargetNode: "beta", Status: status}
		if hub.DelegationStatusTerminal(status) {
			d.Response = f.response
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"delegation": d})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeHub) client() *hubDelegationClient {
	return &hubDelegationClient{
		hubURL:     f.server.URL,
		nodeID:     "alpha",
		token:      "alpha-token",
		httpClient: f.server.Client(),
	}
}

func TestHubDelegateMissingConfig(t *testing.T) {
	t.Cleanup(resetHubDelegationForTest())

	out, err := NewHubDelegateTool().Execute(context.Background(),
		json.RawMessage(`{"target_node":"beta","prompt":"x"}`))
	if err != nil {
		t.Fatalf("Execute returned hard error: %v", err)
	}
	text := out.Content
	for _, needle := range []string{"TERM_LLM_HUB_URL", "TERM_LLM_HUB_NODE_ID", "TERM_LLM_HUB_TOKEN", "not configured"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("error should name %q: %s", needle, text)
		}
	}
}

func TestHubDelegateClientFromEnvCaptureScrubsToken(t *testing.T) {
	t.Cleanup(resetHubDelegationForTest())
	t.Setenv("TERM_LLM_HUB_URL", "http://127.0.0.1:8090/")
	t.Setenv("TERM_LLM_HUB_NODE_ID", "alpha")
	t.Setenv("TERM_LLM_HUB_TOKEN", "tkn-secret")

	captureHubDelegationEnv()

	c, err := newHubDelegationClient()
	if err != nil {
		t.Fatalf("newHubDelegationClient: %v", err)
	}
	if c.hubURL != "http://127.0.0.1:8090" || c.nodeID != "alpha" || c.token != "tkn-secret" {
		t.Fatalf("client = %+v", c)
	}
	// The credential must be gone from the process environment: every
	// shell/custom/widget/MCP subprocess inherits os.Environ().
	if got := os.Getenv("TERM_LLM_HUB_TOKEN"); got != "" {
		t.Fatalf("TERM_LLM_HUB_TOKEN still set after capture: %q", got)
	}
	for _, e := range os.Environ() {
		if strings.Contains(e, "tkn-secret") {
			t.Fatalf("token leaked into process environment: %s", e)
		}
	}
}

func TestConfigureHubDelegationStaysOutOfEnv(t *testing.T) {
	t.Cleanup(resetHubDelegationForTest())

	ConfigureHubDelegation("http://127.0.0.1:9999", "alpha", "flag-secret")
	c, err := newHubDelegationClient()
	if err != nil {
		t.Fatalf("newHubDelegationClient: %v", err)
	}
	if c.token != "flag-secret" {
		t.Fatalf("token = %q", c.token)
	}
	for _, e := range os.Environ() {
		if strings.Contains(e, "flag-secret") {
			t.Fatalf("flag-provided token leaked into process environment: %s", e)
		}
	}
	// Flag-provided config is re-derived on reload; no env handover needed.
	if env := HubDelegationEnviron(); env != nil {
		t.Fatalf("HubDelegationEnviron = %v, want nil for flag-provided token", env)
	}
}

func TestHubDelegationEnvironForReload(t *testing.T) {
	t.Cleanup(resetHubDelegationForTest())
	t.Setenv("TERM_LLM_HUB_URL", "http://127.0.0.1:8090")
	t.Setenv("TERM_LLM_HUB_NODE_ID", "alpha")
	t.Setenv("TERM_LLM_HUB_TOKEN", "env-secret")
	captureHubDelegationEnv()

	env := strings.Join(HubDelegationEnviron(), "\n")
	for _, needle := range []string{"TERM_LLM_HUB_TOKEN=env-secret", "TERM_LLM_HUB_URL=", "TERM_LLM_HUB_NODE_ID=alpha"} {
		if !strings.Contains(env, needle) {
			t.Fatalf("HubDelegationEnviron missing %q: %q", needle, env)
		}
	}
}

func TestHubDelegateCreate(t *testing.T) {
	f := newFakeHub(t)
	tool := NewHubDelegateToolWithClient(f.client())

	out, err := tool.Execute(context.Background(), json.RawMessage(
		`{"target_node":"beta","prompt":"do it","agent_name":"developer","timeout_seconds":120,"cwd":"/work/sub","parent_delegation_id":"dlg_parent"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result HubDelegationResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatalf("decode output %q: %v", out.Content, err)
	}
	if result.DelegationID != "dlg_test" || result.Status != hub.DelegationStatusRunning {
		t.Fatalf("result = %+v", result)
	}
	if f.lastAuth != "Bearer alpha-token" || f.lastNodeID != "alpha" {
		t.Fatalf("hub saw auth %q node %q", f.lastAuth, f.lastNodeID)
	}
	for key, want := range map[string]any{
		"target_node":          "beta",
		"prompt":               "do it",
		"agent_name":           "developer",
		"timeout_seconds":      float64(120),
		"cwd":                  "/work/sub",
		"parent_delegation_id": "dlg_parent",
	} {
		if got := f.lastCreate[key]; got != want {
			t.Fatalf("create payload %s = %v, want %v", key, got, want)
		}
	}
}

func TestHubDelegateParentFromContext(t *testing.T) {
	f := newFakeHub(t)
	tool := NewHubDelegateToolWithClient(f.client())

	// Inside a delegated job the runner stamps the context; the tool must
	// chain from it even when the model omits parent_delegation_id.
	ctx := WithHubDelegationID(context.Background(), "dlg_trusted")
	if _, err := tool.Execute(ctx, json.RawMessage(`{"target_node":"beta","prompt":"x"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := f.lastCreate["parent_delegation_id"]; got != "dlg_trusted" {
		t.Fatalf("parent_delegation_id = %v, want dlg_trusted", got)
	}

	// A model-provided parent id must not override the trusted context.
	if _, err := tool.Execute(ctx, json.RawMessage(`{"target_node":"beta","prompt":"x","parent_delegation_id":"dlg_spoofed"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := f.lastCreate["parent_delegation_id"]; got != "dlg_trusted" {
		t.Fatalf("parent_delegation_id = %v, want dlg_trusted (context wins)", got)
	}
}

func TestHubDelegateWait(t *testing.T) {
	f := newFakeHub(t)
	f.statusSequence = []string{"running", "running", "succeeded"}
	f.response = "remote result"
	tool := NewHubDelegateToolWithClient(f.client())
	tool.pollIntervalOverride = time.Millisecond

	out, err := tool.Execute(context.Background(), json.RawMessage(
		`{"target_node":"beta","prompt":"do it","wait":true}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result HubDelegationResult
	_ = json.Unmarshal([]byte(out.Content), &result)
	if result.Status != hub.DelegationStatusSucceeded || result.Response != "remote result" {
		t.Fatalf("result = %+v", result)
	}
	if f.getCalls < 3 {
		t.Fatalf("getCalls = %d, want >= 3", f.getCalls)
	}
}

func TestHubDelegateInvalidParams(t *testing.T) {
	tool := NewHubDelegateToolWithClient(newFakeHub(t).client())
	out, _ := tool.Execute(context.Background(), json.RawMessage(`{"prompt":"x"}`))
	if !strings.Contains(out.Content, "target_node is required") {
		t.Fatalf("output = %s", out.Content)
	}
	out, _ = tool.Execute(context.Background(), json.RawMessage(`{"target_node":"beta"}`))
	if !strings.Contains(out.Content, "prompt is required") {
		t.Fatalf("output = %s", out.Content)
	}
}

func TestHubCheckDelegationWaits(t *testing.T) {
	f := newFakeHub(t)
	f.statusSequence = []string{"running", "succeeded"}
	f.response = "done"
	tool := NewHubCheckDelegationToolWithClient(f.client())
	tool.pollIntervalOverride = time.Millisecond

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"delegation_ids":["dlg_test"]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp struct {
		Delegations []HubDelegationResult `json:"delegations"`
	}
	if err := json.Unmarshal([]byte(out.Content), &resp); err != nil {
		t.Fatalf("decode %q: %v", out.Content, err)
	}
	if len(resp.Delegations) != 1 || resp.Delegations[0].Status != hub.DelegationStatusSucceeded || resp.Delegations[0].Response != "done" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestHubCheckDelegationNoWait(t *testing.T) {
	f := newFakeHub(t)
	tool := NewHubCheckDelegationToolWithClient(f.client())

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"delegation_ids":["dlg_test"],"wait":false}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp struct {
		Delegations []HubDelegationResult `json:"delegations"`
	}
	_ = json.Unmarshal([]byte(out.Content), &resp)
	if len(resp.Delegations) != 1 || resp.Delegations[0].Status != hub.DelegationStatusRunning {
		t.Fatalf("resp = %+v", resp)
	}
	if f.getCalls != 1 {
		t.Fatalf("getCalls = %d, want 1", f.getCalls)
	}
}

func TestHubDelegationToolsRegistered(t *testing.T) {
	if !ValidToolName(HubDelegateToolName) || !ValidToolName(HubCheckDelegationToolName) {
		t.Fatalf("hub delegation tools should be valid tool names")
	}
	if GetToolKind(HubDelegateToolName) != KindAgent || GetToolKind(HubCheckDelegationToolName) != KindAgent {
		t.Fatalf("hub delegation tools should be KindAgent")
	}
}
