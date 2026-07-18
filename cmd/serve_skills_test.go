package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

type fakeServeSkillChildRunner struct {
	mu      sync.Mutex
	request runpkg.ChildRunRequest
}

func (r *fakeServeSkillChildRunner) RunChild(ctx context.Context, request runpkg.ChildRunRequest, callback runpkg.ChildRunEventCallback) (runpkg.ChildRunResult, error) {
	r.mu.Lock()
	r.request = request
	r.mu.Unlock()
	if callback != nil {
		callback(request.RunID, tools.SubagentEvent{Type: tools.SubagentEventText, Text: "working"})
	}
	return runpkg.ChildRunResult{RunID: request.RunID, ChildSessionID: "child-web", Output: "Web review complete", StartedAt: time.Now().Add(-time.Second), CompletedAt: time.Now()}, nil
}

func (r *fakeServeSkillChildRunner) requestSnapshot() runpkg.ChildRunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.request
}

func TestSessionMessageEntriesExposeSkillProvenance(t *testing.T) {
	srv := &serveServer{}
	provenance := &llm.SkillActivationProvenance{
		Name: "review", Source: "local", SourcePath: "/repo/.skills/review", Origin: "user",
		Execution: "isolated", RawArguments: "staged", Agent: "reviewer", RunID: "skill-1",
		ChildSessionID: "child-1", Status: "complete", StartedAt: time.Now().Add(-time.Second).Format(time.RFC3339Nano),
		CompletedAt: time.Now().Format(time.RFC3339Nano), ActivatedAt: time.Now().Add(-time.Second).Format(time.RFC3339Nano),
	}
	entries := srv.sessionMessageEntries([]session.Message{{
		ID: 1, Role: llm.RoleEvent, CreatedAt: time.Now(),
		Parts: []llm.Part{{Type: llm.PartSkillActivation, SkillActivation: provenance}, {Type: llm.PartText, Text: "result"}},
	}})
	if len(entries) != 1 || len(entries[0].Parts) != 2 {
		t.Fatalf("skill event entries = %#v", entries)
	}
	part := entries[0].Parts[0]
	if part.Type != "skill_activation" || part.SkillActivation == nil || part.SkillActivation.RunID != "skill-1" || part.SkillActivation.SourcePath != "/repo/.skills/review" {
		t.Fatalf("skill provenance part = %#v", part)
	}
}

func TestServeSessionSkillsListingVisibilityCollisionAndOwnership(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	store := newServeRuntimeTestStore()
	store.sessions["sess-a"] = &session.Session{ID: "sess-a", CWD: root}
	srv := &serveServer{store: store, skillsSetup: setup}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-a/skills", nil)
	req.Header.Set("session_id", "sess-a")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET skills status = %d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Skills []struct {
			Name                string `json:"name"`
			Execution           string `json:"execution"`
			CollidesWithBuiltin bool   `json:"collides_with_builtin"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	seen := map[string]struct {
		execution string
		collision bool
	}{}
	for _, item := range payload.Skills {
		seen[item.Name] = struct {
			execution string
			collision bool
		}{item.Execution, item.CollidesWithBuiltin}
	}
	if _, ok := seen["manual-review"]; !ok {
		t.Fatalf("manual-only skill missing from user listing: %#v", seen)
	}
	if _, ok := seen["model-only"]; ok {
		t.Fatalf("model-only skill leaked into user listing: %#v", seen)
	}
	if got := seen["forked"]; got.execution != "isolated" {
		t.Fatalf("forked execution = %#v", got)
	}
	if got := seen["compact"]; !got.collision {
		t.Fatalf("compact collision metadata = %#v", got)
	}
	if got := seen["h"]; !got.collision {
		t.Fatalf("help alias collision metadata = %#v", got)
	}

	foreign := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-a/skills", nil)
	foreign.Header.Set("session_id", "sess-b")
	foreignRR := httptest.NewRecorder()
	srv.handleSessionByID(foreignRR, foreign)
	if foreignRR.Code != http.StatusForbidden {
		t.Fatalf("foreign session listing status = %d, want 403", foreignRR.Code)
	}
	foreignInvoke := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-a/skills/invoke", strings.NewReader(`{"name":"default"}`))
	foreignInvoke.Header.Set("Content-Type", "application/json")
	foreignInvoke.Header.Set("session_id", "sess-b")
	foreignInvokeRR := httptest.NewRecorder()
	srv.handleSessionByID(foreignInvokeRR, foreignInvoke)
	if foreignInvokeRR.Code != http.StatusForbidden {
		t.Fatalf("foreign session invocation status = %d, want 403", foreignInvokeRR.Code)
	}
}

func TestServeSessionSkillsUseSessionProjectAndProjectPrecedence(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	writeSkill := func(baseDir, name, description string) {
		t.Helper()
		dir := filepath.Join(baseDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nBody\n", name, description)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(filepath.Join(configHome, "term-llm", "skills"), "shared", "User version")
	projectA := filepath.Join(root, "project-a")
	projectB := filepath.Join(root, "project-b")
	writeSkill(filepath.Join(projectA, ".skills"), "shared", "Project A version")
	writeSkill(filepath.Join(projectB, ".skills"), "only-b", "Project B skill")

	store := newServeRuntimeTestStore()
	store.sessions["sess-a"] = &session.Session{ID: "sess-a", CWD: projectA}
	store.sessions["sess-b"] = &session.Session{ID: "sess-b", CWD: projectB}
	skillsCfg := config.SkillsConfig{
		Enabled: true, AutoInvoke: true, IncludeProjectSkills: true, IncludeEcosystemPaths: false,
	}
	srv := &serveServer{
		store:       store,
		cfgRef:      &config.Config{Skills: skillsCfg},
		skillsSetup: SetupSkillsInDir(&skillsCfg, "", "", io.Discard, projectB),
	}
	list := func(sessionID string) map[string]struct{ Description, Source string } {
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID+"/skills", nil)
		req.Header.Set("session_id", sessionID)
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("list %s status=%d body=%s", sessionID, rr.Code, rr.Body.String())
		}
		var payload struct {
			Skills []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Source      string `json:"source"`
			} `json:"skills"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		result := make(map[string]struct{ Description, Source string })
		for _, item := range payload.Skills {
			result[item.Name] = struct{ Description, Source string }{item.Description, item.Source}
		}
		return result
	}

	a := list("sess-a")
	if got := a["shared"]; got.Description != "Project A version" || got.Source != "local" {
		t.Fatalf("project precedence listing = %#v", got)
	}
	b := list("sess-b")
	if _, ok := b["only-b"]; !ok {
		t.Fatalf("session B project skill missing: %#v", b)
	}
	if got := b["shared"]; got.Description != "User version" || got.Source != "user" {
		t.Fatalf("session B user fallback = %#v", got)
	}
}

func TestServeSessionSkillInvokeEnforcesPolicyAndStartsIsolatedRun(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	store := newServeRuntimeTestStore()
	store.sessions["sess-a"] = &session.Session{ID: "sess-a", CWD: root}
	runner := &fakeServeSkillChildRunner{}
	srv := &serveServer{
		store:       store,
		skillsSetup: setup,
		skillChildRunnerFactory: func(_ string, _ *serveRuntime) (runpkg.ChildRunner, error) {
			return runner, nil
		},
	}

	invoke := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-a/skills/invoke", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("session_id", "sess-a")
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		return rr
	}
	denied := invoke(`{"name":"model-only"}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("model-only invoke status = %d body=%s", denied.Code, denied.Body.String())
	}
	badArgs := invoke(`{"name":"forked","arguments":"\"unterminated"}`)
	if badArgs.Code != http.StatusBadRequest {
		t.Fatalf("malformed args status = %d body=%s", badArgs.Code, badArgs.Body.String())
	}

	started := invoke(`{"name":"forked","arguments":"internal/config"}`)
	if started.Code != http.StatusAccepted {
		t.Fatalf("isolated invoke status = %d body=%s", started.Code, started.Body.String())
	}
	var response struct {
		Execution      string `json:"execution"`
		RunID          string `json:"run_id"`
		ChildSessionID string `json:"child_session_id"`
		EventsURL      string `json:"events_url"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Execution != "isolated" || response.RunID == "" || response.EventsURL == "" {
		t.Fatalf("isolated response = %#v", response)
	}

	deadline := time.Now().Add(time.Second)
	for runner.requestSnapshot().RunID == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	childRequest := runner.requestSnapshot()
	if childRequest.Kind != runpkg.ChildRunIsolatedSkill || !strings.Contains(childRequest.Prompt, "Review internal/config.") || !strings.Contains(childRequest.Prompt, "# Skill: forked") || !strings.Contains(childRequest.Prompt, "**Description:** Forked review") || childRequest.ParentSessionID != "sess-a" {
		t.Fatalf("isolated child request = %#v", childRequest)
	}

	var eventsRR *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		eventsReq := httptest.NewRequest(http.MethodGet, response.EventsURL, nil)
		eventsReq.Header.Set("session_id", "sess-a")
		eventsRR = httptest.NewRecorder()
		srv.handleSessionByID(eventsRR, eventsReq)
		if strings.Contains(eventsRR.Body.String(), "Web review complete") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if eventsRR == nil || eventsRR.Code != http.StatusOK || !strings.Contains(eventsRR.Body.String(), "Web review complete") {
		t.Fatalf("skill run events status=%d body=%s", eventsRR.Code, eventsRR.Body.String())
	}

	foreignCancel := httptest.NewRequest(http.MethodDelete, response.EventsURL, nil)
	foreignCancel.Header.Set("session_id", "sess-b")
	foreignCancelRR := httptest.NewRecorder()
	srv.handleSessionByID(foreignCancelRR, foreignCancel)
	if foreignCancelRR.Code != http.StatusForbidden {
		t.Fatalf("foreign cancel status = %d, want 403", foreignCancelRR.Code)
	}
}

func TestServeSessionMainSkillStartsStructuredResponseAndRestoresToolPolicy(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	store := newServeRuntimeTestStore()
	store.sessions["sess-main"] = &session.Session{ID: "sess-main", CWD: root, Provider: "mock", Model: "mock-model"}
	provider := llm.NewMockProvider("mock").AddTextResponse("Main skill response")
	engine := llm.NewEngine(provider, nil)
	engine.RegisterTool(tools.NewReadFileTool(nil, tools.OutputLimits{}))
	runtime := &serveRuntime{provider: provider, providerKey: "mock", engine: engine, store: store, defaultModel: "mock-model"}
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) { return runtime, nil })
	defer manager.Close()
	srv := &serveServer{store: store, sessionMgr: manager, skillsSetup: setup}
	srv.ensureResponseRuns().setActiveRun("sess-main", "resp-existing")
	busyReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-main/skills/invoke", strings.NewReader(`{"name":"default"}`))
	busyReq.Header.Set("Content-Type", "application/json")
	busyReq.Header.Set("session_id", "sess-main")
	busyRR := httptest.NewRecorder()
	srv.handleSessionByID(busyRR, busyReq)
	if busyRR.Code != http.StatusConflict {
		t.Fatalf("main skill during active response status = %d, want 409; body=%s", busyRR.Code, busyRR.Body.String())
	}
	srv.ensureResponseRuns().clearActiveRun("sess-main", "resp-existing")

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-main/skills/invoke", strings.NewReader(`{"name":"default","arguments":"scope"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "sess-main")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("main invoke status = %d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		Execution  string `json:"execution"`
		ResponseID string `json:"response_id"`
		EventsURL  string `json:"events_url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Execution != "main" || response.ResponseID == "" || response.EventsURL == "" {
		t.Fatalf("main response = %#v", response)
	}

	deadline := time.Now().Add(2 * time.Second)
	var requests []llm.Request
	for len(requests) == 0 && time.Now().Before(deadline) {
		requests = provider.RecordedRequests()
		if len(requests) == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if len(requests) == 0 {
		t.Fatal("main skill response never reached provider")
	}
	if len(requests[0].Tools) != 0 {
		t.Fatalf("explicit-empty skill advertised tools to provider: %#v", requests[0].Tools)
	}
	var contextText strings.Builder
	for _, message := range requests[0].Messages {
		contextText.WriteString(llm.MessageText(message))
		contextText.WriteByte('\n')
	}
	for _, want := range []string{"Default body", "Invocation arguments", "scope", "/default scope"} {
		if !strings.Contains(contextText.String(), want) {
			t.Fatalf("provider context missing %q: %s", want, contextText.String())
		}
	}
	var provenance *llm.SkillActivationProvenance
	var persistedMessages []session.Message
	for provenance == nil && time.Now().Before(deadline) {
		persistedMessages, _ = store.GetMessages(context.Background(), "sess-main", 0, 0)
		for _, message := range persistedMessages {
			if message.Role != llm.RoleDeveloper {
				continue
			}
			for _, part := range message.Parts {
				if part.Type == llm.PartSkillActivation && part.SkillActivation != nil {
					provenance = part.SkillActivation
					break
				}
			}
		}
		if provenance == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if provenance == nil || provenance.Name != "default" || provenance.Origin != "user" || provenance.Execution != "main" || provenance.RawArguments != "scope" || provenance.Status != "running" {
		t.Fatalf("persisted main skill provenance = %#v", provenance)
	}
	activationCount := 0
	for _, message := range persistedMessages {
		for _, part := range message.Parts {
			if part.Type == llm.PartSkillActivation && part.SkillActivation != nil && part.SkillActivation.Name == "default" {
				activationCount++
			}
		}
	}
	if activationCount != 1 {
		t.Fatalf("persisted main skill activation count = %d, want 1; messages=%#v", activationCount, persistedMessages)
	}
	for !engine.IsToolAllowed(tools.ReadFileToolName) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !engine.IsToolAllowed(tools.ReadFileToolName) {
		t.Fatal("main skill explicit-empty tool filter was not restored after response")
	}
}

type blockingServeSkillChildRunner struct {
	started  chan struct{}
	finished chan struct{}
}

func (r *blockingServeSkillChildRunner) RunChild(ctx context.Context, request runpkg.ChildRunRequest, callback runpkg.ChildRunEventCallback) (runpkg.ChildRunResult, error) {
	close(r.started)
	if callback != nil {
		callback(request.RunID, tools.SubagentEvent{Type: tools.SubagentEventText, Text: "partial"})
	}
	<-ctx.Done()
	close(r.finished)
	return runpkg.ChildRunResult{RunID: request.RunID, ChildSessionID: request.ChildSessionID, Output: "partial", CompletedAt: time.Now()}, ctx.Err()
}

func TestServeSkillRunResultWaitsForParentTurnBoundary(t *testing.T) {
	setup, _ := serveSkillTestSetup(t)
	activation, err := skills.NewActivator(setup.Registry).Activate(skills.ActivationRequest{Name: "forked", Origin: skills.SkillActivationUser})
	if err != nil {
		t.Fatal(err)
	}
	store := newServeRuntimeTestStore()
	store.sessions["sess-boundary"] = &session.Session{ID: "sess-boundary"}
	srv := &serveServer{store: store, responseRuns: newServeResponseRunManager()}
	srv.responseRuns.setActiveRun("sess-boundary", "resp-parent")
	run := newServeSkillRun("skill-boundary", "sess-boundary", "child-boundary", activation, func() {})
	run.finish(runpkg.ChildRunResult{RunID: run.ID, ChildSessionID: run.ChildSessionID, Output: "review result", CompletedAt: time.Now()}, nil)

	done := make(chan struct{})
	go func() {
		srv.persistServeSkillRunResultAtBoundary(context.Background(), run, nil)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	messages, _ := store.GetMessages(context.Background(), "sess-boundary", 0, 0)
	if len(messages) != 0 {
		t.Fatalf("skill result bisected active parent turn: %#v", messages)
	}

	srv.responseRuns.clearActiveRun("sess-boundary", "resp-parent")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("skill result did not flush after parent turn")
	}
	messages, _ = store.GetMessages(context.Background(), "sess-boundary", 0, 0)
	if len(messages) != 2 || messages[0].Role != llm.RoleEvent || messages[1].Role != llm.RoleDeveloper {
		t.Fatalf("flushed skill result messages = %#v", messages)
	}
}

func TestServeSkillRunResultSerializesWithRuntimeAndUpdatesHistory(t *testing.T) {
	setup, _ := serveSkillTestSetup(t)
	activation, err := skills.NewActivator(setup.Registry).Activate(skills.ActivationRequest{Name: "forked", Origin: skills.SkillActivationUser})
	if err != nil {
		t.Fatal(err)
	}
	store := newServeRuntimeTestStore()
	store.sessions["sess-runtime-boundary"] = &session.Session{ID: "sess-runtime-boundary"}
	runtime := &serveRuntime{engine: llm.NewEngine(llm.NewMockProvider("mock"), nil), store: store, history: []llm.Message{llm.UserText("parent")}, historyPersisted: true}
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) { return runtime, nil })
	defer manager.Close()
	if _, err := manager.GetOrCreate(context.Background(), "sess-runtime-boundary"); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store, sessionMgr: manager}
	run := newServeSkillRun("skill-runtime-boundary", "sess-runtime-boundary", "child-runtime", activation, func() {})
	run.finish(runpkg.ChildRunResult{RunID: run.ID, ChildSessionID: run.ChildSessionID, Output: "runtime result", CompletedAt: time.Now()}, nil)

	runtime.mu.Lock()
	done := make(chan struct{})
	go func() {
		srv.persistServeSkillRunResultAtBoundary(context.Background(), run, nil)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	messages, _ := store.GetMessages(context.Background(), run.SessionID, 0, 0)
	if len(messages) != 0 {
		t.Fatalf("skill result persisted while runtime turn lock held: %#v", messages)
	}
	runtime.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("skill result did not persist after runtime boundary")
	}

	messages, _ = store.GetMessages(context.Background(), run.SessionID, 0, 0)
	if len(messages) != 2 {
		t.Fatalf("runtime boundary messages = %#v", messages)
	}
	runtime.mu.Lock()
	history := append([]llm.Message(nil), runtime.history...)
	runtime.mu.Unlock()
	if len(history) != 2 || history[1].Role != llm.RoleDeveloper || !strings.Contains(llm.MessageText(history[1]), "runtime result") {
		t.Fatalf("runtime history after skill result = %#v", history)
	}
}

func TestPersistServeSkillRunResultAtBoundaryStopsSafelyOnCancellation(t *testing.T) {
	setup, _ := serveSkillTestSetup(t)
	activation, err := skills.NewActivator(setup.Registry).Activate(skills.ActivationRequest{Name: "forked", Origin: skills.SkillActivationUser})
	if err != nil {
		t.Fatal(err)
	}
	store := newServeRuntimeTestStore()
	store.sessions["sess-shutdown-boundary"] = &session.Session{ID: "sess-shutdown-boundary"}
	srv := &serveServer{store: store, responseRuns: newServeResponseRunManager()}
	srv.responseRuns.setActiveRun("sess-shutdown-boundary", "resp-parent")
	run := newServeSkillRun("skill-shutdown-boundary", "sess-shutdown-boundary", "child-boundary", activation, func() {})
	run.finish(runpkg.ChildRunResult{RunID: run.ID, ChildSessionID: run.ChildSessionID, Output: "partial result", CompletedAt: time.Now()}, context.Canceled)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.persistServeSkillRunResultAtBoundary(ctx, run, context.Canceled)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled boundary persistence did not return")
	}
	messages, _ := store.GetMessages(context.Background(), run.SessionID, 0, 0)
	if len(messages) != 0 {
		t.Fatalf("cancelled boundary persistence raced active parent: %#v", messages)
	}
}

func TestServeSkillRunSubscriberOverflowReconnectsThroughReplay(t *testing.T) {
	run := newServeSkillRun("skill-overflow", "sess-overflow", "child-overflow", &skills.Activation{Skill: &skills.Skill{Name: "review"}}, func() {})
	_, subscriberID, events, terminal := run.subscribe(0)
	if subscriberID == 0 || terminal {
		t.Fatalf("subscription = id %d terminal %v", subscriberID, terminal)
	}
	for i := 0; i < 33; i++ {
		run.appendEvent("skill_run.progress", map[string]any{"index": i})
	}
	var delivered int
	for range events {
		delivered++
	}
	if delivered != 32 || !run.subscriberOverflowed(subscriberID) {
		t.Fatalf("overflow delivery = %d dropped=%v, want 32/true", delivered, run.subscriberOverflowed(subscriberID))
	}
	replay, replayID, _, terminal := run.subscribe(delivered)
	defer run.unsubscribe(replayID)
	if terminal || len(replay) != 1 || replay[0].Sequence != 33 {
		t.Fatalf("overflow replay = %#v terminal=%v", replay, terminal)
	}
}

func TestServeSkillRunEventHistoryIsBounded(t *testing.T) {
	run := newServeSkillRun("skill-history", "sess-history", "child-history", &skills.Activation{Skill: &skills.Skill{Name: "review"}}, func() {})
	total := serveSkillRunEventHistoryLimit*4 + 137
	for i := 0; i < total; i++ {
		run.appendEvent("skill_run.progress", map[string]any{"index": i})
	}

	snapshotEvents, ok := run.snapshot()["events"].([]serveSkillRunEvent)
	if !ok {
		t.Fatal("snapshot events have unexpected type")
	}
	if len(snapshotEvents) != serveSkillRunEventHistoryLimit {
		t.Fatalf("snapshot retained events = %d, want %d", len(snapshotEvents), serveSkillRunEventHistoryLimit)
	}
	firstSequence := total - serveSkillRunEventHistoryLimit + 1
	for i, event := range snapshotEvents {
		if want := firstSequence + i; event.Sequence != want {
			t.Fatalf("snapshot event %d sequence = %d, want %d", i, event.Sequence, want)
		}
	}

	replay, subscriberID, _, terminal := run.subscribe(0)
	defer run.unsubscribe(subscriberID)
	if terminal {
		t.Fatal("running skill unexpectedly returned a terminal subscription")
	}
	if len(replay) != serveSkillRunEventHistoryLimit {
		t.Fatalf("replayed events = %d, want %d", len(replay), serveSkillRunEventHistoryLimit)
	}
	for i, event := range replay {
		if want := firstSequence + i; event.Sequence != want {
			t.Fatalf("replay event %d sequence = %d, want %d", i, event.Sequence, want)
		}
	}
}

func BenchmarkServeSkillRunAppendEvents(b *testing.B) {
	activation := &skills.Activation{Skill: &skills.Skill{Name: "review"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		run := newServeSkillRun("skill-benchmark", "sess-benchmark", "child-benchmark", activation, func() {})
		for j := 0; j < 10_000; j++ {
			run.appendEvent("skill_run.progress", nil)
		}
	}
}

func TestServeSkillRunStreamAlwaysDeliversCompletion(t *testing.T) {
	for i := 0; i < 100; i++ {
		run := newServeSkillRun(fmt.Sprintf("skill-stream-%d", i), "sess-stream", "child-stream", &skills.Activation{Skill: &skills.Skill{Name: "review"}}, func() {})
		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		rr := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			(&serveServer{}).streamServeSkillRunEvents(rr, req, run)
			close(done)
		}()
		deadline := time.Now().Add(time.Second)
		for {
			run.mu.Lock()
			subscribed := len(run.subscribers) == 1
			run.mu.Unlock()
			if subscribed || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		run.finish(runpkg.ChildRunResult{Output: "done", CompletedAt: time.Now()}, nil)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("skill event stream did not finish")
		}
		if !strings.Contains(rr.Body.String(), "skill_run.completed") {
			t.Fatalf("iteration %d stream omitted completion event: %s", i, rr.Body.String())
		}
	}
}

func TestServeSkillRunDeleteRejectsEventsSubpath(t *testing.T) {
	run := newServeSkillRun("skill-route", "sess-route", "child-route", &skills.Activation{Skill: &skills.Skill{Name: "review"}}, func() {})
	srv := &serveServer{skillRuns: map[string]*serveSkillRun{run.ID: run}}
	req := httptest.NewRequest(http.MethodDelete, "/v1/sessions/sess-route/skill-runs/skill-route/events", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionSkillRun(rr, req, "sess-route", "skill-runs/skill-route/events")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE events status = %d, want 405", rr.Code)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.Status != "running" {
		t.Fatalf("DELETE events changed run status to %q", run.Status)
	}
}

func TestServeSkillRunCompletedRetentionCleanup(t *testing.T) {
	run := newServeSkillRun("skill-retained", "sess-retained", "child-retained", &skills.Activation{Skill: &skills.Skill{Name: "review"}}, func() {})
	run.finish(runpkg.ChildRunResult{CompletedAt: time.Now()}, nil)
	srv := &serveServer{
		skillRuns:         map[string]*serveSkillRun{run.ID: run},
		skillRunRetention: 5 * time.Millisecond,
	}
	srv.scheduleServeSkillRunCleanup(run)
	deadline := time.Now().Add(time.Second)
	for srv.getServeSkillRun(run.ID) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.getServeSkillRun(run.ID) != nil {
		t.Fatal("completed skill run was not evicted after retention period")
	}
}

func TestSkillsForServeSessionCachesSetupByDirectory(t *testing.T) {
	_, root := serveSkillTestSetup(t)
	cfg := &config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      50,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	}
	srv := &serveServer{skillsConfig: cfg}
	first := srv.skillsForServeSession(&session.Session{ID: "first", CWD: root})
	second := srv.skillsForServeSession(&session.Session{ID: "second", CWD: root})
	if first == nil || first != second {
		t.Fatalf("same-directory setup cache = %p/%p", first, second)
	}
	srv.skillsCacheMu.Lock()
	entry := srv.skillsByDir[filepath.Clean(root)]
	entry.discovered = time.Now().Add(-serveSkillsCacheTTL)
	srv.skillsByDir[filepath.Clean(root)] = entry
	srv.skillsCacheMu.Unlock()
	refreshed := srv.skillsForServeSession(&session.Session{ID: "refreshed", CWD: root})
	if refreshed == nil || refreshed == first {
		t.Fatalf("expired setup cache was not refreshed: %p/%p", first, refreshed)
	}
	otherRoot := t.TempDir()
	otherSkillDir := filepath.Join(otherRoot, ".skills", "other")
	if err := os.MkdirAll(otherSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherSkillDir, "SKILL.md"), []byte("---\nname: other\ndescription: Other skill\n---\nOther body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := srv.skillsForServeSession(&session.Session{ID: "other", CWD: otherRoot})
	if other == nil || other == first {
		t.Fatalf("different-directory setup cache = %p/%p", first, other)
	}
}

func TestServeSkillRunCancelPreservesPartialOutputAndReplaysEvents(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	store := newServeRuntimeTestStore()
	store.sessions["sess-cancel"] = &session.Session{ID: "sess-cancel", CWD: root}
	runner := &blockingServeSkillChildRunner{started: make(chan struct{}), finished: make(chan struct{})}
	srv := &serveServer{
		store:       store,
		skillsSetup: setup,
		skillChildRunnerFactory: func(_ string, _ *serveRuntime) (runpkg.ChildRunner, error) {
			return runner, nil
		},
	}

	invokeReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-cancel/skills/invoke", strings.NewReader(`{"name":"forked"}`))
	invokeReq.Header.Set("Content-Type", "application/json")
	invokeReq.Header.Set("session_id", "sess-cancel")
	invokeRR := httptest.NewRecorder()
	srv.handleSessionByID(invokeRR, invokeReq)
	if invokeRR.Code != http.StatusAccepted {
		t.Fatalf("invoke status = %d body=%s", invokeRR.Code, invokeRR.Body.String())
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(invokeRR.Body.Bytes(), &started); err != nil || started.RunID == "" {
		t.Fatalf("decode invocation: run=%#v err=%v", started, err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("child run did not start")
	}

	baseURL := "/v1/sessions/sess-cancel/skill-runs/" + started.RunID
	foreignReq := httptest.NewRequest(http.MethodGet, baseURL, nil)
	foreignReq.Header.Set("session_id", "other-session")
	foreignRR := httptest.NewRecorder()
	srv.handleSessionByID(foreignRR, foreignReq)
	if foreignRR.Code != http.StatusForbidden {
		t.Fatalf("foreign run snapshot status = %d, want 403", foreignRR.Code)
	}

	cancelReq := httptest.NewRequest(http.MethodDelete, baseURL, nil)
	cancelReq.Header.Set("session_id", "sess-cancel")
	cancelRR := httptest.NewRecorder()
	srv.handleSessionByID(cancelRR, cancelReq)
	if cancelRR.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d body=%s", cancelRR.Code, cancelRR.Body.String())
	}
	select {
	case <-runner.finished:
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach child runner")
	}

	deadline := time.Now().Add(time.Second)
	var snapshotRR *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		snapshotReq := httptest.NewRequest(http.MethodGet, baseURL, nil)
		snapshotReq.Header.Set("session_id", "sess-cancel")
		snapshotRR = httptest.NewRecorder()
		srv.handleSessionByID(snapshotRR, snapshotReq)
		if strings.Contains(snapshotRR.Body.String(), `"status":"cancelled"`) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if snapshotRR == nil || !strings.Contains(snapshotRR.Body.String(), `"output":"partial"`) {
		t.Fatalf("cancelled snapshot did not preserve output: %s", snapshotRR.Body.String())
	}

	replayReq := httptest.NewRequest(http.MethodGet, baseURL+"/events?after=1", nil)
	replayReq.Header.Set("session_id", "sess-cancel")
	replayReq.Header.Set("Accept", "text/event-stream")
	replayRR := httptest.NewRecorder()
	srv.handleSessionByID(replayRR, replayReq)
	body := replayRR.Body.String()
	if strings.Contains(body, "skill_run.created") || !strings.Contains(body, "skill_run.progress") || !strings.Contains(body, "skill_run.completed") {
		t.Fatalf("event replay after sequence 1 = %s", body)
	}
}

func TestServeSkillRunsCancelAndDrainOnShutdown(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	store := newServeRuntimeTestStore()
	store.sessions["sess-stop"] = &session.Session{ID: "sess-stop", CWD: root}
	runner := &blockingServeSkillChildRunner{started: make(chan struct{}), finished: make(chan struct{})}
	srv := &serveServer{
		store:       store,
		skillsSetup: setup,
		skillChildRunnerFactory: func(_ string, _ *serveRuntime) (runpkg.ChildRunner, error) {
			return runner, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-stop/skills/invoke", strings.NewReader(`{"name":"forked"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "sess-stop")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("invoke status = %d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("child run did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.stopServeSkillRuns(ctx); err != nil {
		t.Fatalf("stop skill runs: %v", err)
	}
	select {
	case <-runner.finished:
	default:
		t.Fatal("shutdown returned before child runner finished")
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-stop/skills/invoke", strings.NewReader(`{"name":"forked"}`))
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("session_id", "sess-stop")
	second := httptest.NewRecorder()
	srv.handleSessionByID(second, secondReq)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("invoke after shutdown status = %d, want 503; body=%s", second.Code, second.Body.String())
	}
}

func serveSkillTestSetup(t *testing.T) (*skills.Setup, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	manifests := map[string]string{
		"default":       "---\nname: default\ndescription: Default skill\nallowed-tools: []\n---\nDefault body\n",
		"manual-review": "---\nname: manual-review\ndescription: Manual review\ndisable-model-invocation: true\n---\nManual body\n",
		"model-only":    "---\nname: model-only\ndescription: Model only\nuser-invocable: false\n---\nModel body\n",
		"forked":        "---\nname: forked\ndescription: Forked review\ncontext: fork\nagent: reviewer\n---\nReview $ARGUMENTS.\n",
		"compact":       "---\nname: compact\ndescription: Collision\n---\nCompact skill\n",
		"grep-only":     "---\nname: grep-only\ndescription: Grep restriction\nallowed-tools: grep\n---\nGrep skill\n",
		"read-only":     "---\nname: read-only\ndescription: Read restriction\nallowed-tools: read_file\n---\nRead skill\n",
		"h":             "---\nname: h\ndescription: Alias collision\n---\nHelp alias skill\n",
	}
	for name, manifest := range manifests {
		dir := filepath.Join(root, ".skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setup, err := skills.NewSetupWithOptions(&config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      50,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	}, skills.SetupOptions{ProjectDir: root})
	if err != nil || setup == nil {
		t.Fatalf("skill setup = %#v err=%v", setup, err)
	}
	return setup, root
}

var _ llm.Provider = (*llm.MockProvider)(nil)
