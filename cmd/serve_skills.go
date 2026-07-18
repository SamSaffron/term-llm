package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
	chatui "github.com/samsaffron/term-llm/internal/tui/chat"
)

type serveSkillRunEvent struct {
	Sequence int       `json:"sequence"`
	Type     string    `json:"type"`
	Data     any       `json:"data,omitempty"`
	At       time.Time `json:"at"`
}

const (
	serveSkillRunEventHistoryLimit = 1024
	serveSkillsCacheTTL            = 5 * time.Second
)

type serveSkillsCacheEntry struct {
	setup      *skills.Setup
	discovered time.Time
}

type serveSkillRun struct {
	mu                sync.Mutex
	ID                string
	SessionID         string
	ChildSessionID    string
	SkillName         string
	Agent             string
	Status            string
	Output            string
	StartedAt         time.Time
	CompletedAt       time.Time
	Activation        *skills.Activation
	cancel            context.CancelFunc
	events            []serveSkillRunEvent
	eventHead         int
	eventCount        int
	nextSequence      int
	subscribers       map[int]chan serveSkillRunEvent
	subscriberDropped map[int]bool
	nextSubscriber    int
	cleanupTimer      *time.Timer
}

func newServeSkillRun(id, sessionID, childSessionID string, activation *skills.Activation, cancel context.CancelFunc) *serveSkillRun {
	return &serveSkillRun{
		ID:                id,
		SessionID:         sessionID,
		ChildSessionID:    childSessionID,
		SkillName:         activation.Skill.Name,
		Agent:             activation.Metadata.Agent,
		Status:            "running",
		StartedAt:         time.Now(),
		Activation:        activation,
		cancel:            cancel,
		subscribers:       make(map[int]chan serveSkillRunEvent),
		subscriberDropped: make(map[int]bool),
	}
}

func (run *serveSkillRun) appendEvent(eventType string, data any) {
	run.mu.Lock()
	run.appendEventLocked(eventType, data)
	run.mu.Unlock()
}

func (run *serveSkillRun) appendEventLocked(eventType string, data any) {
	run.nextSequence++
	event := serveSkillRunEvent{Sequence: run.nextSequence, Type: eventType, Data: data, At: time.Now().UTC()}
	if run.eventCount < serveSkillRunEventHistoryLimit {
		run.events = append(run.events, event)
		run.eventCount++
	} else {
		run.events[run.eventHead] = event
		run.eventHead = (run.eventHead + 1) % serveSkillRunEventHistoryLimit
	}
	for id, subscriber := range run.subscribers {
		select {
		case subscriber <- event:
		default:
			// Force the SSE handler to reconnect and replay from its last
			// acknowledged sequence rather than silently losing events.
			run.subscriberDropped[id] = true
			close(subscriber)
			delete(run.subscribers, id)
		}
	}
}

func (run *serveSkillRun) finish(result runpkg.ChildRunResult, runErr error) {
	run.mu.Lock()
	run.Output = result.Output
	if result.ChildSessionID != "" {
		run.ChildSessionID = result.ChildSessionID
	}
	run.CompletedAt = result.CompletedAt
	if run.CompletedAt.IsZero() {
		run.CompletedAt = time.Now()
	}
	switch {
	case errors.Is(runErr, context.Canceled):
		run.Status = "cancelled"
	case runErr != nil:
		run.Status = "failed"
	default:
		run.Status = "complete"
	}
	run.cancel = nil
	run.appendEventLocked("skill_run.completed", map[string]any{
		"status":           run.Status,
		"output":           run.Output,
		"child_session_id": run.ChildSessionID,
		"error":            errorString(runErr),
	})
	run.mu.Unlock()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (run *serveSkillRun) eventHistoryLocked() []serveSkillRunEvent {
	if run.eventCount == 0 {
		return nil
	}
	events := make([]serveSkillRunEvent, run.eventCount)
	copied := copy(events, run.events[run.eventHead:])
	copy(events[copied:], run.events[:run.eventHead])
	return events
}

func (run *serveSkillRun) snapshot() map[string]any {
	run.mu.Lock()
	defer run.mu.Unlock()
	events := run.eventHistoryLocked()
	return map[string]any{
		"id":               run.ID,
		"session_id":       run.SessionID,
		"child_session_id": run.ChildSessionID,
		"skill":            run.SkillName,
		"agent":            run.Agent,
		"status":           run.Status,
		"output":           run.Output,
		"started_at":       run.StartedAt,
		"completed_at":     run.CompletedAt,
		"events":           events,
	}
}

func (run *serveSkillRun) subscribe(after int) ([]serveSkillRunEvent, int, <-chan serveSkillRunEvent, bool) {
	run.mu.Lock()
	defer run.mu.Unlock()
	events := run.eventHistoryLocked()
	replay := events[:0]
	for _, event := range events {
		if event.Sequence > after {
			replay = append(replay, event)
		}
	}
	terminal := run.Status != "running" && run.Status != "cancelling"
	if terminal {
		return replay, 0, nil, true
	}
	run.nextSubscriber++
	id := run.nextSubscriber
	channel := make(chan serveSkillRunEvent, 32)
	run.subscribers[id] = channel
	return replay, id, channel, false
}

func (run *serveSkillRun) unsubscribe(id int) {
	if id == 0 {
		return
	}
	run.mu.Lock()
	delete(run.subscribers, id)
	delete(run.subscriberDropped, id)
	run.mu.Unlock()
}

func (run *serveSkillRun) subscriberOverflowed(id int) bool {
	if id == 0 {
		return false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.subscriberDropped[id]
}

func (s *serveServer) handleSessionSkills(w http.ResponseWriter, r *http.Request, sessionID, suffix string) {
	if !serveSkillSessionAuthorized(r, sessionID) {
		writeOpenAIError(w, http.StatusForbidden, "permission_error", "session_id header does not match the requested session")
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil || sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}

	switch {
	case suffix == "skills" && r.Method == http.MethodGet:
		s.handleSessionSkillsList(w, r, sess)
	case suffix == "skills/invoke" && r.Method == http.MethodPost:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionSkillInvoke(w, r, sess)
	case strings.HasPrefix(suffix, "skill-runs/"):
		s.handleSessionSkillRun(w, r, sessionID, suffix)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func serveSkillSessionAuthorized(r *http.Request, sessionID string) bool {
	// Serve currently trusts possession of a session ID. This header check only
	// prevents a request from accidentally targeting a different URL session;
	// it is not an authentication boundary.
	claimed := strings.TrimSpace(r.Header.Get("session_id"))
	return claimed != "" && claimed == sessionID
}

func (s *serveServer) skillsForServeSession(sess *session.Session) *skills.Setup {
	var skillsConfig *config.SkillsConfig
	if s.skillsConfig != nil {
		skillsConfig = s.skillsConfig
	} else if s.cfgRef != nil {
		skillsConfig = &s.cfgRef.Skills
	}
	if skillsConfig != nil && sess != nil {
		dir := strings.TrimSpace(sess.WorktreeDir)
		if dir == "" {
			dir = strings.TrimSpace(sess.CWD)
		}
		if dir == "" {
			// An empty directory resolves relative to the process working directory,
			// which is not a stable identity to share across sessions.
			return SetupSkillsInDir(skillsConfig, "", "", io.Discard, dir)
		}
		cacheKey := filepath.Clean(dir)
		now := time.Now()
		s.skillsCacheMu.Lock()
		cached, found := s.skillsByDir[cacheKey]
		if found && now.Sub(cached.discovered) >= serveSkillsCacheTTL {
			delete(s.skillsByDir, cacheKey)
			found = false
		}
		s.skillsCacheMu.Unlock()
		if found && cached.setup != nil {
			return cached.setup
		}

		// Discovery can touch multiple filesystem roots; do not serialize cold
		// scans for unrelated sessions under the cache mutex.
		setup := SetupSkillsInDir(skillsConfig, "", "", io.Discard, dir)
		if setup == nil {
			return nil
		}
		s.skillsCacheMu.Lock()
		defer s.skillsCacheMu.Unlock()
		if cached, found = s.skillsByDir[cacheKey]; found && time.Since(cached.discovered) < serveSkillsCacheTTL && cached.setup != nil {
			return cached.setup
		}
		if s.skillsByDir == nil {
			s.skillsByDir = make(map[string]serveSkillsCacheEntry)
		}
		if len(s.skillsByDir) >= 32 {
			var oldestKey string
			var oldestTime time.Time
			for existing, entry := range s.skillsByDir {
				if oldestKey == "" || entry.discovered.Before(oldestTime) {
					oldestKey = existing
					oldestTime = entry.discovered
				}
			}
			delete(s.skillsByDir, oldestKey)
		}
		s.skillsByDir[cacheKey] = serveSkillsCacheEntry{setup: setup, discovered: time.Now()}
		return setup
	}
	return s.skillsSetup
}

func (s *serveServer) handleSessionSkillsList(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	setup := s.skillsForServeSession(sess)
	if setup == nil || setup.Registry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"skills": []any{}, "runs": s.sessionSkillRunSnapshots(sess.ID)})
		return
	}
	catalog, diagnostics, err := setup.Registry.ListUserInvocable()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	items := make([]map[string]any, 0, len(catalog))
	builtinNames := serveBuiltinSlashNames()
	for _, skill := range catalog {
		metadata, metadataErr := skills.InvocationFor(skill)
		if metadataErr != nil {
			continue
		}
		items = append(items, map[string]any{
			"name":                  skill.Name,
			"description":           skill.Description,
			"argument_hint":         metadata.ArgumentHint,
			"execution":             metadata.Execution.String(),
			"source":                skill.Source.SourceName(),
			"collides_with_builtin": builtinNames[skill.Name],
		})
	}
	diagnosticItems := make([]map[string]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		diagnosticItems = append(diagnosticItems, map[string]string{"name": diagnostic.Name, "error": diagnostic.Err.Error()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": items, "diagnostics": diagnosticItems, "runs": s.sessionSkillRunSnapshots(sess.ID)})
}

func serveBuiltinSlashNames() map[string]bool {
	set := make(map[string]bool)
	for _, command := range chatui.AllCommands() {
		if name := strings.ToLower(strings.TrimSpace(command.Name)); name != "" {
			set[name] = true
		}
		for _, alias := range command.Aliases {
			if name := strings.ToLower(strings.TrimSpace(alias)); name != "" {
				set[name] = true
			}
		}
	}
	return set
}

type serveSkillInvokeRequest struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (s *serveServer) handleSessionSkillInvoke(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	var request serveSkillInvokeRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid skill invocation: "+err.Error())
		return
	}
	setup := s.skillsForServeSession(sess)
	if setup == nil || setup.Registry == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "skills are not enabled for this session")
		return
	}
	activation, err := skills.NewActivator(setup.Registry).Activate(skills.ActivationRequest{Name: request.Name, RawArgs: request.Arguments, Origin: skills.SkillActivationUser})
	if err != nil {
		writeServeSkillActivationError(w, err)
		return
	}
	if activation.Metadata.Execution == skills.SkillExecutionIsolatedAgent {
		s.startServeIsolatedSkill(w, r, sess, activation)
		return
	}
	s.startServeMainSkill(w, r, sess, activation)
}

func writeServeSkillActivationError(w http.ResponseWriter, err error) {
	var activationErr *skills.ActivationError
	if !errors.As(err, &activationErr) {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	switch activationErr.Kind {
	case skills.ActivationNotFound:
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", activationErr.Error())
	case skills.ActivationDisabledForOrigin:
		writeOpenAIError(w, http.StatusForbidden, "permission_error", activationErr.Error())
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", activationErr.Error())
	}
}

func (s *serveServer) startServeMainSkill(w http.ResponseWriter, r *http.Request, sess *session.Session, activation *skills.Activation) {
	if s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "session runtime is unavailable")
		return
	}
	if activeID := s.ensureResponseRuns().activeRunID(sess.ID); activeID != "" {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "a response is already active for this session")
		return
	}
	runtime, _, err := s.runtimeForRequest(r.Context(), sess.ID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	display := "/" + activation.Skill.Name
	if activation.RawArgs != "" {
		display += " " + activation.RawArgs
	}
	provenance := serveSkillProvenance(activation)
	provenance.Status = "running"
	instructions := skills.RenderActivationInstructions(activation)
	inputMessages := []llm.Message{
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartSkillActivation, SkillActivation: provenance}, {Type: llm.PartText, Text: instructions}}},
		llm.UserText(display),
	}
	run, err := s.startResponseRun(runtime, true, false, inputMessages, llm.Request{
		SessionID:           sess.ID,
		Model:               runtime.defaultModel,
		AllowedTools:        append([]string(nil), activation.AllowedTools...),
		AllowedToolsPresent: activation.AllowedToolsPresent,
	}, sess.ID, startResponseRunOptions{
		previousResponseID: runtime.getLastResponseID(),
		uiSession:          true,
		runtimeSetup: func(req *llm.Request) error {
			return setupServeMainSkillRequest(runtime, activation, req)
		},
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"execution":   "main",
		"response_id": run.id,
		"events_url":  "/v1/responses/" + run.id + "/events",
	})
}

func setupServeMainSkillRequest(runtime *serveRuntime, activation *skills.Activation, req *llm.Request) error {
	if runtime == nil || runtime.engine == nil || req == nil {
		return fmt.Errorf("session runtime is unavailable")
	}
	if len(activation.ToolDefs) > 0 {
		if runtime.toolMgr == nil {
			return fmt.Errorf("skill %q declares tools, but this session has no local tool registry", activation.Skill.Name)
		}
		if err := runtime.toolMgr.Registry.RegisterSkillTools(activation.ToolDefs, activation.BaseDir); err != nil {
			return err
		}
		for _, definition := range activation.ToolDefs {
			if tool, ok := runtime.toolMgr.Registry.Get(definition.Name); ok {
				runtime.engine.AddDynamicTool(tool)
			}
		}
	}
	req.Tools = runtime.selectTools(nil)
	if len(req.Tools) > 0 {
		req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	} else {
		req.ToolChoice = llm.ToolChoice{}
	}
	return nil
}

var (
	errServeSkillRunsStopping = errors.New("server is shutting down")
	errServeSkillRunActive    = errors.New("another isolated skill run is already active for this session")
)

const defaultServeSkillRunRetention = 10 * time.Minute

func (s *serveServer) serveSkillRunRetentionDuration() time.Duration {
	if s.skillRunRetention > 0 {
		return s.skillRunRetention
	}
	return defaultServeSkillRunRetention
}

func (s *serveServer) registerServeSkillRun(run *serveSkillRun) error {
	if run == nil {
		return fmt.Errorf("skill run is nil")
	}
	s.skillRunsMu.Lock()
	defer s.skillRunsMu.Unlock()
	if s.skillRunsStopping {
		return errServeSkillRunsStopping
	}
	for _, existing := range s.skillRuns {
		if existing.SessionID != run.SessionID {
			continue
		}
		existing.mu.Lock()
		active := existing.Status == "running" || existing.Status == "cancelling"
		existing.mu.Unlock()
		if active {
			return errServeSkillRunActive
		}
	}
	if s.skillRuns == nil {
		s.skillRuns = make(map[string]*serveSkillRun)
	}
	s.skillRuns[run.ID] = run
	s.skillRunsWG.Add(1)
	return nil
}

func (s *serveServer) scheduleServeSkillRunCleanup(run *serveSkillRun) {
	if run == nil {
		return
	}
	run.mu.Lock()
	if run.cleanupTimer != nil {
		run.cleanupTimer.Stop()
	}
	run.cleanupTimer = time.AfterFunc(s.serveSkillRunRetentionDuration(), func() {
		s.skillRunsMu.Lock()
		defer s.skillRunsMu.Unlock()
		if s.skillRuns[run.ID] != run {
			return
		}
		run.mu.Lock()
		defer run.mu.Unlock()
		if run.Status == "running" || run.Status == "cancelling" {
			return
		}
		run.cleanupTimer = nil
		delete(s.skillRuns, run.ID)
	})
	run.mu.Unlock()
}

func (s *serveServer) clearServeSkillRuns() {
	s.skillRunsMu.Lock()
	defer s.skillRunsMu.Unlock()
	for id, run := range s.skillRuns {
		run.mu.Lock()
		if run.cleanupTimer != nil {
			run.cleanupTimer.Stop()
			run.cleanupTimer = nil
		}
		run.mu.Unlock()
		delete(s.skillRuns, id)
	}
}

func (s *serveServer) stopServeSkillRuns(ctx context.Context) error {
	s.skillRunsMu.Lock()
	s.skillRunsStopping = true
	runs := make([]*serveSkillRun, 0, len(s.skillRuns))
	for _, run := range s.skillRuns {
		runs = append(runs, run)
	}
	s.skillRunsMu.Unlock()

	for _, run := range runs {
		run.mu.Lock()
		if run.Status == "running" {
			run.Status = "cancelling"
		}
		cancel := run.cancel
		run.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		s.skillRunsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.clearServeSkillRuns()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *serveServer) startServeIsolatedSkill(w http.ResponseWriter, r *http.Request, sess *session.Session, activation *skills.Activation) {
	var runtime *serveRuntime
	if s.sessionMgr != nil {
		runtime, _, _ = s.runtimeForRequest(r.Context(), sess.ID)
	}
	runner, err := s.serveSkillChildRunner(sess.ID, runtime)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	runID := "skill_" + randomSuffix()
	childSessionID := session.NewID()
	runCtx, cancel := context.WithCancel(context.Background())
	run := newServeSkillRun(runID, sess.ID, childSessionID, activation, cancel)
	if registerErr := s.registerServeSkillRun(run); registerErr != nil {
		cancel()
		if errors.Is(registerErr, errServeSkillRunsStopping) {
			writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", registerErr.Error())
		} else if errors.Is(registerErr, errServeSkillRunActive) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", registerErr.Error())
		} else {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", registerErr.Error())
		}
		return
	}
	display := "/" + activation.Skill.Name
	if activation.RawArgs != "" {
		display += " " + activation.RawArgs
	}
	s.persistServeSkillActivationEvent(sess.ID, display, activation, "running", runID, childSessionID)
	run.appendEvent("skill_run.created", map[string]any{"run_id": runID, "skill": activation.Skill.Name, "agent": activation.Metadata.Agent, "child_session_id": childSessionID})

	baseDir := strings.TrimSpace(sess.WorktreeDir)
	if baseDir == "" {
		baseDir = strings.TrimSpace(sess.CWD)
	}
	request := runpkg.ChildRunRequest{
		Kind:            runpkg.ChildRunIsolatedSkill,
		RunID:           runID,
		ChildSessionID:  childSessionID,
		AgentName:       activation.Metadata.Agent,
		Prompt:          skills.RenderActivationInstructions(activation),
		ModelOverride:   activation.Metadata.Model,
		ParentSessionID: sess.ID,
		BaseDir:         baseDir,
		Skill: &runpkg.SkillRunMetadata{
			Name:                activation.Skill.Name,
			Source:              activation.Skill.Source.SourceName(),
			SourcePath:          activation.BaseDir,
			RawArguments:        activation.RawArgs,
			AllowedTools:        append([]string(nil), activation.AllowedTools...),
			AllowedToolsPresent: activation.AllowedToolsPresent,
			ToolDefs:            append([]skills.SkillToolDef(nil), activation.ToolDefs...),
			Resources:           append([]string(nil), activation.Resources...),
		},
	}
	go func() {
		defer s.skillRunsWG.Done()
		defer cancel()
		result, runErr := runner.RunChild(runCtx, request, func(_ string, event tools.SubagentEvent) {
			run.appendEvent("skill_run.progress", event)
		})
		run.finish(result, runErr)
		persistCtx, cancelPersist := s.contextWithShutdown(context.Background())
		s.persistServeSkillRunResultAtBoundary(persistCtx, run, runErr)
		cancelPersist()
		s.scheduleServeSkillRunCleanup(run)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"execution":        "isolated",
		"run_id":           runID,
		"child_session_id": childSessionID,
		"events_url":       "/v1/sessions/" + sess.ID + "/skill-runs/" + runID + "/events",
	})
}

func (s *serveServer) serveSkillChildRunner(sessionID string, runtime *serveRuntime) (runpkg.ChildRunner, error) {
	if s.skillChildRunnerFactory != nil {
		return s.skillChildRunnerFactory(sessionID, runtime)
	}
	if s.cfgRef == nil {
		return nil, fmt.Errorf("server configuration is unavailable")
	}
	var approval *tools.ApprovalManager
	yolo := false
	if runtime != nil {
		yolo = runtime.yoloMode
		if runtime.toolMgr != nil {
			approval = runtime.toolMgr.ApprovalMgr
		}
	}
	runner, err := NewSpawnAgentRunnerWithStore(s.cfgRef, yolo, approval, s.store, sessionID)
	if err != nil {
		return nil, err
	}
	if runtime != nil && runtime.toolMgr != nil {
		runner.SetBaseDirFunc(runtime.toolMgr.BaseDir)
	}
	return runner, nil
}

func (s *serveServer) handleSessionSkillRun(w http.ResponseWriter, r *http.Request, sessionID, suffix string) {
	rest := strings.TrimPrefix(suffix, "skill-runs/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	run := s.getServeSkillRun(parts[0])
	if run == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "skill run not found")
		return
	}
	if run.SessionID != sessionID {
		writeOpenAIError(w, http.StatusForbidden, "permission_error", "skill run belongs to another session")
		return
	}
	exactRunPath := len(parts) == 1
	eventsPath := len(parts) == 2 && parts[1] == "events"
	if r.Method == http.MethodDelete && exactRunPath {
		run.mu.Lock()
		if run.Status == "running" {
			run.Status = "cancelling"
			if run.cancel != nil {
				run.cancel()
			}
		}
		status := run.Status
		run.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"id": run.ID, "status": status})
		return
	}
	if r.Method != http.MethodGet || (!exactRunPath && !eventsPath) {
		w.Header().Set("Allow", "GET, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		writeJSON(w, http.StatusOK, run.snapshot())
		return
	}
	s.streamServeSkillRunEvents(w, r, run)
}

func (s *serveServer) streamServeSkillRunEvents(w http.ResponseWriter, r *http.Request, run *serveSkillRun) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is unsupported")
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	replay, subscriberID, events, terminal := run.subscribe(after)
	defer run.unsubscribe(subscriberID)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	writeEvent := func(event serveSkillRunEvent) bool {
		data, err := json.Marshal(event)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, data)
		if err == nil {
			flusher.Flush()
		}
		return err == nil
	}
	for _, event := range replay {
		if !writeEvent(event) {
			return
		}
	}
	if terminal {
		return
	}
	for {
		select {
		case event, ok := <-events:
			if !ok {
				if run.subscriberOverflowed(subscriberID) {
					log.Printf("skill run %s subscriber %d fell behind; reconnecting from replay", run.ID, subscriberID)
				}
				return
			}
			if !writeEvent(event) {
				return
			}
			if event.Type == "skill_run.completed" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *serveServer) getServeSkillRun(runID string) *serveSkillRun {
	s.skillRunsMu.Lock()
	defer s.skillRunsMu.Unlock()
	return s.skillRuns[runID]
}

func (s *serveServer) hasActiveServeSkillRun(sessionID string) bool {
	s.skillRunsMu.Lock()
	defer s.skillRunsMu.Unlock()
	for _, run := range s.skillRuns {
		if run.SessionID != sessionID {
			continue
		}
		run.mu.Lock()
		active := run.Status == "running" || run.Status == "cancelling"
		run.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

func (s *serveServer) sessionSkillRunSnapshots(sessionID string) []map[string]any {
	s.skillRunsMu.Lock()
	var runs []*serveSkillRun
	for _, run := range s.skillRuns {
		if run.SessionID == sessionID {
			runs = append(runs, run)
		}
	}
	s.skillRunsMu.Unlock()
	result := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		result = append(result, run.snapshot())
	}
	return result
}

func (s *serveServer) persistServeSkillActivationEvent(sessionID, display string, activation *skills.Activation, status, runID, childSessionID string) {
	if s.store == nil || activation == nil {
		return
	}
	provenance := serveSkillProvenance(activation)
	provenance.Status = status
	provenance.RunID = runID
	provenance.ChildSessionID = childSessionID
	message := &session.Message{
		SessionID: sessionID,
		Role:      llm.RoleEvent,
		Parts: []llm.Part{
			{Type: llm.PartSkillActivation, SkillActivation: provenance},
			{Type: llm.PartText, Text: display},
		},
		TextContent: "↳ Skill invocation " + display + " · " + activation.Metadata.Execution.String(),
		CreatedAt:   time.Now(),
		Sequence:    -1,
	}
	if err := s.store.AddMessage(context.Background(), sessionID, message); err != nil {
		log.Printf("[serve] persist skill activation for %s: %v", sessionID, err)
	}
}

func serveSkillProvenance(activation *skills.Activation) *llm.SkillActivationProvenance {
	return &llm.SkillActivationProvenance{
		Name:                activation.Skill.Name,
		Source:              activation.Skill.Source.SourceName(),
		SourcePath:          activation.Skill.SourcePath,
		Origin:              activation.Origin.String(),
		Execution:           activation.Metadata.Execution.String(),
		RawArguments:        activation.RawArgs,
		Agent:               activation.Metadata.Agent,
		Model:               activation.Metadata.Model,
		AllowedTools:        append([]string(nil), activation.AllowedTools...),
		AllowedToolsPresent: activation.AllowedToolsPresent,
		ActivatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func (s *serveServer) serveSkillRunsAreStopping() bool {
	s.skillRunsMu.Lock()
	defer s.skillRunsMu.Unlock()
	return s.skillRunsStopping
}

func (s *serveServer) persistServeSkillRunResultAtBoundary(ctx context.Context, run *serveSkillRun, runErr error) {
	if run == nil {
		return
	}
	if s.sessionMgr != nil {
		if runtime, ok := s.sessionMgr.Get(run.SessionID); ok && runtime != nil {
			retryDelay := 25 * time.Millisecond
			for !runtime.mu.TryLock() {
				if !s.waitForServeSkillBoundaryRetry(ctx, retryDelay) {
					log.Printf("[serve] skip skill result persistence for %s during shutdown: session runtime is still active", run.SessionID)
					return
				}
				retryDelay = min(retryDelay*2, time.Second)
			}
			defer runtime.mu.Unlock()
			if contextMessage := s.persistServeSkillRunResult(run, runErr); contextMessage != nil {
				runtime.history = append(runtime.history, *contextMessage)
				runtime.refreshSideQuestionSnapshot(runtime.history)
			}
			return
		}
	}

	manager := s.ensureResponseRuns()
	retryDelay := 25 * time.Millisecond
	for {
		persisted := manager.runIfSessionIdle(run.SessionID, func() {
			s.persistServeSkillRunResult(run, runErr)
		})
		if persisted {
			return
		}
		if !s.waitForServeSkillBoundaryRetry(ctx, retryDelay) {
			log.Printf("[serve] skip skill result persistence for %s during shutdown: parent response is still active", run.SessionID)
			return
		}
		retryDelay = min(retryDelay*2, time.Second)
	}
}

func (s *serveServer) waitForServeSkillBoundaryRetry(ctx context.Context, delay time.Duration) bool {
	if ctx.Err() != nil || s.serveSkillRunsAreStopping() {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return !s.serveSkillRunsAreStopping()
	}
}

func (s *serveServer) persistServeSkillRunResult(run *serveSkillRun, runErr error) *llm.Message {
	if s.store == nil || run == nil || run.Activation == nil {
		return nil
	}
	run.mu.Lock()
	status := run.Status
	output := run.Output
	completedAt := run.CompletedAt
	childSessionID := run.ChildSessionID
	run.mu.Unlock()
	provenance := serveSkillProvenance(run.Activation)
	provenance.RunID = run.ID
	provenance.ChildSessionID = childSessionID
	provenance.Status = status
	provenance.StartedAt = run.StartedAt.UTC().Format(time.RFC3339Nano)
	provenance.CompletedAt = completedAt.UTC().Format(time.RFC3339Nano)
	resultText := fmt.Sprintf("↳ Skill /%s · @%s · %s · run %s · child %s", run.SkillName, run.Agent, status, run.ID, childSessionID)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		resultText += "\n" + runErr.Error()
	}
	if strings.TrimSpace(output) != "" {
		resultText += "\n\n" + output
	}
	visible := &session.Message{SessionID: run.SessionID, Role: llm.RoleEvent, Parts: []llm.Part{{Type: llm.PartSkillActivation, SkillActivation: provenance}, {Type: llm.PartText, Text: resultText}}, TextContent: resultText, CreatedAt: completedAt, Sequence: -1}
	if err := s.store.AddMessage(context.Background(), run.SessionID, visible); err != nil {
		log.Printf("[serve] persist skill result event for %s: %v", run.SessionID, err)
	}
	var activeContext *llm.Message
	if strings.TrimSpace(output) != "" {
		contextText := fmt.Sprintf("<isolated_skill_result name=%q run_id=%q child_session_id=%q status=%q>\n%s\n</isolated_skill_result>", run.SkillName, run.ID, childSessionID, status, output)
		developer := &session.Message{SessionID: run.SessionID, Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartSkillActivation, SkillActivation: provenance}, {Type: llm.PartText, Text: contextText}}, TextContent: contextText, CreatedAt: completedAt, Sequence: -1}
		if err := s.store.AddMessage(context.Background(), run.SessionID, developer); err != nil {
			log.Printf("[serve] persist skill result context for %s: %v", run.SessionID, err)
		} else {
			message := developer.ToLLMMessage()
			activeContext = &message
		}
	}
	return activeContext
}
