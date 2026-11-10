package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestCodeModeRequiresEngine(t *testing.T) {
	tool, _ := ToolSpec(CodeModeToolName)
	if tool.Name != CodeModeToolName {
		t.Fatal(tool)
	}
	manager, err := NewToolManager(&ToolConfig{Enabled: []string{CodeModeToolName}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ApprovalMgr.Close()
	executable, _ := manager.Registry.Get(CodeModeToolName)
	if _, err = executable.Execute(context.Background(), json.RawMessage(`{"code":"return 1"}`)); err == nil {
		t.Fatal("unadmitted code mode ran")
	}
	for _, name := range StandardToolNames() {
		if name == CodeModeToolName {
			t.Fatal("global default widened")
		}
	}
}
func TestCodeModeNativeReadWriteShellAndExplicitState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewToolManager(&ToolConfig{Enabled: []string{CodeModeToolName, ReadFileToolName, WriteFileToolName, ShellToolName}, ReadDirs: []string{dir}, WriteDirs: []string{dir}, BaseDir: dir, Workspace: "none", ShellAllow: []string{"printf *"}, ShellAutoRun: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ApprovalMgr.Close()
	recorder := &fakeFileRecorder{}
	manager.Registry.SetFileChangeRecorder(recorder)
	h := testutil.NewEngineHarness()
	manager.Registry.RegisterWithEngine(h.Engine)
	// Native results are honestly text envelopes, not guessed business objects.
	code := `const r=await tools.read_file({path:'note.txt'});text(r.text.includes('before'));await tools.write_file({path:'note.txt',content:'after'});const s=await tools.shell({command:'printf hello'});text(s.text.includes('hello'));store('n',7);`
	h.Provider.AddToolCall("cell-one", CodeModeToolName, map[string]any{"code": code}).AddToolCall("cell-two", CodeModeToolName, map[string]any{"code": `text(load('n'));store('n',8)`, "state": map[string]any{"n": 7}}).AddTextResponse("done")
	_, err = h.Run(context.Background(), core.Request{SessionID: "code-session", Messages: []core.Message{core.UserText("Use the owned fixture file and printf")}, Tools: manager.GetSpecs(), MaxTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "after" {
		t.Fatalf("file %q", b)
	}
	requests := h.Provider.RecordedRequests()
	if len(requests) != 3 {
		t.Fatal(len(requests))
	}
	var results []*core.ToolResult
	for _, m := range requests[2].Messages {
		for _, p := range m.Parts {
			if p.ToolResult != nil {
				results = append(results, p.ToolResult)
			}
		}
	}
	if len(results) != 2 || !strings.Contains(results[0].Content, `"state":{"n":7}`) || !strings.Contains(results[1].Content, `"state":{"n":8}`) {
		t.Fatalf("%+v", results)
	}
	if len(recorder.records) != 1 || recorder.records[0].ToolCallID == "cell-one" || recorder.records[0].ToolCallID == "" {
		t.Fatalf("child file attribution lost: %+v", recorder.records)
	}
	starts, ends := 0, 0
	for _, ev := range h.Events {
		if ev.ParentToolCallID == "cell-one" {
			if ev.Type == core.EventToolExecStart {
				starts++
			}
			if ev.Type == core.EventToolExecEnd {
				ends++
				if ev.ToolOutput != "" {
					t.Fatal("intermediate body leaked")
				}
			}
		}
	}
	if starts != 3 || ends != 3 {
		t.Fatalf("child events %d/%d", starts, ends)
	}
}
func TestCodeModeGuardianDenialDoesNotCreateTrustedUserTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewToolManager(&ToolConfig{Enabled: []string{CodeModeToolName, ReadFileToolName}, Workspace: "none"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ApprovalMgr.Close()
	manager.ApprovalMgr.SetApprovalMode(ModeAuto)
	manager.ApprovalMgr.SetAutoHeadless(true)
	var reviewRequest PolicyReviewRequest
	manager.ApprovalMgr.SetPolicyReviewFunc(func(_ context.Context, r PolicyReviewRequest) (PolicyDecision, error) {
		reviewRequest = r
		return PolicyDecision{Allowed: false, Rationale: "fixture denied", Model: "guardian-fixture"}, nil
	}, nil)
	h := testutil.NewEngineHarness()
	manager.Registry.RegisterWithEngine(h.Engine)
	h.Provider.AddToolCall("denied-cell", CodeModeToolName, map[string]any{"code": `text((await tools.read_file({path:` + quoteJSON(path) + `})).isError);`}).AddTextResponse("done")
	if _, err = h.Run(context.Background(), core.Request{Messages: []core.Message{core.UserText("Only review the fixture; do not invent authorization")}, Tools: manager.GetSpecs(), MaxTurns: 3}); err != nil {
		t.Fatal(err)
	}
	if reviewRequest.Path != path {
		t.Fatalf("Guardian not used: %+v", reviewRequest)
	}
	request := h.Provider.RecordedRequests()[1]
	for _, m := range request.Messages {
		for _, p := range m.Parts {
			if p.ToolResult != nil {
				if strings.Contains(p.ToolResult.Content, "SECRET") {
					t.Fatal("denied data leaked")
				}
				if strings.Contains(p.ToolResult.Content, "guardian-fixture") {
					t.Fatal("private Guardian review leaked into provider content")
				}
				audit, _ := json.Marshal(p.ToolResult.ChildCalls)
				if !strings.Contains(string(audit), "guardian-fixture") {
					t.Fatalf("private child Guardian audit lost: %s", audit)
				}
			}
		}
	}
}
func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
func TestCodeModeModuleAdmissionOriginAndDependencies(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.mjs"), []byte(`export const x=1`), 0600); err != nil {
		t.Fatal(err)
	}
	activation := &skills.Activation{BaseDir: dir, Skill: &skills.Skill{Name: "probe", Source: skills.SourceLocal, CodeMode: &skills.CodeModeManifest{Module: "index.mjs", Files: []string{"index.mjs"}, Requires: skills.CodeModeRequires{Tools: []string{ReadFileToolName}}}}}
	cfg := &config.Config{}
	manager, err := NewToolManager(&ToolConfig{Enabled: []string{CodeModeToolName, ReadFileToolName}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ApprovalMgr.Close()
	h := testutil.NewEngineHarness()
	manager.Registry.RegisterWithEngine(h.Engine)
	if err = AdmitSkillCodeModule(h.Engine, manager, activation); err == nil {
		t.Fatal("project module admitted by default")
	}
	manager.ApprovalMgr.SetApprovalMode(ModeYolo)
	if err = AdmitSkillCodeModule(h.Engine, manager, activation); err != nil {
		t.Fatal(err)
	}
	manager.ApprovalMgr.SetApprovalMode(ModePrompt)
	h.Provider.AddToolCall("revoked", CodeModeToolName, map[string]any{"code": `await import('skill:probe')`}).AddTextResponse("done")
	if _, err = h.Run(context.Background(), core.Request{Messages: []core.Message{core.UserText("probe")}, Tools: manager.GetSpecs(), MaxTurns: 3}); err != nil {
		t.Fatal(err)
	}
	for _, m := range h.Provider.RecordedRequests()[1].Messages {
		for _, p := range m.Parts {
			if p.ToolResult != nil && !strings.Contains(p.ToolResult.Content, "revoked") {
				t.Fatalf("revoked yolo imported: %s", p.ToolResult.Content)
			}
		}
	}
	activation.Skill.UserInstalled = true
	if err = AdmitSkillCodeModule(h.Engine, manager, activation); err != nil {
		t.Fatal(err)
	}
	h.Engine.SetAllowedToolsFilter([]string{CodeModeToolName})
	if err = AdmitSkillCodeModule(h.Engine, manager, activation); err == nil {
		t.Fatal("requires granted denied tool")
	}
}
func TestCodeModeBuiltinAgentsResolveAlongsideDirectTools(t *testing.T) {
	r, err := agents.NewRegistry(agents.RegistryConfig{UseBuiltin: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"developer", "sysadmin"} {
		a, err := r.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		enabled := map[string]bool{}
		for _, n := range a.Tools.Enabled {
			enabled[n] = true
		}
		for _, n := range []string{CodeExecToolName, ReadFileToolName, ShellToolName} {
			if !enabled[n] {
				t.Errorf("%s missing %s", name, n)
			}
		}
	}
	for _, name := range []string{"codebase", "web-researcher", "reviewer"} {
		a, err := r.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range a.Tools.Enabled {
			if IsCodeModeTool(n) {
				t.Errorf("%s default changed", name)
			}
		}
	}
}

func TestCodeModeStateSchemaAcceptsCallerKeysOnResponsesWire(t *testing.T) {
	spec, _ := ToolSpec(CodeModeToolName)
	wire := responses.BuildResponsesTools([]core.ToolSpec{spec})
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var schema []struct {
		Parameters struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"parameters"`
	}
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema[0].Parameters.Properties["state"]["additionalProperties"] != true {
		t.Fatalf("caller state keys disallowed by provider schema: %s", raw)
	}
}
