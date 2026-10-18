package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	serveshell "github.com/samsaffron/term-llm/internal/app/serve/shell"
)

type serveShellCreateRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type serveShellIDRequest struct {
	ShellID string `json:"shell_id"`
}

type serveShellInputRequest struct {
	ShellID string `json:"shell_id"`
	Data    string `json:"data"`
}

type serveShellResizeRequest struct {
	ShellID string `json:"shell_id"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

type serveShellCollaborationRequest struct {
	ShellID string `json:"shell_id"`
	Enabled bool   `json:"enabled"`
}

type serveShellInterruptRequest struct {
	ShellID   string `json:"shell_id"`
	CommandID string `json:"command_id"`
}

func (s *serveServer) shellManager() (*serveShellManager, error) {
	s.shellsMu.Lock()
	defer s.shellsMu.Unlock()
	if s.shellsClosed {
		return nil, errServeShellClosed
	}
	if s.shells == nil {
		s.shells = serveshell.NewManager(30*time.Minute, s.shellSessionExists)
	}
	return s.shells, nil
}

func (s *serveServer) closeSessionShell(sessionID string) {
	s.shellsMu.Lock()
	manager := s.shells
	s.shellsMu.Unlock()
	if manager != nil {
		manager.CloseSession(sessionID)
	}
}

func (s *serveServer) closeShellManager() {
	s.shellsMu.Lock()
	if s.shellsClosed {
		s.shellsMu.Unlock()
		return
	}
	s.shellsClosed = true
	manager := s.shells
	s.shells = nil
	s.shellsMu.Unlock()
	if manager != nil {
		manager.Close()
	}
}

func (s *serveServer) shellSessionExists(sessionID string) bool {
	if s == nil || s.store == nil {
		return false
	}
	sess, err := s.store.Get(context.Background(), sessionID)
	return err == nil && sess != nil
}

func (s *serveServer) resolveShellCWD(ctx context.Context, sessionID string) (string, error) {
	if s.store == nil {
		return "", os.ErrNotExist
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "", os.ErrNotExist
	}
	var binding serveWorkspaceBinding
	if strings.TrimSpace(sess.ProjectID) != "" {
		binding, err = s.resolvePersistedProjectWorkspace(ctx, *sess)
	} else {
		binding, err = s.resolveWorkspace(ctx, serveWorkspaceRequest{
			SessionID: sessionID, FirstPartyUI: true, AllowNoProject: true,
		})
	}
	if err != nil {
		return "", err
	}
	cwd := strings.TrimSpace(binding.RuntimeDir)
	if cwd == "" {
		cwd = strings.TrimSpace(sess.WorktreeDir)
	}
	if cwd == "" {
		cwd = strings.TrimSpace(sess.CWD)
	}
	if cwd == "" {
		cwd = strings.TrimSpace(s.startupDir)
	}
	if cwd == "" {
		return "", errors.New("session workspace is unavailable")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("inspect session workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("session workspace is not a directory")
	}
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve session workspace: %w", err)
	}
	return absolute, nil
}

func sameOriginShellRequest(r *http.Request) bool {
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if fetchSite == "cross-site" {
		return false
	}
	// Direct, Hub-proxied, and reverse-Hub requests all originate as a
	// same-origin browser fetch. Hub transports deliberately replace the backend
	// Host, so the browser-owned Fetch Metadata signal is authoritative there.
	if fetchSite == "same-origin" {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	return origin == scheme+"://"+r.Host
}

func (s *serveServer) handleSessionShell(w http.ResponseWriter, r *http.Request, sessionID, suffix string) {
	if !platformServeShellSupported() || !s.cfg.ui {
		writeOpenAIError(w, http.StatusNotImplemented, "unsupported_error", "interactive shells are unavailable")
		return
	}
	if !sameOriginShellRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_origin", "shell requests must come from the first-party UI origin")
		return
	}
	switch suffix {
	case "shell":
		if r.Method == http.MethodPost {
			s.handleShellCreate(w, r, sessionID)
			return
		}
		if r.Method == http.MethodDelete {
			s.handleShellDelete(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST, DELETE")
	case "shell/stream":
		if r.Method == http.MethodGet {
			s.handleShellStream(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "GET")
	case "shell/input":
		if r.Method == http.MethodPost {
			s.handleShellInput(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST")
	case "shell/resize":
		if r.Method == http.MethodPost {
			s.handleShellResize(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST")
	case "shell/collaboration":
		if r.Method == http.MethodPost {
			s.handleShellCollaboration(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST")
	case "shell/interrupt":
		if r.Method == http.MethodPost {
			s.handleShellInterrupt(w, r, sessionID)
			return
		}
		w.Header().Set("Allow", "POST")
	}
	writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
}

func decodeServeShellJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, serveshell.InputBytes*2)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid shell request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid shell request")
		return false
	}
	return true
}

func (s *serveServer) handleShellCreate(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellCreateRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	if !serveshell.ValidSize(req.Cols, req.Rows) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid terminal size")
		return
	}
	cwd, err := s.resolveShellCWD(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			writeWorkspaceError(w, err)
		}
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, created, err := manager.Create(sessionID, cwd, req.Cols, req.Rows)
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	collaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
	writeJSON(w, status, map[string]any{
		"shell_id": shell.ID(), "cwd": shell.CWD(), "created": created, "state": "running",
		"collaboration": collaboration,
	})
}

func writeServeShellError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errServeShellClosed):
		writeOpenAIError(w, http.StatusServiceUnavailable, "shell_unavailable", "shell service is shutting down")
	case errors.Is(err, serveshell.ErrStale):
		writeOpenAIError(w, http.StatusConflict, "stale_shell", "shell generation is stale")
	case errors.Is(err, serveshell.ErrInputQueueFull):
		writeOpenAIError(w, http.StatusConflict, "shell_input_busy", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeOpenAIError(w, http.StatusConflict, "shell_input_rejected", "shell input could not be delivered")
	case errors.Is(err, io.ErrClosedPipe):
		writeOpenAIError(w, http.StatusConflict, "shell_exited", "shell has exited")
	default:
		writeOpenAIError(w, http.StatusInternalServerError, "shell_error", err.Error())
	}
}

func (s *serveServer) handleShellInput(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellInputRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil || len(data) == 0 || len(data) > serveshell.InputBytes {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid shell input")
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.Get(sessionID, strings.TrimSpace(req.ShellID))
	if err == nil {
		err = shell.WriteBrowserInput(r.Context(), data)
	}
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(data)})
}

func (s *serveServer) handleShellResize(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellResizeRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	if !serveshell.ValidSize(req.Cols, req.Rows) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid terminal size")
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.Get(sessionID, strings.TrimSpace(req.ShellID))
	if err == nil {
		err = shell.Resize(req.Cols, req.Rows)
	}
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *serveServer) handleShellDelete(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellIDRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	if err := manager.CloseShell(sessionID, strings.TrimSpace(req.ShellID)); err != nil {
		writeServeShellError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeShellSSE(w io.Writer, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func (s *serveServer) handleShellStream(w http.ResponseWriter, r *http.Request, sessionID string) {
	shellID := strings.TrimSpace(r.URL.Query().Get("shell_id"))
	offset, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("offset")), 10, 64)
	if err != nil || offset < 0 || shellID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "shell_id and a non-negative offset are required")
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.Get(sessionID, shellID)
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	ctx, cancel := s.contextWithShutdown(r.Context())
	defer cancel()
	stream := newStreamingResponseWriter(w, serveStreamWriteTimeout)
	flusher, ok := stream.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is unsupported")
		return
	}
	setSSEHeaders(stream)
	stream.Header().Set("Cache-Control", "no-cache, no-transform")
	initial := shell.Snapshot(offset)
	collaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
	eventSequence := collaboration.Sequence
	if err := writeShellSSE(stream, "ready", map[string]any{
		"shell_id": shell.ID(), "cwd": shell.CWD(), "offset": offset,
		"base_offset": initial.BaseOffset, "next_offset": initial.NextOffset,
		"heartbeat_ms": serveshell.Heartbeat.Milliseconds(), "replay_bytes": serveshell.ReplayBytes,
		"sequence": eventSequence, "collaboration": collaboration,
	}); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(serveshell.Heartbeat)
	defer heartbeat.Stop()
	exitSent := false
	for {
		snapshot := shell.Snapshot(offset)
		events, overrun, _ := shell.CollaborationEventsAfter(eventSequence)
		if overrun {
			collaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
			if err := writeShellSSE(stream, "collaboration", collaboration); err != nil {
				return
			}
			eventSequence = collaboration.Sequence
			flusher.Flush()
			continue
		}
		if len(events) > 0 {
			for _, event := range events {
				if err := writeShellSSE(stream, event.Type, event); err != nil {
					return
				}
				eventSequence = event.Sequence
			}
			flusher.Flush()
			continue
		}
		if snapshot.Reset {
			if err := writeShellSSE(stream, "reset", map[string]any{"offset": snapshot.BaseOffset, "reason": "replay_window"}); err != nil {
				return
			}
			offset = snapshot.BaseOffset
			flusher.Flush()
			continue
		}
		if len(snapshot.Data) > 0 {
			next := snapshot.DataOffset + int64(len(snapshot.Data))
			if err := writeShellSSE(stream, "output", map[string]any{
				"offset": snapshot.DataOffset, "next_offset": next,
				"data": base64.StdEncoding.EncodeToString(snapshot.Data),
			}); err != nil {
				return
			}
			offset = next
			flusher.Flush()
			continue
		}
		if snapshot.Exited && !exitSent {
			finalCollaboration := s.shellCollaborationSnapshot(r.Context(), sessionID, shell)
			if err := writeShellSSE(stream, "collaboration", finalCollaboration); err != nil {
				return
			}
			if err := writeShellSSE(stream, "exit", map[string]any{
				"shell_id": shell.ID(), "offset": snapshot.NextOffset, "exit_code": snapshot.ExitCode,
				"collaboration": finalCollaboration,
			}); err != nil {
				return
			}
			flusher.Flush()
			exitSent = true
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-snapshot.Changed:
		case <-heartbeat.C:
			if _, err := io.WriteString(stream, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
