package serve

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/inspect"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	inspectBodiesMaxIDs        = 64
	inspectBodiesMaxQueryBytes = 8 * 1024
)

var inspectBlockIDPattern = regexp.MustCompile(`^(?:(?:m\d+|i\d+)(?:\.p\d+)?\.[a-z_]+|runtime\.(?:system_prompt|tool\.\d+))$`)

func parseInspectBodyIDs(raw string) ([]string, error) {
	if len(raw) > inspectBodiesMaxQueryBytes {
		return nil, fmt.Errorf("inspect bodies query is too long")
	}
	if raw == "" {
		return []string{}, nil
	}
	ids := strings.Split(raw, ",")
	if len(ids) > inspectBodiesMaxIDs {
		return nil, fmt.Errorf("too many inspect body IDs (maximum %d)", inspectBodiesMaxIDs)
	}
	for _, id := range ids {
		if !inspectBlockIDPattern.MatchString(id) {
			return nil, fmt.Errorf("invalid inspect body ID %q", id)
		}
	}
	return ids, nil
}

func (s *serveServer) handleSessionInspect(w http.ResponseWriter, r *http.Request, sessionID string) {
	document, ok := s.loadInspectDocument(w, r, sessionID)
	if !ok {
		return
	}
	writeTranscriptJSON(w, r, document.TranscriptRev, document)
}

func (s *serveServer) handleSessionInspectBodies(w http.ResponseWriter, r *http.Request, sessionID string) {
	if len(r.URL.RawQuery) > inspectBodiesMaxQueryBytes {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "inspect bodies query is too long")
		return
	}
	ids, err := parseInspectBodyIDs(r.URL.Query().Get("ids"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	document, ok := s.loadInspectDocument(w, r, sessionID)
	if !ok {
		return
	}
	writeTranscriptJSON(w, r, document.TranscriptRev, document.ResolveBodies(ids))
}

func (s *serveServer) loadInspectDocument(w http.ResponseWriter, r *http.Request, sessionID string) (*inspect.Document, bool) {
	// This loader reads the store first and only peeks at an existing runtime. It
	// neither creates one nor renews its idle TTL, and TryLocks runtime history.
	meta, messages, rt, ok := s.loadStatsInputs(w, r, sessionID)
	if !ok {
		return nil, false
	}
	rev := int64(-1)
	if reader, ok := s.store.(interface {
		TranscriptRev(context.Context, string) (int64, error)
	}); ok {
		var err error
		rev, err = reader.TranscriptRev(r.Context(), sessionID)
		if err != nil {
			writeOpenAIError(w, 500, "server_error", "failed to get inspect revision")
			return nil, false
		}
	}
	scope := "persisted_history"
	if s.store == nil {
		scope = "runtime_history"
		// NewMessage supplies wall-clock timestamps to the fallback loader, but
		// these are not historical timestamps. Omit them so an unchanged runtime
		// snapshot has a stable ETag; part-level timestamps remain available.
		for i := range messages {
			messages[i].CreatedAt = time.Time{}
		}
	}
	opts := inspect.Options{SessionID: sessionID, TranscriptRev: rev, Scope: scope, Session: meta, Runtime: inspectRuntimeSnapshot(rt, sessionID)}
	opts.ChildSessionIDs = func(name, content string) []string {
		if name == tools.SpawnAgentToolName {
			if id := s.validatedSpawnChildID(sessionID, inspect.ExtractSessionIDFromSpawnAgentResult(content)); id != "" {
				return []string{id}
			}
			return nil
		}
		return s.agentResultSessionIDs(sessionID, name, content)
	}
	return inspect.Project(messages, opts), true
}

func inspectRuntimeSnapshot(rt *serveRuntime, sessionID string) inspect.RuntimeOptions {
	if rt == nil {
		return inspect.RuntimeOptions{UnavailableReason: "runtime_not_loaded"}
	}
	if !rt.mu.TryLock() {
		return inspect.RuntimeOptions{Loaded: true, UnavailableReason: "runtime_busy"}
	}
	out := inspect.RuntimeOptions{Loaded: true, SystemPrompt: rt.systemPrompt}
	engine := rt.engine
	var approval *tools.ApprovalManager
	if rt.toolMgr != nil {
		approval = rt.toolMgr.ApprovalMgr
	}
	rt.mu.Unlock()
	// Engine's registry and discovery planner synchronize their own snapshots.
	// In particular Tool.Spec() and planner access occur after releasing rt.mu:
	// implementations may do work beyond simple field reads.
	if engine != nil {
		out.Tools = inspectRuntimeTools(engine, sessionID, approval)
		if diagnostics, ok := engine.ToolDiscoveryDiagnostics(sessionID); ok {
			out.ToolDiscovery = &diagnostics
		}
	}
	return out
}

func inspectRuntimeTools(engine *engine.Engine, sessionID string, approval *tools.ApprovalManager) []core.ToolSpec {
	specs := engine.Tools().AllSpecs()
	specs = append(specs, engine.ToolDiscoveryActiveSpecs(sessionID)...)
	specs = tools.FilterToolSpecsForApprovalMode(specs, approval)
	byName := make(map[string]core.ToolSpec, len(specs))
	for _, spec := range specs {
		byName[spec.Name] = spec
	}
	specs = make([]core.ToolSpec, 0, len(byName))
	for _, spec := range byName {
		specs = append(specs, spec)
	}
	slices.SortFunc(specs, func(a, b core.ToolSpec) int { return strings.Compare(a.Name, b.Name) })
	return specs
}
