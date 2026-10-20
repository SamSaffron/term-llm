package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiPrefix = "/demo-api"

// Handler returns the browser demo's HTTP surface. It is intentionally a small
// subset of the serve API: enough for the genuine chat UI to boot, list
// sessions, stream responses, and stop a run.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/v1/capabilities", a.handleCapabilities)
	mux.HandleFunc("/v1/providers", a.handleProviders)
	mux.HandleFunc("/v1/models", a.handleModels)
	mux.HandleFunc("/v1/sidebar", a.handleSessions)
	mux.HandleFunc("/v1/sessions", a.handleSessions)
	mux.HandleFunc("/v1/sessions/status", a.handleSessionStatus)
	mux.HandleFunc("/v1/sessions/search", a.handleSessionSearch)
	mux.HandleFunc("/v1/sessions/pinned-order", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": []any{}})
	})
	mux.HandleFunc("/v1/sessions/", a.handleSessionByID)
	mux.HandleFunc("/v1/responses", a.handleCreateResponse)
	mux.HandleFunc("/v1/responses/", a.handleResponseByID)
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/v1/events/poll", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "next_cursor": 0})
	})
	mux.HandleFunc("/v1/sharing/capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "reasons": []string{"browser demo"}})
	})
	mux.HandleFunc("/v1/mcp/servers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"servers": []any{}, "enabled": []string{}})
	})
	mux.HandleFunc("/v1/mcp/catalogue", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"servers": []any{}})
	})
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"projects": []any{}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"message": "not found: " + r.URL.Path, "type": "invalid_request_error",
		}})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiPrefix) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, apiPrefix)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *App) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"projects":  map[string]any{"enabled": false},
		"worktrees": map[string]any{"enabled": false},
		"shell":     map[string]any{"enabled": false},
		"live":      map[string]any{"enabled": false},
		"attachments": map[string]any{
			"max_count":  4,
			"max_bytes":  10 * 1024 * 1024,
			"mime_types": []string{"text/plain", "text/markdown", "application/json"},
			"extensions": []string{".txt", ".md", ".json", ".go", ".js", ".ts", ".css", ".html"},
		},
	})
}

func (a *App) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
		"id":                   demoProviderName,
		"name":                 demoProviderName,
		"display_name":         "Bonsai (WebGPU)",
		"is_default":           true,
		"default_model":        demoModelName,
		"requires_credentials": false,
		"available":            true,
		"models": []any{map[string]any{
			"id": demoModelName, "name": demoModelLabel, "display_name": demoModelLabel,
		}},
	}}})
}

func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
		"id": demoModelName, "name": demoModelLabel, "display_name": demoModelLabel,
		"provider": demoProviderName,
	}}})
}

func (a *App) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.handleCreateSession(w, r)
		return
	case http.MethodGet:
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	selected := strings.TrimSpace(r.URL.Query().Get("selected_session"))
	if r.URL.Query().Get("selected_only") == "1" {
		a.writeSelectedSession(w, selected)
		return
	}
	sessions := a.listSessions()
	items := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, a.sessionJSON(session, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": items, "next_cursor": ""})
}

func (a *App) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Agent    string `json:"agent"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	session := a.createSession("New chat")
	writeJSON(w, http.StatusOK, map[string]any{"session": a.sessionJSON(session, false)})
}

func (a *App) writeSelectedSession(w http.ResponseWriter, id string) {
	session := a.session(id)
	if session == nil {
		writeJSON(w, http.StatusOK, map[string]any{"selected_session": nil, "selected_transcript": nil})
		return
	}
	rev := session.TranscriptRev
	writeJSON(w, http.StatusOK, map[string]any{
		"selected_session": a.sessionJSON(session, false),
		"selected_transcript": map[string]any{
			"rev": rev,
			"index": map[string]any{"rows": map[string]any{
				"ids":   a.messageIDs(session),
				"roles": a.messageRoles(session),
			}},
			"bodies": map[string]any{"rev": rev, "messages": a.messagesJSON(session)},
		},
	})
}

func (a *App) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	items := make([]map[string]any, 0, len(a.order))
	for _, id := range a.order {
		session := a.sessions[id]
		if session == nil {
			continue
		}
		run := a.runs[id]
		clientMessageID := ""
		for i := len(session.Messages) - 1; i >= 0; i-- {
			if session.Messages[i].Role == "user" {
				clientMessageID = session.Messages[i].ClientMessageID
				break
			}
		}
		entry := map[string]any{
			"id":             session.ID,
			"active_run":     run != nil,
			"transcript_rev": session.TranscriptRev,
		}
		if run != nil {
			entry["active_response_id"] = run.ResponseID
		}
		if clientMessageID != "" {
			entry["client_message_id"] = clientMessageID
		}
		items = append(items, entry)
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
}

func (a *App) handleSessionSearch(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": []any{}, "results": []any{}})
}

func (a *App) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.Split(rest, "/")
	id, err := url.PathUnescape(parts[0])
	if err != nil || id == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "session not found"}})
		return
	}
	suffix := ""
	if len(parts) > 1 {
		suffix = strings.Join(parts[1:], "/")
	}
	a.mu.Lock()
	session := a.sessions[id]
	a.mu.Unlock()
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "session not found", "type": "session_not_found"}})
		return
	}
	switch {
	case suffix == "" && r.Method == http.MethodDelete:
		a.mu.Lock()
		delete(a.sessions, id)
		for i, existing := range a.order {
			if existing == id {
				a.order = append(a.order[:i], a.order[i+1:]...)
				break
			}
		}
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case suffix == "" && r.Method == http.MethodPatch:
		a.handlePatchSession(w, r, session)
	case suffix == "state":
		a.writeSessionState(w, session)
	case suffix == "skills":
		writeJSON(w, http.StatusOK, map[string]any{"skills": []any{}})
	case suffix == "children":
		writeJSON(w, http.StatusOK, map[string]any{"children": []any{}})
	case suffix == "stats":
		writeJSON(w, http.StatusOK, map[string]any{
			"metrics": map[string]any{"input_tokens": 0, "output_tokens": 0},
			"models":  []any{}, "sections": []any{}, "scope": "runtime_local",
		})
	case suffix == "inspect":
		writeJSON(w, http.StatusOK, map[string]any{"session_id": session.ID, "messages": []any{}})
	case strings.HasPrefix(suffix, "transcript/bodies"):
		a.mu.Lock()
		rev := session.TranscriptRev
		messages := a.messagesJSON(session)
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "messages": messages})
	case strings.HasPrefix(suffix, "attention/"):
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case strings.HasPrefix(suffix, "title/refine"):
		writeJSON(w, http.StatusOK, map[string]any{"title": session.Title})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "not found: " + suffix}})
	}
}

func (a *App) handlePatchSession(w http.ResponseWriter, r *http.Request, session *demoSession) {
	var body struct {
		Name     *string `json:"name"`
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
		Pinned   *bool   `json:"pinned"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	a.mu.Lock()
	if body.Name != nil {
		session.Name = *body.Name
		session.Title = *body.Name
	}
	if body.Title != nil {
		session.Title = *body.Title
	}
	if body.Archived != nil {
		session.Archived = *body.Archived
	}
	if body.Pinned != nil {
		session.Pinned = *body.Pinned
	}
	session.Updated = time.Now()
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, a.sessionJSON(session, false))
}

func (a *App) writeSessionState(w http.ResponseWriter, session *demoSession) {
	a.mu.Lock()
	run := a.runs[session.ID]
	lastResponse := session.LastResponse
	activeResponse := ""
	active := false
	if run != nil {
		active = true
		activeResponse = run.ResponseID
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"last_response_id":   lastResponse,
		"active_response_id": activeResponse,
		"active_run":         active,
		"approval_policy": map[string]any{
			"default_mode": "yolo", "requested_mode": "yolo", "effective_mode": "yolo",
			"controls_available": false, "guardian_available": false, "guardian_auto_suspended": false,
		},
		"goal":              nil,
		"pending_ask_users": []any{},
		"pending_approvals": []any{},
	})
}

func (a *App) sessionJSON(session *demoSession, includeMessages bool) map[string]any {
	name := session.Name
	if name == "" {
		name = "New chat"
	}
	title := session.Title
	if title == "" {
		title = name
	}
	out := map[string]any{
		"id":                      session.ID,
		"number":                  session.Number,
		"name":                    name,
		"title":                   title,
		"short_title":             title,
		"mode":                    "chat",
		"origin":                  "web",
		"agent":                   browserDeveloperAgentName,
		"archived":                session.Archived,
		"pinned":                  session.Pinned,
		"created_at":              session.Created.UTC().Format(time.RFC3339Nano),
		"updated_at":              session.Updated.UTC().Format(time.RFC3339Nano),
		"last_message_at":         session.Updated.UTC().Format(time.RFC3339Nano),
		"message_count":           countMessages(session),
		"transcript_rev":          session.TranscriptRev,
		"last_response_id":        session.LastResponse,
		"active_response_id":      session.ActiveResponse,
		"provider":                demoProviderName,
		"provider_key":            demoProviderName,
		"model":                   demoModelName,
		"active_provider":         demoProviderName,
		"active_model":            demoModelName,
		"working_dir":             demoWorkspace,
		"approval_default_mode":   "yolo",
		"approval_effective_mode": "yolo",
		"guardian_available":      false,
		"guardian_auto_suspended": false,
		"interaction_required":    false,
		"mcp_servers":             []any{},
		"mcp_enabled":             []any{},
	}
	if includeMessages {
		out["messages"] = a.messagesJSON(session)
	}
	return out
}

func countMessages(session *demoSession) int {
	count := 0
	for _, message := range session.Messages {
		if message.Role == "user" || message.Role == "assistant" {
			count++
		}
	}
	return count
}

func (a *App) messageIDs(session *demoSession) []int64 {
	ids := make([]int64, 0, len(session.Messages))
	for _, message := range session.Messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func (a *App) messageRoles(session *demoSession) string {
	var roles strings.Builder
	for _, message := range session.Messages {
		switch message.Role {
		case "user":
			roles.WriteByte('u')
		default:
			roles.WriteByte('a')
		}
	}
	return roles.String()
}

func (a *App) messagesJSON(session *demoSession) []map[string]any {
	messages := make([]map[string]any, 0, len(session.Messages))
	for _, message := range session.Messages {
		entry := map[string]any{
			"id":         message.ID,
			"role":       message.Role,
			"created_at": message.Created.UTC().Format(time.RFC3339Nano),
			"parts":      message.Parts,
		}
		if message.ResponseID != "" {
			entry["response_id"] = message.ResponseID
		}
		if message.ClientMessageID != "" {
			entry["client_message_id"] = message.ClientMessageID
		}
		messages = append(messages, entry)
	}
	return messages
}

// responsesRequest is the subset of the OpenAI Responses request the chat UI
// sends. Input is kept raw because it is either a string or a part array.
type responsesRequest struct {
	Model              string          `json:"model"`
	Provider           string          `json:"provider"`
	Input              json.RawMessage `json:"input"`
	Stream             bool            `json:"stream"`
	PreviousResponseID string          `json:"previous_response_id"`
	ClientMessageID    string          `json:"client_message_id"`
	Agent              string          `json:"agent"`
}

func (a *App) handleCreateResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	var req responsesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	parts := parseResponseInput(req.Input)
	if len(parts) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "input is required", "type": "invalid_request_error"}})
		return
	}
	sessionID := strings.TrimSpace(r.Header.Get("X-Term-LLM-Session-ID"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(r.Header.Get("X-Term-LLM-Draft-ID"))
	}
	session := a.session(sessionID)
	if session == nil {
		session = a.createSession("New chat")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = demoModelName
	}
	clientMessageID := req.ClientMessageID
	if clientMessageID == "" {
		clientMessageID = inputClientMessageID(req.Input)
	}
	responseID, ctx, cancel, record, err := a.startResponse(session, parts, clientMessageID, model)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Session-ID", session.ID)
	w.Header().Set("X-Session-Number", strconv.Itoa(session.Number))
	w.Header().Set("X-Response-ID", responseID)
	w.WriteHeader(http.StatusOK)
	flush(w)

	stream := &responseStream{app: a, record: record, w: w}
	if err := stream.created(); err != nil {
		return
	}
	runErr := a.runTurn(ctx, session, stream, model)
	switch {
	case ctx.Err() != nil:
		stream.cancel()
	case runErr != nil:
		stream.fail(runErr)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush(w)
	a.mu.Lock()
	if run := a.runs[session.ID]; run != nil && run.ResponseID == responseID {
		delete(a.runs, session.ID)
	}
	session.ActiveResponse = ""
	a.mu.Unlock()
}

func flush(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (a *App) handleResponseByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	parts := strings.Split(rest, "/")
	id, err := url.PathUnescape(parts[0])
	if err != nil || id == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "response not found"}})
		return
	}
	suffix := ""
	if len(parts) > 1 {
		suffix = strings.Join(parts[1:], "/")
	}
	a.mu.Lock()
	record := a.responses[id]
	a.mu.Unlock()
	if record == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "response not found"}})
		return
	}
	switch suffix {
	case "":
		writeJSON(w, http.StatusOK, map[string]any{"id": record.ID, "status": record.Status, "session_id": record.SessionID})
	case "cancel":
		a.cancelResponse(id)
		writeJSON(w, http.StatusOK, map[string]any{"id": record.ID, "status": "cancelling"})
	case "events":
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		record.mu.Lock()
		events := append([]recordedEvent(nil), record.Events...)
		status := record.Status
		record.mu.Unlock()
		for _, event := range events {
			seq, _ := event.Payload["sequence_number"].(int)
			if seq <= after {
				continue
			}
			data, _ := json.Marshal(event.Payload)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Name, data)
		}
		if status == "in_progress" {
			// The POST stream is the live transport; a resume only needs the
			// replay prefix. Tell the client the run is still active.
			_, _ = io.WriteString(w, ": still running\n\n")
		} else {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
		flush(w)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "not found"}})
	}
}

func parseResponseInput(raw json.RawMessage) []demoPart {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
			return []demoPart{{Type: "text", Text: text}}
		}
		return nil
	}
	var items []any
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var parts []demoPart
	for _, item := range items {
		if text, ok := item.(string); ok {
			if strings.TrimSpace(text) != "" {
				parts = append(parts, demoPart{Type: "text", Text: text})
			}
			continue
		}
		record, _ := item.(map[string]any)
		if record == nil {
			continue
		}
		kind := strings.TrimSpace(fmt.Sprint(record["type"]))
		switch kind {
		case "input_text", "output_text", "text":
			parts = appendTextPart(parts, record["text"])
		case "message":
			// The genuine frontend sends input as
			//   [{type:"message", role:"user", content:<string|[parts]>}]
			parts = appendContentParts(parts, record["content"])
			if content, ok := record["content"].(map[string]any); ok {
				parts = appendContentParts(parts, content["text"])
			}
		case "function_call_output":
			parts = appendTextPart(parts, record["output"])
		default:
			// Tolerate providers that send bare content-bearing parts without a
			// recognised discriminator rather than dropping the user's message.
			if text, ok := record["text"]; ok {
				parts = appendTextPart(parts, text)
			}
		}
	}
	return parts
}

// appendContentParts accepts every content shape the chat UI can produce: a
// plain string, an array of typed parts, or nested message content.
func appendContentParts(parts []demoPart, content any) []demoPart {
	switch value := content.(type) {
	case nil:
		return parts
	case string:
		return appendTextPart(parts, value)
	case []any:
		for _, entry := range value {
			if text, ok := entry.(string); ok {
				parts = appendTextPart(parts, text)
				continue
			}
			record, _ := entry.(map[string]any)
			if record == nil {
				continue
			}
			kind := strings.TrimSpace(fmt.Sprint(record["type"]))
			switch kind {
			case "input_text", "output_text", "text":
				parts = appendTextPart(parts, record["text"])
			case "message":
				parts = appendContentParts(parts, record["content"])
			case "function_call_output":
				parts = appendTextPart(parts, record["output"])
			default:
				if text, ok := record["text"]; ok {
					parts = appendTextPart(parts, text)
				}
			}
		}
	case map[string]any:
		if text, ok := value["text"]; ok {
			parts = appendTextPart(parts, text)
		}
	}
	return parts
}

func appendTextPart(parts []demoPart, value any) []demoPart {
	if value == nil {
		return parts
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" {
		return parts
	}
	return append(parts, demoPart{Type: "text", Text: text})
}

// inputClientMessageID reads the stable intent identity the genuine frontend
// places on the user message part when it is not repeated at the top level.
func inputClientMessageID(raw json.RawMessage) string {
	var items []map[string]any
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	for _, item := range items {
		if id := strings.TrimSpace(fmt.Sprint(item["client_message_id"])); id != "" && id != "<nil>" {
			return id
		}
	}
	return ""
}

// responseStream writes sequenced SSE events for one run and records them for
// resumable GET /v1/responses/{id}/events. Sequence numbers start at 1.
type responseStream struct {
	app      *App
	record   *responseRecord
	w        http.ResponseWriter
	mu       sync.Mutex
	sequence int
	closed   bool
}

func (s *responseStream) emit(name string, payload map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("response stream closed")
	}
	s.sequence++
	payload["response_id"] = s.record.ID
	payload["run_epoch"] = s.record.Epoch
	payload["sequence_number"] = s.sequence
	s.record.mu.Lock()
	s.record.Events = append(s.record.Events, recordedEvent{Name: name, Payload: clonePayload(payload)})
	s.record.mu.Unlock()
	if s.w == nil {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	flush(s.w)
	return nil
}

func clonePayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	return out
}

func (s *responseStream) created() error {
	return s.emit("response.created", map[string]any{
		"started_at": time.Now().UnixMilli(),
		"response": map[string]any{
			"id": s.record.ID, "object": "response", "status": "in_progress",
			"model": demoModelName, "provider": demoProviderName,
			"created_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
}

func (s *responseStream) complete(usage map[string]any) error {
	s.record.mu.Lock()
	s.record.Status = "completed"
	s.record.mu.Unlock()
	response := map[string]any{
		"id": s.record.ID, "object": "response", "status": "completed",
		"model": demoModelName, "provider": demoProviderName,
	}
	if usage != nil {
		response["usage"] = usage
	}
	return s.emit("response.completed", map[string]any{
		"ended_at": time.Now().UnixMilli(),
		"response": response,
		"usage":    usage,
	})
}

func (s *responseStream) fail(err error) {
	s.record.mu.Lock()
	s.record.Status = "failed"
	s.record.mu.Unlock()
	_ = s.emit("response.failed", map[string]any{
		"ended_at": time.Now().UnixMilli(),
		"error":    map[string]any{"message": err.Error(), "type": "inference_error"},
		"response": map[string]any{"id": s.record.ID, "status": "failed", "model": demoModelName, "provider": demoProviderName},
	})
}

func (s *responseStream) cancel() {
	s.record.mu.Lock()
	s.record.Status = "cancelled"
	s.record.mu.Unlock()
	_ = s.emit("response.cancelled", map[string]any{
		"ended_at": time.Now().UnixMilli(),
		"response": map[string]any{"id": s.record.ID, "status": "cancelled", "model": demoModelName, "provider": demoProviderName},
	})
}
