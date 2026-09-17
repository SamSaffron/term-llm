package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// SessionDirectoryToolName exposes the durable session directory to a
// delegated turn. It is deliberately not named live_*: listing and searching
// sessions is a general capability, not a live-call control.
const SessionDirectoryToolName = "session_directory"

const (
	sessionDirectoryQueryLimit      = 20
	sessionDirectoryDefaultLimit    = 10
	sessionDirectoryMaxEntries      = 10
	sessionDirectoryMaxSnippetRunes = 200
	sessionDirectoryMaxTitleRunes   = 120
	sessionDirectoryUnknownActivity = "unknown"
)

// SessionDirectoryQuery is the transport-free request for one directory lookup.
type SessionDirectoryQuery struct {
	Query           string
	RunningOnly     bool
	ProjectID       string
	IncludeArchived bool
	Limit           int
}

// SessionDirectoryEntry is one raw directory row. Times stay absolute here; the
// tool renders them relative for speech.
type SessionDirectoryEntry struct {
	ID           string
	Number       int64
	Title        string
	Project      string
	Status       string
	Snippet      string
	Running      bool
	NeedsInput   bool
	LastActivity time.Time
	MessageCount int
}

type sessionDirectoryKey struct{}
type sessionDirectoryBinding struct {
	sessionID string
	list      func(context.Context, SessionDirectoryQuery) ([]SessionDirectoryEntry, error)
}

// ContextWithSessionDirectory pins directory authority to the originating chat
// turn. A registered schema alone grants no access, including to subagent
// sessions.
func ContextWithSessionDirectory(ctx context.Context, sessionID string, list func(context.Context, SessionDirectoryQuery) ([]SessionDirectoryEntry, error)) context.Context {
	return context.WithValue(ctx, sessionDirectoryKey{}, sessionDirectoryBinding{sessionID, list})
}

// SessionDirectoryTool lists or searches recent chat sessions.
type SessionDirectoryTool struct{}

func (*SessionDirectoryTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: SessionDirectoryToolName,
		Description: "List or search this term-llm instance's chat sessions. With no query, returns the most recently active " +
			"sessions; with query, searches full transcripts and returns matching excerpts. Sessions marked running are busy, " +
			"and needs_input ones are waiting for user approval or an answer. Use the returned session_id or number with " +
			"live_switch_session, and set include_archived to find a session that is no longer active: archived sessions are " +
			"hidden otherwise but can still be switched to. At most 10 sessions are returned, so narrow the query when there are " +
			"more matches.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":            map[string]any{"type": "string", "description": "Optional words to search for in session transcripts. Omit to list recent sessions."},
				"running_only":     map[string]any{"type": "boolean", "description": "Only report sessions with a task currently running."},
				"project":          map[string]any{"type": "string", "description": "Optional stable project ID (for example prj_01f...) to restrict results to one project."},
				"include_archived": map[string]any{"type": "boolean", "description": "Include archived sessions, which are hidden by default."},
				"limit":            map[string]any{"type": "integer", "description": "How many sessions to examine before the 10-entry output cap (1-20, default 10)."},
			},
			"additionalProperties": false,
		},
	}
}

func (*SessionDirectoryTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	binding, ok := ctx.Value(sessionDirectoryKey{}).(sessionDirectoryBinding)
	if !ok || binding.list == nil || binding.sessionID == "" || binding.sessionID != llm.SessionIDFromContext(ctx) {
		return llm.ToolOutput{}, NewToolError(ErrPermissionDenied, "the session directory is available only to the originating live chat turn")
	}
	params, err := parseSessionDirectoryArgs(args)
	if err != nil {
		return llm.ToolOutput{}, err
	}
	entries, err := binding.list(ctx, params)
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("session directory: %w", err)
	}
	encoded, err := json.Marshal(sessionDirectoryOutput(entries, time.Now()))
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("encode session directory: %w", err)
	}
	return llm.TextOutput(string(encoded)), nil
}

func (*SessionDirectoryTool) Preview(json.RawMessage) string {
	return "List or search chat sessions"
}

func parseSessionDirectoryArgs(args json.RawMessage) (SessionDirectoryQuery, error) {
	params := struct {
		Query           string `json:"query"`
		RunningOnly     bool   `json:"running_only"`
		Project         string `json:"project"`
		IncludeArchived bool   `json:"include_archived"`
		Limit           int    `json:"limit"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return SessionDirectoryQuery{}, NewToolErrorf(ErrInvalidParams, "invalid session directory arguments: %v", err)
	}
	limit := params.Limit
	if limit <= 0 {
		limit = sessionDirectoryDefaultLimit
	}
	if limit > sessionDirectoryQueryLimit {
		limit = sessionDirectoryQueryLimit
	}
	return SessionDirectoryQuery{
		Query: strings.TrimSpace(params.Query), RunningOnly: params.RunningOnly,
		ProjectID: strings.TrimSpace(params.Project), IncludeArchived: params.IncludeArchived, Limit: limit,
	}, nil
}

type sessionDirectoryOutputEntry struct {
	SessionID  string `json:"session_id"`
	Number     int64  `json:"number,omitempty"`
	Title      string `json:"title,omitempty"`
	Project    string `json:"project,omitempty"`
	Status     string `json:"status,omitempty"`
	Activity   string `json:"activity,omitempty"`
	Messages   int    `json:"messages,omitempty"`
	Running    bool   `json:"running,omitempty"`
	NeedsInput bool   `json:"needs_input,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
}

// sessionDirectoryOutputBody is the JSON the agent reads. Count is the number of
// rows the lookup matched, which the requested limit bounds and which can exceed
// the listed sessions: Sessions never holds more than sessionDirectoryMaxEntries
// entries, and Truncated marks the difference.
type sessionDirectoryOutputBody struct {
	Count     int                           `json:"count"`
	Truncated bool                          `json:"truncated,omitempty"`
	Sessions  []sessionDirectoryOutputEntry `json:"sessions"`
	Note      string                        `json:"note,omitempty"`
}

// sessionDirectoryOutput shapes raw rows for a listening agent: bounded
// entries, bounded snippets, and relative activity instead of timestamps.
func sessionDirectoryOutput(entries []SessionDirectoryEntry, now time.Time) sessionDirectoryOutputBody {
	body := sessionDirectoryOutputBody{Count: len(entries), Sessions: make([]sessionDirectoryOutputEntry, 0, min(len(entries), sessionDirectoryMaxEntries))}
	if len(entries) > sessionDirectoryMaxEntries {
		body.Truncated = true
	}
	for _, entry := range entries[:min(len(entries), sessionDirectoryMaxEntries)] {
		body.Sessions = append(body.Sessions, sessionDirectoryOutputEntry{
			SessionID: entry.ID, Number: entry.Number,
			Title:   truncateRunes(strings.TrimSpace(entry.Title), sessionDirectoryMaxTitleRunes),
			Project: truncateRunes(strings.TrimSpace(entry.Project), sessionDirectoryMaxTitleRunes),
			Status:  strings.TrimSpace(entry.Status), Activity: sessionDirectoryActivity(entry.LastActivity, now),
			Messages: entry.MessageCount, Running: entry.Running, NeedsInput: entry.NeedsInput,
			Snippet: truncateRunes(strings.TrimSpace(entry.Snippet), sessionDirectoryMaxSnippetRunes),
		})
	}
	if len(body.Sessions) == 0 {
		body.Note = "No sessions matched. Try a broader query or drop running_only."
	}
	if body.Truncated {
		body.Note = "More sessions matched than shown; narrow the query for a specific one."
	}
	return body
}

func sessionDirectoryActivity(at, now time.Time) string {
	if at.IsZero() {
		return sessionDirectoryUnknownActivity
	}
	elapsed := now.Sub(at)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	case elapsed < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	case elapsed < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(elapsed.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(elapsed.Hours()/(24*365)))
	}
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 || text == "" {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
