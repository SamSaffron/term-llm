package cmd

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) verboseLog(format string, args ...any) {
	if s.cfg.verbose {
		log.Printf("[verbose] "+format, args...)
	}
}

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *serveServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ui {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	// basePath is already stripped by http.StripPrefix; URL.Path is "/" or "/session-id" etc.
	assetName := strings.TrimPrefix(r.URL.Path, "/")
	if assetName == "index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if strings.Contains(r.URL.RawQuery, "v=") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		_, _ = w.Write(s.renderIndexHTML())
		return
	}
	if assetName == "manifest.webmanifest" {
		w.Header().Set("Content-Type", "application/manifest+json")
		if strings.Contains(r.URL.RawQuery, "v=") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		_, _ = w.Write(serveui.RenderManifest())
		return
	}
	if assetName == "sw.js" {
		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(serveui.RenderServiceWorker())
		return
	}
	if assetName != "" && !strings.Contains(assetName, "..") {
		if data, err := serveui.StaticAsset(assetName); err == nil {
			contentType := mime.TypeByExtension(filepath.Ext(assetName))
			if contentType == "" {
				// mime.TypeByExtension may return empty for .woff2 on some systems.
				switch filepath.Ext(assetName) {
				case ".woff2":
					contentType = "font/woff2"
				default:
					contentType = http.DetectContentType(data)
				}
			}
			w.Header().Set("Content-Type", contentType)
			if strings.Contains(r.URL.RawQuery, "v=") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			_, _ = w.Write(data)
			return
		}
	}

	// SPA catch-all: serve index.html for all other paths.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write(s.renderIndexHTML())
}

func (s *serveServer) renderIndexHTML() []byte {
	// Inject UI prefix so JS can prefix all API calls with it.
	// Also inject VAPID public key for web push if configured.
	var headSnippet string
	escaped, _ := json.Marshal(s.cfg.basePath)
	headSnippet += `<script>window.TERM_LLM_UI_PREFIX=` + string(escaped) + `;</script>`
	sidebarSessions := s.cfg.sidebarSessions
	if len(sidebarSessions) == 0 {
		sidebarSessions = []string{"all"}
	}
	sidebarEscaped, _ := json.Marshal(sidebarSessions)
	headSnippet += `<script>window.TERM_LLM_SIDEBAR_SESSIONS=` + string(sidebarEscaped) + `;</script>`
	if s.cfgRef != nil {
		if vapidKey := s.cfgRef.Serve.WebPush.VAPIDPublicKey; vapidKey != "" {
			vapidEscaped, _ := json.Marshal(vapidKey)
			headSnippet += `<script>window.TERM_LLM_VAPID_PUBLIC_KEY=` + string(vapidEscaped) + `;</script>`
		}
	}
	headSnippet += s.webrtcHeadSnippet
	return serveui.RenderIndexHTML(s.cfg.basePath, headSnippet)
}

func (s *serveServer) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	// basePath is already stripped by http.StripPrefix; URL.Path is "/images/filename".
	filename := strings.TrimPrefix(r.URL.Path, "/images/")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		http.NotFound(w, r)
		return
	}

	outputDir := image.ExpandPath(s.cfgRef.Image.OutputDir)
	if outputDir == "" {
		outputDir = image.ExpandPath("~/Pictures/term-llm")
	}

	filePath := filepath.Join(outputDir, filename)
	absDir, err := filepath.EvalSymlinks(outputDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	absFile, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(absFile, absDir+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Add("Vary", "Authorization, Cookie")
	http.ServeFile(w, r, absFile)
}

// ensureImageServeable ensures the given image path is under the serveable
// image output directory. If the file is already there, the path is returned
// as-is. Otherwise the file is copied into the output dir so that the
// images handler (mounted at basePath/images/) can serve it. Returns the
// serveable path and true on success, or ("", false) if the image could not
// be made serveable.
func (s *serveServer) ensureImageServeable(imgPath string) (string, bool) {
	outputDir := image.ExpandPath(s.cfgRef.Image.OutputDir)
	if outputDir == "" {
		outputDir = image.ExpandPath("~/Pictures/term-llm")
	}

	absDir, err := filepath.Abs(outputDir)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: abs(%s): %v", outputDir, err)
		return "", false
	}
	absImg, err := filepath.Abs(imgPath)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: abs(%s): %v", imgPath, err)
		return "", false
	}

	// Already under the output dir — nothing to do.
	if strings.HasPrefix(absImg, absDir+string(filepath.Separator)) {
		return imgPath, true
	}

	// Copy the file into the output dir with a unique prefix to avoid collisions.
	if err := os.MkdirAll(absDir, 0755); err != nil {
		log.Printf("[serve] ensureImageServeable: mkdir %s: %v", absDir, err)
		return "", false
	}

	src, err := os.Open(absImg)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: open %s: %v", absImg, err)
		return "", false
	}
	defer src.Close()

	destName := fmt.Sprintf("serve-%s-%s", randomSuffix(), filepath.Base(absImg))
	destPath := filepath.Join(absDir, destName)
	dst, err := os.Create(destPath)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: create %s: %v", destPath, err)
		return "", false
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(destPath)
		log.Printf("[serve] ensureImageServeable: copy to %s: %v", destPath, err)
		return "", false
	}
	if err := dst.Close(); err != nil {
		os.Remove(destPath)
		log.Printf("[serve] ensureImageServeable: close %s: %v", destPath, err)
		return "", false
	}

	return destPath, true
}

func (s *serveServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	categories, err := parseSidebarSessionCategories(r.URL.Query().Get("categories"), false)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")

	sessions, err := s.store.List(r.Context(), session.ListOptions{
		Limit:      100,
		Archived:   includeArchived,
		Categories: categories,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list sessions")
		return
	}

	type sessionEntry struct {
		ID         string                `json:"id"`
		Number     int64                 `json:"number,omitempty"`
		Name       string                `json:"name,omitempty"`
		ShortTitle string                `json:"short_title"`
		LongTitle  string                `json:"long_title"`
		Mode       session.SessionMode   `json:"mode,omitempty"`
		Origin     session.SessionOrigin `json:"origin,omitempty"`
		Archived   bool                  `json:"archived"`
		Pinned     bool                  `json:"pinned"`
		CreatedAt  int64                 `json:"created_at"`
		MsgCount   int                   `json:"message_count"`
	}

	result := make([]sessionEntry, 0, len(sessions))
	for _, sess := range sessions {
		result = append(result, sessionEntry{
			Name:       sess.Name,
			ID:         sess.ID,
			Number:     sess.Number,
			ShortTitle: sess.PreferredShortTitle(),
			LongTitle:  sess.PreferredLongTitle(),
			Mode:       sess.Mode,
			Origin:     sess.Origin,
			Archived:   sess.Archived,
			Pinned:     sess.Pinned,
			CreatedAt:  sess.CreatedAt.UnixMilli(),
			MsgCount:   sess.MessageCount,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": result})
}

func (s *serveServer) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	// Parse /v1/sessions/{id}/...
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	sessionID := parts[0]
	// If the path segment is purely numeric, resolve via session number.
	if num, err := strconv.ParseInt(sessionID, 10, 64); err == nil && num > 0 && s.store != nil {
		sess, err := s.store.GetByNumber(r.Context(), num)
		if err != nil || sess == nil {
			http.NotFound(w, r)
			return
		}
		sessionID = sess.ID
	}
	suffix := ""
	if len(parts) > 1 {
		suffix = parts[1]
	}

	if suffix == "" && r.Method == http.MethodPatch {
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionMetadataPatch(w, r, sessionID)
		return
	}

	if suffix == "interrupt" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionInterrupt(w, r, sessionID)
		return
	}

	if suffix == "ask_user" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionAskUser(w, r, sessionID)
		return
	}

	if suffix == "approval" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionApproval(w, r, sessionID)
		return
	}

	if suffix == "state" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		s.handleSessionState(w, r, sessionID)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	if suffix != "messages" {
		http.NotFound(w, r)
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	msgs, err := s.store.GetMessages(r.Context(), sessionID, limit, offset)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get messages")
		return
	}

	type partEntry struct {
		Type       string `json:"type"`
		Text       string `json:"text,omitempty"`
		ToolName   string `json:"tool_name,omitempty"`
		ToolArgs   string `json:"tool_arguments,omitempty"`
		ToolCallID string `json:"tool_call_id,omitempty"`
		ImageURL   string `json:"image_url,omitempty"`
		MimeType   string `json:"mime_type,omitempty"`
	}

	type messageEntry struct {
		Role      string      `json:"role"`
		Parts     []partEntry `json:"parts"`
		CreatedAt int64       `json:"created_at"`
	}

	result := make([]messageEntry, 0, len(msgs))
	for _, msg := range msgs {
		// System messages contain the internal system prompt — never expose to UI clients.
		if msg.Role == llm.RoleSystem {
			continue
		}
		entry := messageEntry{
			Role:      string(msg.Role),
			CreatedAt: msg.CreatedAt.UnixMilli(),
		}
		for _, p := range msg.Parts {
			switch p.Type {
			case llm.PartText:
				if p.Text != "" {
					entry.Parts = append(entry.Parts, partEntry{
						Type: "text",
						Text: p.Text,
					})
				}
			case llm.PartImage:
				if p.ImageData != nil && p.ImageData.Base64 != "" {
					entry.Parts = append(entry.Parts, partEntry{
						Type:     "image",
						ImageURL: "data:" + p.ImageData.MediaType + ";base64," + p.ImageData.Base64,
						MimeType: p.ImageData.MediaType,
					})
				}
			case llm.PartToolCall:
				if p.ToolCall != nil {
					pe := partEntry{
						Type:       "tool_call",
						ToolName:   p.ToolCall.Name,
						ToolCallID: p.ToolCall.ID,
					}
					if len(p.ToolCall.Arguments) > 0 {
						pe.ToolArgs = string(p.ToolCall.Arguments)
					}
					entry.Parts = append(entry.Parts, pe)
				}
			case llm.PartToolResult:
				// Omitted: UI ignores tool_result parts and they bloat payloads.
			}
		}
		if len(entry.Parts) == 0 {
			entry.Parts = []partEntry{}
		}
		result = append(result, entry)
	}

	writeJSON(w, http.StatusOK, map[string]any{"messages": result})
}

func (s *serveServer) handleSessionInterrupt(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionInterruptRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "message is required")
		return
	}

	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	if s.responseRuns != nil {
		if runID := s.responseRuns.activeRunID(sessionID); runID != "" {
			if run, ok := s.responseRuns.get(runID); ok {
				run.disableCompaction()
			}
		}
	}

	fastProvider, fastErr := llm.NewFastProvider(s.cfgRef, rt.providerKey)
	if fastErr != nil {
		log.Printf("[serve] fast provider unavailable for interrupt: %v", fastErr)
	}
	action, interruptErr := rt.Interrupt(r.Context(), msg, fastProvider)
	if interruptErr != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", interruptErr.Error())
		return
	}

	actionName := "interject"
	switch action {
	case llm.InterruptCancel:
		actionName = "cancel"
	case llm.InterruptInterject:
		actionName = "interject"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": actionName,
	})
}

func (s *serveServer) handleSessionMetadataPatch(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}

	var req struct {
		Name     *string `json:"name"`
		Archived *bool   `json:"archived"`
		Pinned   *bool   `json:"pinned"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load session")
		return
	}
	if sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}

	if req.Name != nil {
		sess.Name = strings.TrimSpace(*req.Name)
	}
	if req.Archived != nil {
		sess.Archived = *req.Archived
	}
	if req.Pinned != nil {
		sess.Pinned = *req.Pinned
	}
	if err := s.store.Update(r.Context(), sess); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to update session")
		return
	}

	if s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok {
			rt.mu.Lock()
			if rt.sessionMeta != nil {
				rt.sessionMeta.Name = sess.Name
				rt.sessionMeta.Archived = sess.Archived
				rt.sessionMeta.Pinned = sess.Pinned
				rt.sessionMeta.Origin = sess.Origin
			}
			rt.mu.Unlock()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          sess.ID,
		"name":        sess.Name,
		"short_title": sess.PreferredShortTitle(),
		"long_title":  sess.PreferredLongTitle(),
		"mode":        sess.Mode,
		"origin":      sess.Origin,
		"archived":    sess.Archived,
		"pinned":      sess.Pinned,
		"created_at":  sess.CreatedAt.UnixMilli(),
	})
}

func (s *serveServer) auth(next http.HandlerFunc) http.HandlerFunc {
	if !s.cfg.requireAuth {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}

		var gotToken string
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, prefix) {
			gotToken = strings.TrimSpace(strings.TrimPrefix(auth, prefix))
		}
		if gotToken == "" {
			if xKey := strings.TrimSpace(r.Header.Get("x-api-key")); xKey != "" {
				gotToken = xKey
			}
		}
		if gotToken == "" && r.Method == http.MethodGet {
			// Cookie fallback only on safe GET requests (e.g. <img src> fetches
			// that cannot set Authorization headers).
			if cookie, err := r.Cookie("term_llm_token"); err == nil && cookie.Value != "" {
				if decoded, decErr := url.QueryUnescape(cookie.Value); decErr == nil {
					gotToken = decoded
				} else {
					gotToken = cookie.Value
				}
			}
		}

		if gotToken == "" || subtle.ConstantTimeCompare([]byte(gotToken), []byte(s.cfg.token)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid authentication credentials")
			return
		}
		next(w, r)
	}
}

func (s *serveServer) cors(next http.HandlerFunc) http.HandlerFunc {
	allowed := make(map[string]struct{}, len(s.cfg.corsOrigins))
	allowAll := false
	for _, origin := range s.cfg.corsOrigins {
		o := strings.TrimSpace(origin)
		if o == "" {
			continue
		}
		if o == "*" {
			allowAll = true
			continue
		}
		allowed[o] = struct{}{}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, session_id, X-Term-LLM-UI, X-API-Key, anthropic-version")
			w.Header().Set("Access-Control-Expose-Headers", "x-session-id, x-session-number")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

func (s *serveServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	providers := buildProviderList(s.cfgRef)
	items := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		if !p.Configured && !p.IsBuiltin {
			continue
		}
		models := p.Models
		if models == nil {
			models = []string{}
		}
		items = append(items, map[string]any{
			"name":       p.Name,
			"type":       p.Type,
			"models":     models,
			"configured": p.Configured,
			"is_builtin": p.IsBuiltin,
			"is_default": p.Name == s.cfgRef.DefaultProvider,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

func (s *serveServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	queryProvider := strings.TrimSpace(r.URL.Query().Get("provider"))
	provider, effectiveName, err := s.getModelsProvider(queryProvider)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	models := make([]llm.ModelInfo, 0)
	if lister, ok := provider.(interface {
		ListModels(context.Context) ([]llm.ModelInfo, error)
	}); ok {
		if listed, err := lister.ListModels(ctx); err == nil {
			models = listed
		}
	}

	if len(models) == 0 {
		if pc, ok := s.cfgRef.Providers[effectiveName]; ok {
			if pc.Model != "" {
				models = append(models, llm.ModelInfo{ID: pc.Model})
			}
		} else if queryProvider == "" {
			if providerCfg := s.cfgRef.GetActiveProviderConfig(); providerCfg != nil {
				if providerCfg.Model != "" {
					models = append(models, llm.ModelInfo{ID: providerCfg.Model})
				}
			}
		}
		if curated, ok := llm.ProviderModels[effectiveName]; ok {
			for _, id := range curated {
				models = append(models, llm.ModelInfo{ID: id})
			}
		}
	}

	seen := map[string]bool{}
	items := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		items = append(items, map[string]any{
			"id":      m.ID,
			"object":  "model",
			"created": m.Created,
			"owned_by": func() string {
				if m.OwnedBy != "" {
					return m.OwnedBy
				}
				return "term-llm"
			}(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		idi, _ := items[i]["id"].(string)
		idj, _ := items[j]["id"].(string)
		return idi < idj
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

func (s *serveServer) getModelsProvider(name string) (llm.Provider, string, error) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if s.modelsProviders == nil {
		s.modelsProviders = make(map[string]llm.Provider)
	}

	cacheKey := name
	if cacheKey == "" {
		cacheKey = s.cfgRef.DefaultProvider
	}

	if p, ok := s.modelsProviders[cacheKey]; ok {
		return p, cacheKey, nil
	}

	var provider llm.Provider
	var err error
	if name == "" || name == s.cfgRef.DefaultProvider {
		provider, err = llm.NewProvider(s.cfgRef)
	} else {
		provider, err = llm.NewProviderByName(s.cfgRef, name, "")
	}
	if err != nil {
		return nil, "", err
	}
	s.modelsProviders[cacheKey] = provider
	return provider, cacheKey, nil
}

// sseKeepalive starts a background goroutine that writes an SSE comment ping
// to w every interval while streaming is active. This prevents intermediate
// proxies (e.g. nginx with a short send_timeout) from closing the connection
// during silent periods — e.g. when the LLM is in extended thinking mode or
// the API is slow to emit tokens.
//
// The returned mu must wrap all writes to w inside the RunWithEvents callback.
// Call stop() immediately after RunWithEvents returns; it blocks until the
// goroutine has exited so subsequent final writes to w are safe without a lock.
func sseKeepalive(w http.ResponseWriter, flusher http.Flusher, interval time.Duration) (mu *sync.Mutex, stop func()) {
	mu = &sync.Mutex{}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				_, _ = io.WriteString(w, ": ping\n\n")
				flusher.Flush()
				mu.Unlock()
			case <-done:
				return
			}
		}
	}()
	return mu, func() {
		close(done)
		wg.Wait()
	}
}

// registerResponseID stores a response ID on the runtime and server-wide map,
// pruning old IDs that exceed the per-session cap.
func (s *serveServer) registerResponseID(rt *serveRuntime, respID, sessionID string) {
	pruned := rt.addResponseID(respID)
	s.responseToSession.Store(respID, sessionID)
	for _, old := range pruned {
		s.responseToSession.Delete(old)
	}
}

func (s *serveServer) runtimeForRequest(ctx context.Context, sessionID string) (*serveRuntime, bool, error) {
	if sessionID == "" {
		// Ephemeral stateless runtime (fresh per request for isolation)
		rt, err := s.sessionMgr.factory(ctx)
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	// Stateful sessions should outlive a single HTTP request context.
	rt, err := s.sessionMgr.GetOrCreate(context.Background(), sessionID)
	if err != nil {
		return nil, false, err
	}
	return rt, true, nil
}

// runtimeForProviderRequest creates a runtime using a specific (non-default) provider.
func (s *serveServer) runtimeForProviderRequest(ctx context.Context, sessionID string, providerName string) (*serveRuntime, bool, error) {
	if s.runtimeFactory == nil {
		return s.runtimeForRequest(ctx, sessionID)
	}
	if sessionID == "" {
		rt, err := s.runtimeFactory(ctx, providerName, "")
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	// Use GetOrCreateWith to get proper in-flight deduplication.
	rt, err := s.sessionMgr.GetOrCreateWith(context.Background(), sessionID, func(ctx context.Context) (*serveRuntime, error) {
		return s.runtimeFactory(ctx, providerName, "")
	})
	if err != nil {
		return nil, false, err
	}
	return rt, true, nil
}

func (s *serveServer) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "session store not available")
		return
	}

	switch r.Method {
	case http.MethodPost:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256DH string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if req.Endpoint == "" || req.Keys.P256DH == "" || req.Keys.Auth == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "endpoint and keys (p256dh, auth) are required")
			return
		}
		sub := &session.PushSubscription{
			Endpoint:  req.Endpoint,
			KeyP256DH: req.Keys.P256DH,
			KeyAuth:   req.Keys.Auth,
		}
		if err := s.store.SavePushSubscription(r.Context(), sub); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to save subscription")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true})

	case http.MethodDelete:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req struct {
			Endpoint string `json:"endpoint"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if req.Endpoint == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "endpoint is required")
			return
		}
		if err := s.store.DeletePushSubscription(r.Context(), req.Endpoint); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to delete subscription")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// POST /v1/messages — Anthropic Messages API
// ---------------------------------------------------------------------------
