package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestCmdRunnerUnboundRemoteSettingsRejectAmbientCWD(t *testing.T) {
	runner := newCmdRunner(&config.Config{}, cmdRunnerOptions{}).(*cmdRunner)
	for _, platform := range []string{runpkg.PlatformWeb, runpkg.PlatformTelegram} {
		t.Run(platform, func(t *testing.T) {
			settings, err := runner.resolveSettings(&config.Config{}, nil, runpkg.Request{Platform: platform}, "")
			if err != nil {
				t.Fatal(err)
			}
			if settings.BaseDir != "" || settings.ShellWorkingDir != "" || !settings.RequireExplicitWorkingDir || settings.PrimaryWorkspace != "" {
				t.Fatalf("unbound remote settings = %#v", settings)
			}
		})
	}

	bound := t.TempDir()
	settings, err := runner.resolveSettings(&config.Config{}, nil, runpkg.Request{Platform: runpkg.PlatformWeb, Cwd: bound}, "")
	if err != nil {
		t.Fatal(err)
	}
	if settings.BaseDir != bound || settings.ShellWorkingDir != bound || settings.PrimaryWorkspace != bound || settings.RequireExplicitWorkingDir {
		t.Fatalf("explicitly bound web settings = %#v", settings)
	}
	local, err := runner.resolveSettings(&config.Config{}, nil, runpkg.Request{Platform: runpkg.PlatformConsole}, "")
	if err != nil {
		t.Fatal(err)
	}
	if local.BaseDir == "" || local.ShellWorkingDir == "" || local.PrimaryWorkspace == "" || local.RequireExplicitWorkingDir {
		t.Fatalf("local settings lost cwd compatibility = %#v", local)
	}
}

func TestCmdRunnerResolveSettingsUsesRequestWorkingDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for project-instruction discovery")
	}
	workingDir := t.TempDir()
	if err := exec.Command("git", "init", "-q", workingDir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	const marker = "instructions-from-current-worktree"
	if err := os.WriteFile(filepath.Join(workingDir, "AGENTS.md"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	runner := newCmdRunner(&config.Config{}, cmdRunnerOptions{}).(*cmdRunner)
	settings, err := runner.resolveSettings(&config.Config{}, &agents.Agent{
		Name:         "reviewer",
		SystemPrompt: "review system prompt",
		AgentsMd:     "true",
	}, runpkg.Request{Platform: runpkg.PlatformConsole, Cwd: workingDir}, "")
	if err != nil {
		t.Fatalf("resolveSettings: %v", err)
	}
	if !strings.Contains(settings.SystemPrompt, marker) {
		t.Fatalf("SystemPrompt does not contain request-directory instructions %q: %q", marker, settings.SystemPrompt)
	}
	if settings.BaseDir != workingDir || settings.ShellWorkingDir != workingDir {
		t.Fatalf("BaseDir/ShellWorkingDir = %q/%q, want %q", settings.BaseDir, settings.ShellWorkingDir, workingDir)
	}
	if settings.PrimaryWorkspace != workingDir {
		t.Fatalf("PrimaryWorkspace = %q, want request directory %q", settings.PrimaryWorkspace, workingDir)
	}
	if len(settings.ReadDirs) != 0 || len(settings.WriteDirs) != 0 {
		t.Fatalf("ReadDirs/WriteDirs = %#v/%#v, primary must remain separate from static allowlists", settings.ReadDirs, settings.WriteDirs)
	}
}

func TestCmdRunnerPrepareUsesRequestWorkingDirForSkills(t *testing.T) {
	workingDir := t.TempDir()
	skillDir := filepath.Join(workingDir, ".skills", "worktree-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: worktree-skill\ndescription: Skill from current worktree\n---\nInstructions.\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
		Skills: config.SkillsConfig{
			Enabled:              true,
			AutoInvoke:           true,
			IncludeProjectSkills: true,
			MetadataBudgetTokens: 1000,
			MaxVisibleSkills:     10,
		},
	}
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		Messages:         []llm.Message{llm.UserText("hello")},
		ProviderInstance: llm.NewMockProvider("mock"),
		Cwd:              workingDir,
		DeferSession:     true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer env.Close()
	if !strings.Contains(env.settings.SystemPrompt, "worktree-skill") {
		t.Fatalf("SystemPrompt does not contain request-directory skill: %q", env.settings.SystemPrompt)
	}
}

func TestCmdRunnerPreparePropagatesWorkingDirToLLMRequest(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
	}
	provider := llm.NewMockProvider("mock")
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
	workingDir := t.TempDir()

	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		Messages:         []llm.Message{llm.UserText("hello")},
		ProviderInstance: provider,
		Cwd:              workingDir,
		DeferSession:     true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer env.Close()

	if env.llmReq.WorkingDir != workingDir {
		t.Fatalf("llm request WorkingDir = %q, want %q", env.llmReq.WorkingDir, workingDir)
	}
}

func TestCmdRunnerEnsureRunSessionUsesExplicitPrimaryWorkspace(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	runner := newCmdRunner(&config.Config{}, cmdRunnerOptions{}).(*cmdRunner)
	store := newServeRuntimeTestStore()
	workingDir := t.TempDir()

	sess, err := runner.ensureRunSession(
		context.Background(),
		store,
		runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: "working-dir-session"},
		provider,
		"mock",
		"mock-model",
		"",
		SessionSettings{BaseDir: workingDir, PrimaryWorkspace: workingDir},
	)
	if err != nil {
		t.Fatalf("ensureRunSession: %v", err)
	}
	if sess == nil {
		t.Fatal("ensureRunSession returned nil")
	}
	if sess.CWD != workingDir {
		t.Fatalf("session CWD = %q, want %q", sess.CWD, workingDir)
	}
}

type cmdRunnerCreateFailureStore struct {
	session.Store
	createErr        error
	concurrentCreate *session.Session
}

func (s *cmdRunnerCreateFailureStore) Create(ctx context.Context, _ *session.Session) error {
	if s.concurrentCreate != nil {
		_ = s.Store.Create(ctx, s.concurrentCreate)
	}
	return s.createErr
}

func TestCmdRunnerPersistedSessionCreateFailure(t *testing.T) {
	createErr := errors.New("session database is read-only")
	tests := []struct {
		name             string
		concurrentCreate bool
		wantErr          bool
	}{
		{name: "unrecovered failure", wantErr: true},
		{name: "concurrent create is recovered", concurrentCreate: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const sessionID = "persisted-job-session"
			baseStore := newServeRuntimeTestStore()
			store := &cmdRunnerCreateFailureStore{Store: baseStore, createErr: createErr}
			if tc.concurrentCreate {
				store.concurrentCreate = &session.Session{
					ID:       sessionID,
					Provider: "mock",
					Model:    "mock-model",
					Status:   session.StatusActive,
				}
			}
			provider := llm.NewMockProvider("mock").AddTextResponse("ok")
			cfg := &config.Config{
				DefaultProvider: "mock",
				Providers: map[string]config.ProviderConfig{
					"mock": {Model: "mock-model"},
				},
			}
			runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store}).(*cmdRunner)
			includeConfiguredTools := false

			result, err := runner.Run(context.Background(), runpkg.Request{
				Platform:               runpkg.PlatformJob,
				Prompt:                 "run the job",
				SessionID:              sessionID,
				Persist:                true,
				ProviderInstance:       provider,
				Cwd:                    t.TempDir(),
				IncludeConfiguredTools: &includeConfiguredTools,
			}, nil)

			if tc.wantErr {
				if !errors.Is(err, createErr) {
					t.Fatalf("Run() error = %v, want wrapped %v", err, createErr)
				}
				if requests := provider.RecordedRequests(); len(requests) != 0 {
					t.Fatalf("provider requests = %d, want 0", len(requests))
				}
				if result.SessionID != "" || result.Provider != "" || result.Model != "" || result.Response != "" || result.Engine != nil || result.ProviderInstance != nil || result.Progressive != nil {
					t.Fatalf("Run() result = %+v, want zero result", result)
				}
				return
			}

			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if requests := provider.RecordedRequests(); len(requests) != 1 {
				t.Fatalf("provider requests = %d, want 1", len(requests))
			}
			if result.Response != "ok" {
				t.Fatalf("Run() response = %q, want %q", result.Response, "ok")
			}
		})
	}
}

func TestCmdRunnerPrepareUsesBorrowedEngineProvider(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
	}
	provider := llm.NewMockProvider("mock")
	engine := newEngine(provider, cfg)
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)

	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		Messages:         []llm.Message{llm.UserText("hello")},
		Engine:           engine,
		ProviderInstance: provider,
		DeferSession:     true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer env.Close()

	if env.engine != engine {
		t.Fatal("prepare did not reuse borrowed engine")
	}
	if env.provider != provider {
		t.Fatal("prepare did not reuse borrowed provider")
	}
	if env.runtime == nil || !env.runtime.skipProviderCleanup {
		t.Fatal("borrowed provider should skip runtime provider cleanup")
	}
	if !env.runtime.borrowedEngine {
		t.Fatal("borrowed engine should preserve provider conversation state")
	}
}

func TestCmdRunnerResumeRetainsHistoryAndRejectsMissingSource(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			store := newServeRuntimeTestStore()
			const sid = "resume-source"
			if exists {
				if err := store.Create(context.Background(), &session.Session{ID: sid, Provider: "mock", Model: "mock-model", Status: session.StatusActive}); err != nil {
					t.Fatal(err)
				}
				if err := store.AddMessage(context.Background(), sid, &session.Message{SessionID: sid, Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "original unfinished task"}}, TextContent: "original unfinished task", Sequence: -1}); err != nil {
					t.Fatal(err)
				}
			}
			provider := llm.NewMockProvider("mock").AddTextResponse("continued")
			cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
			runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store})
			noTools := false
			_, err := runner.Run(context.Background(), runpkg.Request{Platform: runpkg.PlatformJob, SessionID: sid, Resume: true, Persist: true, Messages: []llm.Message{{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "internal continuation"}}}}, ProviderInstance: provider, Cwd: t.TempDir(), IncludeConfiguredTools: &noTools}, nil)
			requests := provider.RecordedRequests()
			if !exists {
				if err == nil || len(requests) != 0 {
					t.Fatalf("missing resume source ran: err=%v calls=%d", err, len(requests))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 1 {
				t.Fatalf("provider calls=%d", len(requests))
			}
			found := false
			for _, msg := range requests[0].Messages {
				if strings.Contains(llm.MessageText(msg), "original unfinished task") {
					found = true
				}
			}
			if !found {
				t.Fatal("Resume dropped the original transcript")
			}
		})
	}
}

type runnerUncooperativeTool struct{ started, release chan struct{} }

func (t *runnerUncooperativeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "hold_cleanup", Description: "test actual execution lifetime", Schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}
}
func (t *runnerUncooperativeTool) Preview(json.RawMessage) string { return "" }
func (t *runnerUncooperativeTool) Execute(context.Context, json.RawMessage) (llm.ToolOutput, error) {
	close(t.started)
	<-t.release
	return llm.ToolOutput{}, nil
}

func TestCmdRunnerCompletionWaitsForActualToolExit(t *testing.T) {
	tool := &runnerUncooperativeTool{started: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(tool.release)
		}
	}()
	provider := llm.NewMockProvider("mock").AddToolCall("held", "hold_cleanup", map[string]any{}).AddTextResponse("done")
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	runner := newCmdRunner(cfg, cmdRunnerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	engineDone := make(chan struct{})
	noTools := false
	go func() {
		_, err := runner.Run(ctx, runpkg.Request{Platform: runpkg.PlatformJob, Prompt: "work", ProviderInstance: provider, Cwd: t.TempDir(), DeferSession: true, ExtraTools: []llm.ToolSpec{tool.Spec()}, IncludeConfiguredTools: &noTools, OnEngineReady: func(e *llm.Engine) { e.RegisterTool(tool) }, OnEngineDone: func(*llm.Engine) { close(engineDone) }}, nil)
		returned <- err
	}()
	select {
	case <-tool.started:
	case err := <-returned:
		t.Fatalf("runner returned before tool started: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("tool not started")
	}
	cancel()
	select {
	case <-engineDone:
		t.Fatal("engine completion preceded actual tool exit")
	case err := <-returned:
		t.Fatalf("job returned with Execute still running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(tool.release)
	released = true
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not finish after tool exit")
	}
	select {
	case <-engineDone:
	default:
		t.Fatal("completion hook missing")
	}
}

func TestCmdRunnerProgressiveResumeRetainsHistoryAndRejectsMissingSource(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			store := newServeRuntimeTestStore()
			const sid = "resume-source"
			if exists {
				if err := store.Create(context.Background(), &session.Session{ID: sid, Provider: "mock", Model: "mock-model", Status: session.StatusActive}); err != nil {
					t.Fatal(err)
				}
				if err := store.AddMessage(context.Background(), sid, &session.Message{SessionID: sid, Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "original unfinished task"}}, TextContent: "original unfinished task", Sequence: -1}); err != nil {
					t.Fatal(err)
				}
			}
			provider := llm.NewMockProvider("mock").AddTextResponse("continued")
			cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
			runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store})
			noTools := false
			_, err := runner.Run(context.Background(), runpkg.Request{Platform: runpkg.PlatformJob, SessionID: sid, Resume: true, Progressive: &runpkg.ProgressiveOptions{StopWhen: "done"}, Persist: true, Messages: []llm.Message{{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "internal continuation"}}}}, ProviderInstance: provider, Cwd: t.TempDir(), IncludeConfiguredTools: &noTools}, nil)
			requests := provider.RecordedRequests()
			if !exists {
				if err == nil || len(requests) != 0 {
					t.Fatalf("missing resume source ran: err=%v calls=%d", err, len(requests))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) < 1 {
				t.Fatalf("provider calls=%d", len(requests))
			}
			found := false
			for _, msg := range requests[0].Messages {
				if strings.Contains(llm.MessageText(msg), "original unfinished task") {
					found = true
				}
			}
			if !found {
				t.Fatal("Resume dropped the original transcript")
			}
		})
	}
}

type progressiveFailingMessageStore struct {
	session.Store
	role    llm.Role
	failure error
}

func (s *progressiveFailingMessageStore) AddMessage(ctx context.Context, id string, message *session.Message) error {
	if message.Role == s.role {
		return s.failure
	}
	return s.Store.AddMessage(ctx, id, message)
}

func TestCmdRunnerProgressivePersistenceFailureIsNotSuccess(t *testing.T) {
	for _, role := range []llm.Role{llm.RoleUser, llm.RoleAssistant} {
		t.Run(string(role), func(t *testing.T) {
			failure := errors.New("fixture transcript write failure")
			store := &progressiveFailingMessageStore{Store: newServeRuntimeTestStore(), role: role, failure: failure}
			provider := llm.NewMockProvider("mock").AddTextResponse("answer").AddTextResponse("final answer")
			cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
			runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store})
			noTools := false
			_, err := runner.Run(context.Background(), runpkg.Request{Platform: runpkg.PlatformJob, SessionID: "failure-source", Persist: true, Progressive: &runpkg.ProgressiveOptions{StopWhen: "done"}, Prompt: "original task", ProviderInstance: provider, Cwd: t.TempDir(), IncludeConfiguredTools: &noTools}, nil)
			if !errors.Is(err, failure) {
				t.Fatalf("persistence failure swallowed: %v", err)
			}
			if role == llm.RoleUser && len(provider.RecordedRequests()) != 0 {
				t.Fatal("provider invoked without durable original input")
			}
		})
	}
}

func TestProgressiveResumeInputKeepsChangedPolicyAndUserInput(t *testing.T) {
	history := []llm.Message{llm.SystemText("policy"), llm.SystemText("conversation summary"), llm.UserText("task")}
	input := []llm.Message{llm.SystemText("policy"), llm.SystemText("updated policy"), llm.UserText("task")}
	got := progressiveResumeInput(history, input)
	if len(got) != 2 || llm.MessageText(got[0]) != "updated policy" || got[1].Role != llm.RoleUser {
		t.Fatalf("incorrect resume deduplication: %+v", got)
	}
	if len(history) != 3 || len(input) != 3 {
		t.Fatal("mutated source transcript/input")
	}
}

func TestCmdRunnerPreservesRequestModelBoundary(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	provider := llm.NewMockProvider("mock")
	provider.AddTextResponse("must not execute")
	engine := newEngine(provider, cfg)
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
	boundaryErr := errors.New("parked boundary")
	calls := 0
	_, err := runner.Run(context.Background(), runpkg.Request{Platform: runpkg.PlatformTelegram, Cwd: t.TempDir(), Messages: []llm.Message{llm.UserText("work")}, Engine: engine, ProviderInstance: provider, DeferSession: true, DisableRuntimePersistence: true, ModelBoundary: func(context.Context) error { calls++; return boundaryErr }}, eventSinkFunc(nil))
	if calls != 1 || err == nil {
		t.Fatalf("boundary calls=%d err=%v", calls, err)
	}
	if len(provider.RecordedRequests()) != 0 {
		t.Fatal("boundary error still issued provider request")
	}
	calls = 0
	_, err = runner.Run(context.Background(), runpkg.Request{Platform: runpkg.PlatformWeb, Cwd: t.TempDir(), Messages: []llm.Message{llm.UserText("work")}, Engine: engine, ProviderInstance: provider, DeferSession: true, ModelBoundary: func(context.Context) error { calls++; return nil }}, eventSinkFunc(nil))
	if err == nil || calls != 0 || len(provider.RecordedRequests()) != 0 {
		t.Fatal("runtime-owned boundary accepted unpersisted input")
	}
}
