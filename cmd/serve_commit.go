package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/commitworkflow"
	"github.com/samsaffron/term-llm/internal/gitcommit"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/worktree"
)

const commitOperationProviderKey = "term-llm.commit-operations.v1"

var errCommitCheckoutBusy = errors.New("the checkout is busy with another response or Git operation")

type serveCommitRun struct {
	mu             sync.Mutex
	ID             string                        `json:"run_id"`
	SessionID      string                        `json:"session_id"`
	Kind           string                        `json:"kind"`
	Status         string                        `json:"status"`
	AgentName      string                        `json:"agent_name,omitempty"`
	AgentSource    string                        `json:"agent_source,omitempty"`
	ChildSessionID string                        `json:"child_session_id,omitempty"`
	Proposal       *commitworkflow.ScopeProposal `json:"proposal,omitempty"`
	Message        string                        `json:"message,omitempty"`
	Provider       string                        `json:"provider,omitempty"`
	Model          string                        `json:"model,omitempty"`
	Error          string                        `json:"error,omitempty"`
	StartedAt      time.Time                     `json:"started_at"`
	CompletedAt    time.Time                     `json:"completed_at,omitempty"`
	checkoutRoot   string
	cancel         context.CancelFunc
	events         []any
}

func (r *serveCommitRun) snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{"run_id": r.ID, "session_id": r.SessionID, "kind": r.Kind, "status": r.Status, "agent_name": r.AgentName, "agent_source": r.AgentSource, "child_session_id": r.ChildSessionID, "proposal": r.Proposal, "message": r.Message, "provider": r.Provider, "model": r.Model, "error": r.Error, "started_at": r.StartedAt, "completed_at": r.CompletedAt}
}

type serveCommitOperation struct {
	ID             string                  `json:"operation_id"`
	SessionID      string                  `json:"session_id"`
	IdempotencyKey string                  `json:"idempotency_key"`
	RequestHash    string                  `json:"request_hash"`
	Status         string                  `json:"status"`
	Expected       gitcommit.Fingerprint   `json:"expected_fingerprint"`
	Message        string                  `json:"message,omitempty"`
	Result         *gitcommit.CommitResult `json:"result,omitempty"`
	Error          string                  `json:"error,omitempty"`
	ErrorKind      gitcommit.ErrorKind     `json:"error_kind,omitempty"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
	checkoutRoot   string
}

type commitStageBody struct {
	Mode                gitcommit.StageMode   `json:"mode"`
	Paths               []string              `json:"paths"`
	ExpectedStatusToken string                `json:"expected_status_token"`
	ExpectedFingerprint gitcommit.Fingerprint `json:"expected_fingerprint"`
}
type commitRunBody struct {
	Kind                string                `json:"kind"`
	Intent              string                `json:"intent"`
	ScopeSummary        string                `json:"scope_summary"`
	ExpectedStatusToken string                `json:"expected_status_token"`
	ExpectedFingerprint gitcommit.Fingerprint `json:"expected_fingerprint"`
}
type commitOperationBody struct {
	Message             string                `json:"message"`
	ExpectedFingerprint gitcommit.Fingerprint `json:"expected_fingerprint"`
}

func (s *serveServer) commitSession(ctx context.Context, sessionID string) (*session.Session, string, error) {
	if s.store == nil {
		return nil, "", session.ErrNotFound
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, "", session.ErrNotFound
	}
	dir := ""
	if strings.TrimSpace(sess.ProjectID) != "" {
		binding, resolveErr := s.resolvePersistedProjectWorkspace(ctx, *sess)
		if resolveErr != nil {
			return nil, "", resolveErr
		}
		dir = strings.TrimSpace(binding.RuntimeDir)
		if dir == "" {
			dir = strings.TrimSpace(binding.RepoRoot)
		}
	} else if strings.TrimSpace(sess.WorktreeDir) != "" {
		dir = strings.TrimSpace(sess.WorktreeDir)
	} else {
		dir = strings.TrimSpace(sess.CWD)
	}
	if dir == "" {
		return nil, "", errors.New("session has no active checkout")
	}
	return sess, dir, nil
}

func (s *serveServer) commitRepository(ctx context.Context, sessionID string) (*session.Session, *gitcommit.Repository, error) {
	sess, dir, err := s.commitSession(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	repo, err := gitcommit.OpenWithCoordinator(ctx, dir, commitRootCoordinator{})
	return sess, repo, err
}

type commitRootCoordinator struct{}

func (commitRootCoordinator) Acquire(ctx context.Context, root string) (func(), error) {
	main, mainErr := worktree.MainRepoRoot(root)
	checkout, checkoutErr := worktree.CheckoutRoot(root)
	if mainErr != nil {
		return nil, fmt.Errorf("resolve main checkout: %w", mainErr)
	}
	if checkoutErr != nil {
		return nil, fmt.Errorf("resolve active checkout: %w", checkoutErr)
	}
	main, mainErr = filepath.Abs(main)
	if mainErr != nil {
		return nil, fmt.Errorf("canonicalize main checkout: %w", mainErr)
	}
	checkout, checkoutErr = filepath.Abs(checkout)
	if checkoutErr != nil {
		return nil, fmt.Errorf("canonicalize active checkout: %w", checkoutErr)
	}
	if filepath.Clean(main) != filepath.Clean(checkout) {
		return func() {}, nil
	}
	release, blocked, err := processRootCheckoutLeases.tryAcquireMutation(root, false)
	if err != nil {
		return nil, err
	}
	if blocked != rootCheckoutMutationAvailable {
		return nil, errCommitCheckoutBusy
	}
	return release, nil
}

func (s *serveServer) commitBusy(sessionID string, includeChildRuns bool) bool {
	checkout := ""
	if _, dir, err := s.commitSession(context.Background(), sessionID); err == nil {
		checkout = canonicalCommitCheckout(dir)
	}
	if s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok && rt != nil && rt.hasActiveRun() {
			return true
		}
		if checkout != "" {
			s.sessionMgr.mu.Lock()
			runtimes := make(map[string]*serveRuntime, len(s.sessionMgr.sessions))
			for id, rt := range s.sessionMgr.sessions {
				runtimes[id] = rt
			}
			s.sessionMgr.mu.Unlock()
			for id, rt := range runtimes {
				if id == sessionID || rt == nil || !rt.hasActiveRun() {
					continue
				}
				if _, dir, err := s.commitSession(context.Background(), id); err == nil && canonicalCommitCheckout(dir) == checkout {
					return true
				}
			}
		}
	}
	s.skillRunsMu.Lock()
	var activeSkillSessions []string
	for _, run := range s.skillRuns {
		if run == nil {
			continue
		}
		run.mu.Lock()
		active := run.Status == "running" || run.Status == "cancelling"
		run.mu.Unlock()
		if active {
			activeSkillSessions = append(activeSkillSessions, run.SessionID)
		}
	}
	s.skillRunsMu.Unlock()
	for _, id := range activeSkillSessions {
		if id == sessionID {
			return true
		}
		if checkout != "" {
			if _, dir, err := s.commitSession(context.Background(), id); err == nil && canonicalCommitCheckout(dir) == checkout {
				return true
			}
		}
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if includeChildRuns {
		for _, run := range s.commitRuns {
			if run.SessionID == sessionID || (checkout != "" && run.checkoutRoot == checkout) {
				run.mu.Lock()
				active := run.Status == "running" || run.Status == "cancelling"
				run.mu.Unlock()
				if active {
					return true
				}
			}
		}
	}
	for _, operation := range s.commitOperations {
		matches := operation.SessionID == sessionID || (checkout != "" && operation.checkoutRoot == checkout)
		if matches && (operation.Status == "queued" || operation.Status == "running") {
			return true
		}
	}
	return false
}

func canonicalCommitCheckout(dir string) string {
	root, err := worktree.CheckoutRoot(dir)
	if err != nil {
		return ""
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return ""
	}
	return filepath.Clean(root)
}

// commitActiveForSession blocks new model/tool work while a commit workflow can
// mutate the same session or checkout. Idempotent replay is checked by callers
// before this admission check so reconnects remain available.
func (s *serveServer) commitActiveForSession(ctx context.Context, sessionID string) bool {
	checkout := ""
	if _, dir, err := s.commitSession(ctx, sessionID); err == nil {
		checkout = canonicalCommitCheckout(dir)
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	for _, run := range s.commitRuns {
		if run == nil {
			continue
		}
		run.mu.Lock()
		active := run.Status == "running" || run.Status == "cancelling"
		matches := run.SessionID == sessionID || (checkout != "" && run.checkoutRoot == checkout)
		run.mu.Unlock()
		if active && matches {
			return true
		}
	}
	for _, operation := range s.commitOperations {
		if operation == nil {
			continue
		}
		active := operation.Status == "queued" || operation.Status == "running"
		matches := operation.SessionID == sessionID || (checkout != "" && operation.checkoutRoot == checkout)
		if active && matches {
			return true
		}
	}
	return false
}

func decodeCommitJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return false
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid commit request: "+err.Error())
		return false
	}
	return true
}
func writeCommitError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "server_error"
	switch {
	case errors.Is(err, session.ErrNotFound), gitcommit.IsKind(err, gitcommit.ErrNotRepository):
		status = http.StatusNotFound
		code = "not_found_error"
	case errors.Is(err, errCommitCheckoutBusy), gitcommit.IsKind(err, gitcommit.ErrStale), gitcommit.IsKind(err, gitcommit.ErrIndexLock):
		status = http.StatusConflict
		code = "conflict_error"
	case gitcommit.IsKind(err, gitcommit.ErrInvalidSelection), gitcommit.IsKind(err, gitcommit.ErrEmptyIndex), gitcommit.IsKind(err, gitcommit.ErrConflict), gitcommit.IsKind(err, gitcommit.ErrUnsupportedOperation), gitcommit.IsKind(err, gitcommit.ErrIntentToAdd):
		status = http.StatusUnprocessableEntity
		code = "invalid_request_error"
	case gitcommit.IsKind(err, gitcommit.ErrGitMissing):
		status = http.StatusServiceUnavailable
	}
	writeOpenAIError(w, status, code, err.Error())
}

func (s *serveServer) handleCommitStatus(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.commitBusy(sessionID, true) {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "the session is busy; wait for active responses, skills, or commit runs")
		return
	}
	_, repo, err := s.commitRepository(r.Context(), sessionID)
	if err != nil {
		writeCommitError(w, err)
		return
	}
	state, err := repo.Inspect(r.Context())
	if err != nil {
		writeCommitError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
func (s *serveServer) handleCommitStage(w http.ResponseWriter, r *http.Request, sessionID string) {
	var body commitStageBody
	if !decodeCommitJSON(w, r, &body) {
		return
	}
	if s.commitBusy(sessionID, true) {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "the session is busy")
		return
	}
	_, repo, err := s.commitRepository(r.Context(), sessionID)
	if err != nil {
		writeCommitError(w, err)
		return
	}
	state, err := repo.Stage(r.Context(), gitcommit.StageRequest{Mode: body.Mode, Paths: body.Paths, StatusToken: body.ExpectedStatusToken}, body.ExpectedFingerprint)
	if err != nil {
		writeCommitError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *serveServer) handleCreateCommitRun(w http.ResponseWriter, r *http.Request, sessionID string) {
	var body commitRunBody
	if !decodeCommitJSON(w, r, &body) {
		return
	}
	if body.Kind != "scope" && body.Kind != "message" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "kind must be scope or message")
		return
	}
	if s.commitBusy(sessionID, false) {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "the session is busy")
		return
	}
	sess, dir, err := s.commitSession(r.Context(), sessionID)
	if err != nil {
		writeCommitError(w, err)
		return
	}
	var runtime *serveRuntime
	if s.sessionMgr != nil {
		runtime, _, _ = s.runtimeForRequest(r.Context(), sessionID)
	}
	runner, err := s.serveSkillChildRunner(sessionID, runtime)
	if err != nil {
		writeCommitError(w, err)
		return
	}
	agentName := "commit-message"
	if s.cfgRef != nil {
		agentName = s.cfgRef.Commit.EffectiveMessageAgent()
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &serveCommitRun{ID: "commit_run_" + randomSuffix(), SessionID: sessionID, Kind: body.Kind, Status: "running", AgentName: agentName, StartedAt: time.Now().UTC(), checkoutRoot: canonicalCommitCheckout(dir), cancel: cancel}
	s.commitMu.Lock()
	if s.commitStopping {
		s.commitMu.Unlock()
		cancel()
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "commit operations are shutting down")
		return
	}
	if s.commitRuns == nil {
		s.commitRuns = map[string]*serveCommitRun{}
	}
	for _, existing := range s.commitRuns {
		existing.mu.Lock()
		active := existing.Status == "running" || existing.Status == "cancelling"
		if active && run.checkoutRoot != "" && existing.checkoutRoot == run.checkoutRoot && existing.SessionID != sessionID {
			existing.mu.Unlock()
			s.commitMu.Unlock()
			cancel()
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "a commit workflow is already active for this checkout")
			return
		}
		if existing.SessionID == sessionID && active && existing.cancel != nil {
			existing.cancel()
		}
		existing.mu.Unlock()
	}
	for _, operation := range s.commitOperations {
		matches := operation.SessionID == sessionID || (run.checkoutRoot != "" && operation.checkoutRoot == run.checkoutRoot)
		if matches && (operation.Status == "queued" || operation.Status == "running") {
			s.commitMu.Unlock()
			cancel()
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "a commit operation is already active for this checkout")
			return
		}
	}
	s.commitRuns[run.ID] = run
	s.commitRunsWG.Add(1)
	s.commitMu.Unlock()
	go func() {
		defer s.commitRunsWG.Done()
		defer cancel()
		request := commitworkflow.Request{ParentSessionID: sess.ID, CheckoutDir: dir, AgentName: agentName, Intent: body.Intent, ScopeSummary: body.ScopeSummary, ExpectedFingerprint: body.ExpectedFingerprint, ExpectedStatusToken: body.ExpectedStatusToken, Runner: runner, Progress: func(_ string, event tools.SubagentEvent) {
			run.mu.Lock()
			if len(run.events) < 1024 {
				run.events = append(run.events, event)
			}
			run.mu.Unlock()
		}}
		var runErr error
		var meta commitworkflow.ChildRunMetadata
		var completedProposal *commitworkflow.ScopeProposal
		var completedMessage string
		if body.Kind == "scope" {
			proposal, childMeta, err := commitworkflow.New().PlanScope(ctx, request)
			meta = childMeta
			runErr = err
			if err == nil {
				completedProposal = &proposal
			}
		} else {
			message, childMeta, err := commitworkflow.New().DraftMessage(ctx, request)
			meta = childMeta
			runErr = err
			if err == nil {
				completedMessage = message
			}
		}
		run.mu.Lock()
		run.Proposal = completedProposal
		run.Message = completedMessage
		run.AgentName = meta.AgentName
		run.AgentSource = meta.AgentSource
		run.ChildSessionID = meta.ChildSessionID
		run.Provider = meta.Provider
		run.Model = meta.Model
		run.CompletedAt = time.Now().UTC()
		if errors.Is(runErr, context.Canceled) {
			run.Status = "cancelled"
		} else if runErr != nil {
			run.Status = "failed"
			run.Error = runErr.Error()
		} else {
			run.Status = "complete"
		}
		run.cancel = nil
		run.mu.Unlock()
	}()
	snapshot := run.snapshot()
	snapshot["events_url"] = "/v1/sessions/" + sessionID + "/commit-runs/" + run.ID + "/events"
	writeJSON(w, http.StatusAccepted, snapshot)
}

func (s *serveServer) commitRun(sessionID, runID string) *serveCommitRun {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	run := s.commitRuns[runID]
	if run == nil || run.SessionID != sessionID {
		return nil
	}
	return run
}
func (s *serveServer) handleGetCommitRun(w http.ResponseWriter, _ *http.Request, sessionID, runID string) {
	run := s.commitRun(sessionID, runID)
	if run == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "commit run not found")
		return
	}
	writeJSON(w, http.StatusOK, run.snapshot())
}
func (s *serveServer) handleCancelCommitRun(w http.ResponseWriter, _ *http.Request, sessionID, runID string) {
	run := s.commitRun(sessionID, runID)
	if run == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "commit run not found")
		return
	}
	run.mu.Lock()
	cancel := run.cancel
	if run.Status == "running" {
		run.Status = "cancelling"
	}
	run.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	writeJSON(w, http.StatusAccepted, run.snapshot())
}
func (s *serveServer) handleCommitRunEvents(w http.ResponseWriter, r *http.Request, sessionID, runID string) {
	run := s.commitRun(sessionID, runID)
	if run == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "commit run not found")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	writer := bufio.NewWriter(w)
	for {
		snapshot := run.snapshot()
		data, _ := json.Marshal(snapshot)
		fmt.Fprintf(writer, "event: snapshot\ndata: %s\n\n", data)
		writer.Flush()
		if flusher != nil {
			flusher.Flush()
		}
		status, _ := snapshot["status"].(string)
		if status != "running" && status != "cancelling" {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func operationMapKey(sessionID, id string) string { return sessionID + "\x00" + id }
func (s *serveServer) loadCommitOperationsLocked(ctx context.Context, sessionID string) {
	prefix := sessionID + "\x00"
	for key := range s.commitOperations {
		if strings.HasPrefix(key, prefix) {
			return
		}
	}
	store, ok := s.store.(session.ProviderStateStore)
	if !ok {
		return
	}
	raw, err := store.LoadProviderState(ctx, sessionID, commitOperationProviderKey)
	if err != nil || len(raw) == 0 {
		return
	}
	var records []*serveCommitOperation
	if json.Unmarshal(raw, &records) != nil {
		return
	}
	changed := false
	for _, op := range records {
		if op == nil {
			continue
		}
		if op.Status == "queued" || op.Status == "running" {
			op.Status = "uncertain"
			op.Error = "server restarted before the Git outcome was classified"
			op.UpdatedAt = time.Now().UTC()
			changed = true
		}
		s.commitOperations[operationMapKey(sessionID, op.ID)] = op
	}
	if changed {
		s.persistCommitOperationsLocked(context.Background(), sessionID)
	}
}
func (s *serveServer) persistCommitOperationsLocked(ctx context.Context, sessionID string) {
	store, ok := s.store.(session.ProviderStateStore)
	if !ok {
		return
	}
	var records []*serveCommitOperation
	prefix := sessionID + "\x00"
	for key, op := range s.commitOperations {
		if strings.HasPrefix(key, prefix) {
			records = append(records, op)
		}
	}
	raw, _ := json.Marshal(records)
	_ = store.SaveProviderState(ctx, sessionID, commitOperationProviderKey, raw)
}
func (s *serveServer) findOperationByKeyLocked(sessionID, key string) *serveCommitOperation {
	for mapKey, op := range s.commitOperations {
		if strings.HasPrefix(mapKey, sessionID+"\x00") && op.IdempotencyKey == key {
			return op
		}
	}
	return nil
}

func (s *serveServer) handleCreateCommitOperation(w http.ResponseWriter, r *http.Request, sessionID string) {
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Idempotency-Key is required")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 128<<10))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var body commitOperationBody
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid commit operation: "+err.Error())
		return
	}
	canonical, _ := json.Marshal(body)
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	s.commitMu.Lock()
	if s.commitOperations == nil {
		s.commitOperations = map[string]*serveCommitOperation{}
	}
	s.loadCommitOperationsLocked(r.Context(), sessionID)
	if existing := s.findOperationByKeyLocked(sessionID, key); existing != nil {
		if existing.RequestHash != hash {
			s.commitMu.Unlock()
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "Idempotency-Key was already used with a different request")
			return
		}
		snapshot := *existing
		s.commitMu.Unlock()
		writeJSON(w, http.StatusAccepted, snapshot)
		return
	}
	s.commitMu.Unlock()
	if s.commitBusy(sessionID, true) {
		s.commitMu.Lock()
		existing := s.findOperationByKeyLocked(sessionID, key)
		if existing != nil && existing.RequestHash == hash {
			snapshot := *existing
			s.commitMu.Unlock()
			writeJSON(w, http.StatusAccepted, snapshot)
			return
		}
		s.commitMu.Unlock()
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "the session is busy")
		return
	}
	_, repo, err := s.commitRepository(r.Context(), sessionID)
	if err != nil {
		writeCommitError(w, err)
		return
	}
	op := &serveCommitOperation{ID: "commit_op_" + randomSuffix(), SessionID: sessionID, IdempotencyKey: key, RequestHash: hash, Status: "queued", Expected: body.ExpectedFingerprint, Message: body.Message, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), checkoutRoot: filepath.Clean(repo.CheckoutRoot())}
	s.commitMu.Lock()
	if existing := s.findOperationByKeyLocked(sessionID, key); existing != nil {
		if existing.RequestHash != hash {
			s.commitMu.Unlock()
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "Idempotency-Key was already used with a different request")
			return
		}
		snapshot := *existing
		s.commitMu.Unlock()
		writeJSON(w, http.StatusAccepted, snapshot)
		return
	}
	if s.commitStopping {
		s.commitMu.Unlock()
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "commit operations are shutting down")
		return
	}
	for _, activeRun := range s.commitRuns {
		activeRun.mu.Lock()
		active := activeRun.Status == "running" || activeRun.Status == "cancelling"
		matches := activeRun.SessionID == sessionID || (op.checkoutRoot != "" && activeRun.checkoutRoot == op.checkoutRoot)
		activeRun.mu.Unlock()
		if active && matches {
			s.commitMu.Unlock()
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "a commit workflow is already running for this checkout")
			return
		}
	}
	for _, active := range s.commitOperations {
		matches := active.SessionID == sessionID || (op.checkoutRoot != "" && active.checkoutRoot == op.checkoutRoot)
		if matches && (active.Status == "queued" || active.Status == "running") {
			s.commitMu.Unlock()
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "a commit operation is already running for this checkout")
			return
		}
	}
	s.commitOperations[operationMapKey(sessionID, op.ID)] = op
	s.persistCommitOperationsLocked(context.Background(), sessionID)
	s.commitOperationsWG.Add(1)
	initialSnapshot := *op
	s.commitMu.Unlock()
	go func() {
		defer s.commitOperationsWG.Done()
		s.commitMu.Lock()
		op.Status = "running"
		op.UpdatedAt = time.Now().UTC()
		s.persistCommitOperationsLocked(context.Background(), sessionID)
		s.commitMu.Unlock()
		result, commitErr := repo.Commit(context.Background(), body.Message, body.ExpectedFingerprint)
		s.commitMu.Lock()
		op.UpdatedAt = time.Now().UTC()
		if commitErr != nil {
			op.Status = "failed"
			op.Error = commitErr.Error()
			var typedErr *gitcommit.Error
			if errors.As(commitErr, &typedErr) {
				op.ErrorKind = typedErr.Kind
			}
			if gitcommit.IsKind(commitErr, gitcommit.ErrUncertain) {
				op.Status = "uncertain"
			}
		} else {
			op.Status = "succeeded"
			op.Result = &result
		}
		s.persistCommitOperationsLocked(context.Background(), sessionID)
		s.commitMu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, initialSnapshot)
}
func (s *serveServer) handleGetCommitOperation(w http.ResponseWriter, r *http.Request, sessionID, operationID string) {
	s.commitMu.Lock()
	if s.commitOperations == nil {
		s.commitOperations = map[string]*serveCommitOperation{}
	}
	s.loadCommitOperationsLocked(r.Context(), sessionID)
	op := s.commitOperations[operationMapKey(sessionID, operationID)]
	if op == nil {
		s.commitMu.Unlock()
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "commit operation not found")
		return
	}
	snapshot := *op
	s.commitMu.Unlock()
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *serveServer) stopCommitWorkflows(ctx context.Context) error {
	s.commitMu.Lock()
	s.commitStopping = true
	runs := make([]*serveCommitRun, 0, len(s.commitRuns))
	for _, run := range s.commitRuns {
		runs = append(runs, run)
	}
	s.commitMu.Unlock()
	for _, run := range runs {
		run.mu.Lock()
		cancel := run.cancel
		run.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	done := make(chan struct{})
	go func() {
		s.commitRunsWG.Wait()
		s.commitOperationsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.commitMu.Lock()
		sessions := map[string]struct{}{}
		for _, operation := range s.commitOperations {
			if operation.Status == "queued" || operation.Status == "running" {
				operation.Status = "uncertain"
				operation.Error = "server shutdown timed out before the Git outcome was classified"
				operation.UpdatedAt = time.Now().UTC()
				sessions[operation.SessionID] = struct{}{}
			}
		}
		for sessionID := range sessions {
			s.persistCommitOperationsLocked(context.Background(), sessionID)
		}
		s.commitMu.Unlock()
		return ctx.Err()
	}
}
