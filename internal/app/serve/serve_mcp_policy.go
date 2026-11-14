package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/term-llm/term-llm/internal/app/runtime/mcpstart"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
)

func (rt *serveRuntime) validateMCPAccess(names []string) error {
	for _, name := range names {
		if err := rt.mcpManager.CheckAccess(name); err != nil {
			return err
		}
	}
	return nil
}

func (s *serveServer) cleanPersistedMCPSelection(ctx context.Context, sess *session.Session, rt *serveRuntime) ([]string, error) {
	selection := rt.mcpSetting
	if sess != nil {
		selection = sess.MCP
	}
	if rt.mcpManager == nil && len(mcpstart.ParseServerList(selection)) == 0 {
		return nil, nil
	}
	if err := rt.ensureMCPManagerLocked(); err != nil {
		return nil, err
	}
	if err := rt.mcpManager.RefreshPolicy(); err != nil {
		return nil, err
	}
	cleaned, err := mcpstart.CleanSelection(rt.mcpManager, selection, false, log.Writer())
	if err != nil {
		return nil, err
	}
	desired := mcpstart.NormalizeSelection(mcpstart.ParseServerList(cleaned))
	if sess != nil && cleaned != sess.MCP {
		sess.MCP = cleaned
		if err := s.store.Update(ctx, sess); err != nil {
			return nil, fmt.Errorf("clean persisted MCP selection: %w", err)
		}
	}
	return desired, nil
}

func (s *serveServer) configuredMCPState(ctx context.Context, sessionID string) (serveMCPSessionResponse, error) {
	manager, setting, err := mcpstart.LoadSessionManager(ctx, s.cfgRef, s.store, sessionID, s.cfg.agentName)
	if err != nil {
		return serveMCPSessionResponse{}, err
	}
	return buildServeMCPState(manager, nil, setting, sessionID), nil
}

// writeServeMCPFailure is the single mapper for manager and session MCP errors.
func writeServeMCPFailure(w http.ResponseWriter, err error) {
	status, kind := http.StatusInternalServerError, "server_error"
	var denied *mcp.PermissionError
	var apiErr *serveMCPError
	switch {
	case errors.As(err, &denied):
		status, kind = http.StatusForbidden, "permission_error"
	case errors.As(err, &apiErr):
		status, kind = apiErr.status, apiErr.errorType
	case errors.Is(err, errServeSessionBusy):
		status, kind = http.StatusConflict, "conflict_error"
	}
	writeOpenAIError(w, status, kind, err.Error())
}

func (s *serveServer) applyPersistedMCPSelectionLocked(ctx context.Context, sessionID string, rt *serveRuntime) error {
	if s == nil || rt == nil {
		return nil
	}
	var sess *session.Session
	if s.store != nil && strings.TrimSpace(sessionID) != "" {
		sess, _ = s.store.Get(ctx, sessionID)
	}
	desired, err := s.cleanPersistedMCPSelection(ctx, sess, rt)
	if err != nil {
		return err
	}
	current := normalizeMCPSelection(mcpstart.ParseServerList(rt.mcpSetting))
	if slices.Equal(desired, current) && rt.mcpSelectionReadyLocked(desired) {
		return nil
	}
	if err := rt.applyMCPSelectionLocked(ctx, desired); err != nil {
		rt.mcpSetting = strings.Join(desired, ",")
		return err
	}
	return nil
}
