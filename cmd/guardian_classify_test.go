package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/typesafe"
	"github.com/samsaffron/term-llm/internal/ui"
)

type guardianClassifyStub struct {
	calls int
	state []byte
}

func (s *guardianClassifyStub) ListModels(context.Context) (*typesafe.ModelsResponse, error) {
	panic("not used")
}
func (s *guardianClassifyStub) Classify(ctx context.Context, req typesafe.Request) (*typesafe.Response, error) {
	s.calls++
	s.state = append([]byte(nil), req.State...)
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Second {
		panic("missing guardian deadline")
	}
	answers := map[string]typesafe.Answer{}
	for id, choice := range map[string]string{"risk_level": "low", "user_authorization": "explicit", "outcome": "allow"} {
		c, confidence := choice, 0.4
		answers[id] = typesafe.Answer{Type: "choice", Choice: &c, Confidence: &confidence}
	}
	return &typesafe.Response{Model: req.Model, Answers: answers}, nil
}

func TestGuardianClassifySetupAndConfidenceBreaker(t *testing.T) {
	stub := &guardianClassifyStub{}
	orig := newGuardianClassifyClient
	t.Cleanup(func() { newGuardianClassifyClient = orig })
	factories := 0
	newGuardianClassifyClient = func(opts typesafe.Options) (classifyClient, error) {
		factories++
		if opts.APIKey != "test-key" || opts.BaseURL != "http://localhost:1234" || opts.Timeout != 3*time.Second {
			t.Fatalf("unexpected options (key omitted): %s %v", opts.BaseURL, opts.Timeout)
		}
		return stub, nil
	}
	policyPath := filepath.Join(t.TempDir(), "policy.md")
	if err := os.WriteFile(policyPath, []byte("custom guardian policy"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Guardian: config.GuardianConfig{Backend: "classify", PolicyPath: policyPath, TimeoutSeconds: 2, Classify: config.GuardianClassifyConfig{MinConfidence: 0.5}}, Classify: config.ClassifyConfig{DefaultProvider: "alias", Providers: map[string]config.ClassifyProviderConfig{"alias": {Type: "typesafe", APIKey: "test-key", BaseURL: "http://localhost:1234", TimeoutSeconds: 3}}}}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	defer mgr.Close()
	if err := applyResolvedApprovalMode(cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err != nil {
		t.Fatal(err)
	}
	if factories != 1 || stub.calls != 0 {
		t.Fatal("setup must eagerly resolve without classifying")
	}
	path := filepath.Join(t.TempDir(), "unapproved")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	var events []tools.GuardianEvent
	mgr.GuardianEventFunc = func(e tools.GuardianEvent) { events = append(events, e) }
	for i := 0; i < 3; i++ {
		_, err := mgr.CheckPathApprovalWithContext(context.Background(), tools.ReadFileToolName, path, path, false)
		if err == nil || !strings.Contains(err.Error(), "confidence") {
			t.Fatalf("denial: %v", err)
		}
	}
	if mgr.ApprovalMode() != tools.ModePrompt || stub.calls != 3 {
		t.Fatalf("breaker not tripped: %s calls=%d", mgr.ApprovalMode(), stub.calls)
	}
	if len(events) < 3 || events[0].DurationMS <= 0 || events[0].StateBytes != len(stub.state) || events[0].Path != path {
		t.Fatalf("events: %+v", events)
	}
	var state map[string]any
	if err := json.Unmarshal(stub.state, &state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state["policy"].(string), "custom guardian policy") {
		t.Fatal("policy missing")
	}
}

func TestGuardianClassifySetupFailuresAndYolo(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	for _, tc := range []struct {
		name string
		cfg  config.Config
	}{
		{"unknown backend", config.Config{Guardian: config.GuardianConfig{Backend: "unknown"}}},
		{"missing key", config.Config{Guardian: config.GuardianConfig{Backend: "classify"}}},
		{"unknown provider", config.Config{Guardian: config.GuardianConfig{Backend: "classify", Classify: config.GuardianClassifyConfig{Provider: "missing"}}}},
		{"bad confidence", config.Config{Guardian: config.GuardianConfig{Backend: "classify", Classify: config.GuardianClassifyConfig{MinConfidence: 1.1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := tools.NewApprovalManager(nil)
			defer mgr.Close()
			if err := installGuardianReviewerCallbacks(&tc.cfg, mgr, true); err == nil || mgr.GuardianReviewerAvailable() {
				t.Fatalf("installation: %v", err)
			}
			if err := applyResolvedApprovalMode(&tc.cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{Headless: true}); err == nil {
				t.Fatal("headless accepted invalid setup")
			}
			var warnings bytes.Buffer
			if err := applyResolvedApprovalMode(&tc.cfg, mgr, resolvedApprovalMode{Mode: tools.ModeAuto}, approvalRuntimeOptions{WarningWriter: &warnings}); err != nil || mgr.ApprovalMode() != tools.ModePrompt || warnings.Len() == 0 {
				t.Fatalf("interactive: %v %s", err, warnings.String())
			}
			if err := applyResolvedApprovalMode(&tc.cfg, mgr, resolvedApprovalMode{Mode: tools.ModeYolo}, approvalRuntimeOptions{Headless: true}); err != nil || !mgr.YoloEnabled() {
				t.Fatalf("yolo: %v", err)
			}
		})
	}
}

func TestGuardianClassifyUnknownCost(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.AddGuardianUsageForModel("jev-latest", 100, 20, 0, 0)
	estimate, err := ui.EstimateSessionStatsCostDetailed(stats, "gpt-5.6-sol")
	if err != nil || estimate.Unpriced != 1 || estimate.Priced != 0 || estimate.CostUSD != 0 {
		t.Fatalf("%+v %v", estimate, err)
	}
	if _, err := ui.EstimateSessionStatsCost(stats, "gpt-5.6-sol"); err == nil {
		t.Fatal("unknown TypeSafe cost must not borrow chat price")
	}
}

func TestGuardianClassifyConfigRenderingAndCompletion(t *testing.T) {
	if got := formatDefaultValue(config.GetDefaults()["guardian.classify.min_confidence"]); got != "0.15" {
		t.Fatalf("default render: %s", got)
	}
	if !valueMatchesDefault("0.5", 0.5) {
		t.Fatal("float default comparison")
	}
	cfg := &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"alias": {Type: "typesafe"}}}}
	if got := capabilityConfigValueCompletions(cfg, "guardian.backend", ""); strings.Join(got, ",") != "llm,classify" {
		t.Fatalf("backend: %v", got)
	}
	if got := capabilityConfigValueCompletions(cfg, "guardian.classify.provider", "a"); strings.Join(got, ",") != "alias" {
		t.Fatalf("provider: %v", got)
	}
}

func TestGuardianClassifyWiringPreservesActions(t *testing.T) {
	stub := &guardianClassifyStub{}
	original := newGuardianClassifyClient
	newGuardianClassifyClient = func(typesafe.Options) (classifyClient, error) { return stub, nil }
	t.Cleanup(func() { newGuardianClassifyClient = original })
	cfg := &config.Config{Guardian: config.GuardianConfig{Backend: "classify", TimeoutSeconds: 2, Classify: config.GuardianClassifyConfig{MinConfidence: 0.5}}, Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"typesafe": {APIKey: "test-key"}}}}
	mgr := tools.NewApprovalManager(nil)
	defer mgr.Close()
	if err := installGuardianReviewerCallbacks(cfg, mgr, true); err != nil {
		t.Fatal(err)
	}
	for _, req := range []tools.PolicyReviewRequest{
		{Command: "echo exact", WorkDir: "/workspace", ApprovalScope: "shared_shell"},
		{ToolName: "read_file", Path: "/workspace/file"},
		{ToolName: "glob", Path: "/workspace", Selector: "/workspace/**/*.go", IsDirectory: true, IsWrite: true},
		{ToolName: "manage_workspace", Path: "/workspace", WorkspaceAccess: "read_write", Reason: "exact reason", ScopeID: "session-id"},
	} {
		req.ApprovalContext = "granted files"
		req.Transcript = []tools.TranscriptEntry{{Role: "parent_user", Text: "trusted task"}}
		d, err := mgr.ReviewPolicy(context.Background(), req)
		if err != nil || d.DurationMS <= 0 || d.StateBytes != len(stub.state) {
			t.Fatalf("%+v %v", d, err)
		}
		var state struct {
			Action          map[string]any   `json:"action"`
			ApprovalContext string           `json:"approval_context"`
			Transcript      []map[string]any `json:"transcript"`
		}
		if err := json.Unmarshal(stub.state, &state); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]any{"command": req.Command, "workdir": req.WorkDir, "tool": req.ToolName, "path": req.Path, "selector": req.Selector, "is_write": req.IsWrite, "is_directory": req.IsDirectory, "workspace_access": req.WorkspaceAccess, "reason": req.Reason, "scope_id": req.ScopeID, "approval_scope": req.ApprovalScope} {
			if state.Action[key] != want {
				t.Fatalf("%s = %v, want %v", key, state.Action[key], want)
			}
		}
		if state.ApprovalContext != req.ApprovalContext || len(state.Transcript) != 1 || state.Transcript[0]["role"] != "parent_user" {
			t.Fatalf("state: %s", stub.state)
		}
	}
}
