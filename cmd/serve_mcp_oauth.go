package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/mcp"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

type serveMCPOAuthStartRequest struct {
	Force bool `json:"force"`
}

type serveMCPOAuthCancelRequest struct {
	FlowID string `json:"flow_id"`
}

func parseSessionMCPOAuthSuffix(suffix string) (server, action string, ok bool) {
	parts := strings.Split(suffix, "/")
	if len(parts) < 3 || parts[0] != "mcp" || parts[1] == "" || parts[2] != "oauth" {
		return "", "", false
	}
	decoded, err := url.PathUnescape(parts[1])
	if err != nil || decoded == "" || strings.Contains(decoded, "/") {
		return "", "", false
	}
	if len(parts) == 3 {
		return decoded, "logout", true
	}
	if len(parts) == 4 && (parts[3] == "start" || parts[3] == "cancel") {
		return decoded, parts[3], true
	}
	return "", "", false
}

func (s *serveServer) handleSessionMCPOAuth(w http.ResponseWriter, r *http.Request, sessionID, serverName, action string) {
	wantMethod := http.MethodPost
	if action == "logout" {
		wantMethod = http.MethodDelete
	}
	if r.Method != wantMethod {
		w.Header().Set("Allow", wantMethod)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session runtime is unavailable")
		return
	}
	rt, err := s.sessionMgr.GetOrCreate(r.Context(), sessionID)
	if err != nil || rt == nil {
		status := http.StatusInternalServerError
		if err != nil && strings.Contains(err.Error(), "busy") {
			status = http.StatusConflict
		}
		writeOpenAIError(w, status, "server_error", "session runtime is unavailable")
		return
	}
	if rt.hasActiveRun() || !rt.mu.TryLock() {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot change MCP authentication while a response is running")
		return
	}
	defer rt.mu.Unlock()
	if rt.hasActiveRun() {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot change MCP authentication while a response is running")
		return
	}
	if err := rt.ensureMCPManagerLocked(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if _, ok := rt.mcpManager.Config().Servers[serverName]; !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "MCP server is not configured")
		return
	}

	switch action {
	case "start":
		var req serveMCPOAuthStartRequest
		if r.Body != nil && r.ContentLength != 0 {
			if err := decodeJSONBody(r, &req); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
		}
		redirectURL, err := s.mcpOAuthCallbackURL(r)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		startCtx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		selected := containsMCPServer(parseServerList(rt.mcpSetting), serverName)
		if !selected && s.store != nil {
			if session, getErr := s.store.Get(r.Context(), sessionID); getErr == nil && session != nil {
				selected = containsMCPServer(parseServerList(session.MCP), serverName)
			}
		}
		flow, err := rt.mcpManager.StartOAuth(startCtx, serverName, mcp.OAuthStartOptions{
			RedirectURL: redirectURL, Force: req.Force, SkipReconnect: true,
		})
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "oauth_error", safeServeOAuthError(err))
			return
		}
		go s.publishMCPOAuthCompletion(sessionID, serverName, flow.ID, rt.mcpManager, selected)
		writeJSON(w, http.StatusAccepted, flow)
	case "cancel":
		var req serveMCPOAuthCancelRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if req.FlowID == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "flow_id is required")
			return
		}
		if err := rt.mcpManager.CancelOAuth(serverName, req.FlowID); err != nil {
			writeOpenAIError(w, http.StatusConflict, "oauth_error", safeServeOAuthError(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"canceled": true})
	case "logout":
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := rt.mcpManager.LogoutOAuth(ctx, serverName, false); err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "oauth_error", safeServeOAuthError(err))
			return
		}
		s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "mcp_oauth"})
		writeJSON(w, http.StatusOK, map[string]bool{"signed_out": true})
	}
}

func (s *serveServer) publishMCPOAuthCompletion(sessionID, serverName, flowID string, manager *mcp.Manager, selected bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	flow, err := mcpoauth.DefaultCoordinator().Wait(ctx, flowID)
	if err == nil && flow != nil && flow.State == mcpoauth.FlowSucceeded {
		if selected && manager != nil {
			_ = manager.Restart(context.Background(), serverName)
		}
		s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "mcp_oauth"})
	}
}

func containsMCPServer(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}

func (s *serveServer) handleMCPOAuthFlow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/mcp/oauth/flows/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	flow, ok := mcpoauth.DefaultCoordinator().Flow(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "OAuth flow was not found")
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

func (s *serveServer) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flowID, accepted := mcpoauth.DefaultCoordinator().CompleteCallback(
		r.URL.Query().Get("state"), r.URL.Query().Get("code"),
		r.URL.Query().Get("iss"), r.URL.Query().Get("error"),
	)
	if !accepted {
		http.Error(w, "This authorization callback is invalid, expired, or was already used.", http.StatusBadRequest)
		return
	}
	ok := r.URL.Query().Get("error") == "" && r.URL.Query().Get("code") != ""
	flowJSON, _ := json.Marshal(flowID)
	okJSON, _ := json.Marshal(ok)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	heading := "Connected"
	detail := "You can close this window."
	if !ok {
		heading, detail = "Authorization not completed", "Return to term-llm and try again."
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><style>body{font:16px system-ui;margin:3rem;max-width:38rem}h1{font-size:1.5rem}</style><h1>%s</h1><p>%s</p><script>if(window.opener){window.opener.postMessage({type:"term-llm-mcp-oauth",flow_id:%s,ok:%s},window.location.origin)}</script>`, html.EscapeString(heading), html.EscapeString(heading), html.EscapeString(detail), flowJSON, okJSON)
}

func (s *serveServer) mcpOAuthCallbackURL(r *http.Request) (string, error) {
	const callbackPath = "/v1/mcp/oauth/callback"
	if s.cfg.publicURL != "" {
		return strings.TrimRight(s.cfg.publicURL, "/") + callbackPath, nil
	}
	if r.Host == "" {
		return "", fmt.Errorf("request host is missing; configure serve --public-url")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + s.cfg.basePath + callbackPath, nil
}

func safeServeOAuthError(err error) string {
	if err == nil {
		return ""
	}
	text := terminaltext.SanitizeSingleLine(err.Error())
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}
