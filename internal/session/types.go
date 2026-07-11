package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// SessionStatus represents the current state of a session.
type SessionStatus string

const (
	StatusActive      SessionStatus = "active"      // Session is open/current (may or may not be streaming)
	StatusComplete    SessionStatus = "complete"    // Session finished normally
	StatusError       SessionStatus = "error"       // Session ended with an error
	StatusInterrupted SessionStatus = "interrupted" // Session was cancelled by user
)

// SessionMode represents the type/context of a session.
type SessionMode string

const (
	ModeChat SessionMode = "chat" // Interactive chat TUI
	ModeAsk  SessionMode = "ask"  // One-shot ask command
	ModePlan SessionMode = "plan" // Collaborative planning TUI
	ModeExec SessionMode = "exec" // Command suggestion/execution
)

type SessionApprovalMode string

const (
	ApprovalModePrompt SessionApprovalMode = "prompt"
	ApprovalModeAuto   SessionApprovalMode = "auto"
	ApprovalModeYolo   SessionApprovalMode = "yolo"
)

type SessionOrigin string

const (
	OriginTUI      SessionOrigin = "tui"
	OriginWeb      SessionOrigin = "web"
	OriginTelegram SessionOrigin = "telegram"
)

type SessionTitleSource string

const (
	TitleSourceNone      SessionTitleSource = ""
	TitleSourceUser      SessionTitleSource = "user"
	TitleSourceGenerated SessionTitleSource = "generated"
)

// Session represents a chat session stored in the database.
type Session struct {
	ID      string `json:"id"`
	Number  int64  `json:"number,omitempty"` // Sequential session number (1, 2, 3...)
	Name    string `json:"name,omitempty"`
	Summary string `json:"summary,omitempty"` // First user message or auto-generated

	GeneratedShortTitle string             `json:"generated_short_title,omitempty"`
	GeneratedLongTitle  string             `json:"generated_long_title,omitempty"`
	TitleSource         SessionTitleSource `json:"title_source,omitempty"`
	TitleGeneratedAt    time.Time          `json:"title_generated_at,omitempty"`
	TitleBasisMsgSeq    int                `json:"title_basis_msg_seq,omitempty"`
	TitleSkippedAt      time.Time          `json:"title_skipped_at,omitempty"` // Set when autotitle considers session untitlable; cleared when session is updated

	Provider        string              `json:"provider"`               // Provider display label
	ProviderKey     string              `json:"provider_key,omitempty"` // Canonical provider key (e.g. openai, chatgpt, custom alias)
	Model           string              `json:"model"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"` // Reasoning effort pinned at session creation (web only)
	ReasoningMode   string              `json:"reasoning_mode,omitempty"`   // Explicit Responses reasoning mode override (standard/pro).
	Mode            SessionMode         `json:"mode,omitempty"`             // Session mode (chat, ask, plan, exec)
	ApprovalMode    SessionApprovalMode `json:"approval_mode,omitempty"`    // Tool approval mode for chat sessions (prompt, auto, yolo)
	Origin          SessionOrigin       `json:"origin,omitempty"`           // Session surface/origin (tui, web, telegram)
	Agent           string              `json:"agent,omitempty"`            // Agent name used for this session
	CWD             string              `json:"cwd,omitempty"`              // Working directory at session start
	WorktreeDir     string              `json:"worktree_dir,omitempty"`     // Bound git worktree directory, if any
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Archived        bool                `json:"archived,omitempty"`
	Pinned          bool                `json:"pinned,omitempty"`
	ParentID        string              `json:"parent_id,omitempty"`   // For session branching
	IsSubagent      bool                `json:"is_subagent,omitempty"` // True if this is a subagent session

	// Session settings (restored on resume unless overridden)
	Search bool   `json:"search,omitempty"` // Web search enabled
	Tools  string `json:"tools,omitempty"`  // Enabled tools (comma-separated)
	MCP    string `json:"mcp,omitempty"`    // Enabled MCP servers (comma-separated)

	// Session metrics
	UserTurns         int           `json:"user_turns,omitempty"`          // Number of user messages
	LLMTurns          int           `json:"llm_turns,omitempty"`           // Number of LLM API round-trips
	ToolCalls         int           `json:"tool_calls,omitempty"`          // Total tool executions
	InputTokens       int           `json:"input_tokens,omitempty"`        // Total non-cached, non-cache-write input tokens
	CachedInputTokens int           `json:"cached_input_tokens,omitempty"` // Total cached input tokens read (cache hits)
	CacheWriteTokens  int           `json:"cache_write_tokens,omitempty"`  // Total tokens written to cache (cache misses)
	OutputTokens      int           `json:"output_tokens,omitempty"`       // Total output tokens used
	LastTotalTokens   int           `json:"last_total_tokens,omitempty"`   // Last observed request context size (input+cached+output)
	LastMessageCount  int           `json:"last_message_count,omitempty"`  // Legacy checkpoint count; estimator uses structural delta
	Status            SessionStatus `json:"status,omitempty"`              // Session status
	Tags              string        `json:"tags,omitempty"`                // Comma-separated tags
	Goal              *Goal         `json:"goal,omitempty"`                // Persistent objective state for /goal
	Share             *ShareState   `json:"share,omitempty"`               // Persisted external share metadata
	CompactionSeq     int           `json:"compaction_seq,omitempty"`      // Sequence of first post-compaction message (-1 = none)
	CompactionCount   int           `json:"compaction_count,omitempty"`    // Number of times this session has been compacted
}

// Message represents a message in a session.
// The Parts field stores the full llm.Message.Parts as JSON to preserve tool
// calls, uploaded images/files, and provider replay state exactly.
type Message struct {
	ID             int64      `json:"id"`
	SessionID      string     `json:"session_id"`
	Role           llm.Role   `json:"role"`
	Parts          []llm.Part `json:"parts"`        // Full parts array
	TextContent    string     `json:"text_content"` // Extracted text for display/FTS
	DurationMs     int64      `json:"duration_ms,omitempty"`
	TurnIndex      int        `json:"turn_index,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	Sequence       int        `json:"sequence"`
	CompactionTail bool       `json:"compaction_tail,omitempty"` // Persisted display hint: retained post-compaction context already visible before the marker
}

// SessionSummary is a lightweight view of a session for listing.
type SessionSummary struct {
	ID                  string             `json:"id"`
	Number              int64              `json:"number,omitempty"` // Sequential session number
	Name                string             `json:"name,omitempty"`
	Summary             string             `json:"summary,omitempty"`
	GeneratedShortTitle string             `json:"generated_short_title,omitempty"`
	GeneratedLongTitle  string             `json:"generated_long_title,omitempty"`
	TitleSource         SessionTitleSource `json:"title_source,omitempty"`
	Provider            string             `json:"provider"`
	ProviderKey         string             `json:"provider_key,omitempty"`
	Model               string             `json:"model"`
	Mode                SessionMode        `json:"mode,omitempty"`
	Origin              SessionOrigin      `json:"origin,omitempty"`
	Archived            bool               `json:"archived,omitempty"`
	Pinned              bool               `json:"pinned,omitempty"`
	MessageCount        int                `json:"message_count"`
	UserTurns           int                `json:"user_turns,omitempty"`
	LLMTurns            int                `json:"llm_turns,omitempty"`
	ToolCalls           int                `json:"tool_calls,omitempty"`
	InputTokens         int                `json:"input_tokens,omitempty"`
	CachedInputTokens   int                `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens    int                `json:"cache_write_tokens,omitempty"`
	OutputTokens        int                `json:"output_tokens,omitempty"`
	Status              SessionStatus      `json:"status,omitempty"`
	Tags                string             `json:"tags,omitempty"`
	WorktreeDir         string             `json:"worktree_dir,omitempty"`
	Goal                *Goal              `json:"goal,omitempty"`
	Share               *ShareState        `json:"share,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	LastMessageAt       time.Time          `json:"last_message_at,omitempty"`
}

// ListOptions configures session listing.
type ListOptions struct {
	Name             string        // Filter by name
	Provider         string        // Filter by provider
	Model            string        // Filter by model
	Mode             SessionMode   // Filter by mode (chat, ask, plan, exec)
	Status           SessionStatus // Filter by status
	Tag              string        // Filter by tag (substring match)
	Categories       []string      // Sidebar/web categories (all, chat, web, ask, plan, exec)
	Limit            int           // Max results (0 = use default)
	Offset           int           // Pagination offset
	BeforeNumber     int64         // Keyset cursor: only sessions with number < this value
	SortByNumberDesc bool          // Order by session number descending instead of activity sort
	Archived         bool          // Include archived sessions
	SortByActivity   bool          // Sort by last_message_at (web sidebar); defaults to last_user_message_at
}

// SearchOptions configures session full-text search.
type SearchOptions struct {
	Query      string   // Text query to search for
	Categories []string // Sidebar/web categories (all, chat, web, ask, plan, exec)
	Limit      int      // Max results (0 = use default)
	Archived   bool     // Include archived sessions
}

// SearchResult represents a search match.
type SearchResult struct {
	SessionID           string             `json:"session_id"`
	SessionNumber       int64              `json:"session_number"` // Sequential session number
	MessageID           int64              `json:"message_id"`
	SessionName         string             `json:"session_name"`
	Summary             string             `json:"summary"`
	GeneratedShortTitle string             `json:"generated_short_title,omitempty"`
	GeneratedLongTitle  string             `json:"generated_long_title,omitempty"`
	TitleSource         SessionTitleSource `json:"title_source,omitempty"`
	Snippet             string             `json:"snippet"` // Matched text snippet
	Provider            string             `json:"provider"`
	ProviderKey         string             `json:"provider_key,omitempty"`
	Model               string             `json:"model"`
	Mode                SessionMode        `json:"mode,omitempty"`
	Origin              SessionOrigin      `json:"origin,omitempty"`
	Archived            bool               `json:"archived,omitempty"`
	Pinned              bool               `json:"pinned,omitempty"`
	Status              SessionStatus      `json:"status,omitempty"`
	MessageCount        int                `json:"message_count"`
	SessionCreatedAt    time.Time          `json:"session_created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	LastMessageAt       time.Time          `json:"last_message_at,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
}

// NewMessage creates a new Message from an llm.Message with the given session ID and sequence.
func NewMessage(sessionID string, msg llm.Message, sequence int) *Message {
	m := &Message{
		SessionID: sessionID,
		Role:      msg.Role,
		Parts:     msg.Parts,
		CreatedAt: time.Now(),
		Sequence:  sequence,
	}
	m.TextContent = m.ExtractTextContent()
	return m
}

// ExtractTextContent extracts and concatenates all text parts from the message.
func (m *Message) ExtractTextContent() string {
	var text string
	for _, p := range m.Parts {
		if (p.Type == llm.PartText || p.Type == llm.PartFile) && p.Text != "" {
			if text != "" {
				text += "\n"
			}
			text += p.Text
		}
	}
	return text
}

// ToLLMMessage converts a Message back to an llm.Message.
func (m *Message) ToLLMMessage() llm.Message {
	if m == nil {
		return llm.Message{}
	}
	msg := llm.Message{
		Role:  m.Role,
		Parts: m.Parts,
	}
	if m.isInternalCompactionSummary() {
		msg.CacheAnchor = true
	}
	return msg
}

func (m *Message) isInternalCompactionSummary() bool {
	if m == nil {
		return false
	}
	if llm.IsInternalCompactionSummaryText(m.TextContent) {
		return true
	}
	for _, part := range m.Parts {
		if (part.Type == llm.PartText || part.Type == llm.PartFile) && llm.IsInternalCompactionSummaryText(part.Text) {
			return true
		}
	}
	return false
}

// PartsJSON returns the Parts field serialized to JSON for database storage.
func (m *Message) PartsJSON() (string, error) {
	data, err := json.Marshal(m.Parts)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (m *Message) PartsJSONForStorage(stripImageBase64 bool) (string, error) {
	parts := m.Parts
	if stripImageBase64 {
		parts = partsForStorage(m.Parts)
	}
	data, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func partsForStorage(parts []llm.Part) []llm.Part {
	var out []llm.Part
	for i, part := range parts {
		if part.Type == llm.PartImage && isSessionUploadPath(part.ImagePath) && part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
			if out == nil {
				out = append([]llm.Part(nil), parts[:i]...)
			}
			copyPart := part
			imageData := *part.ImageData
			imageData.Base64 = ""
			copyPart.ImageData = &imageData
			out = append(out, copyPart)
			continue
		}
		if out != nil {
			out = append(out, part)
		}
	}
	if out != nil {
		return out
	}
	return parts
}

func isSessionUploadPath(path string) bool {
	dataDir, err := GetDataDir()
	if err != nil {
		return false
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	uploadsDir, err = filepath.EvalSymlinks(uploadsDir)
	if err != nil {
		return false
	}
	path, err = filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	if path == uploadsDir {
		return false
	}
	return strings.HasPrefix(path, uploadsDir+string(filepath.Separator))
}

// SetPartsFromJSON deserializes JSON into the Parts field.
func (m *Message) SetPartsFromJSON(data string) error {
	if data == "" {
		m.Parts = nil
		return nil
	}
	return json.Unmarshal([]byte(data), &m.Parts)
}

// PreferredShortTitle returns the best short title available for the session.
func (s Session) PreferredShortTitle() string {
	if strings.TrimSpace(s.Name) != "" {
		return strings.TrimSpace(s.Name)
	}
	if strings.TrimSpace(s.GeneratedShortTitle) != "" {
		return strings.TrimSpace(s.GeneratedShortTitle)
	}
	return strings.TrimSpace(s.Summary)
}

// PreferredLongTitle returns the best long descriptive title available for the session.
func (s Session) PreferredLongTitle() string {
	if strings.TrimSpace(s.Name) != "" {
		return strings.TrimSpace(s.Name)
	}
	if strings.TrimSpace(s.GeneratedLongTitle) != "" {
		return strings.TrimSpace(s.GeneratedLongTitle)
	}
	if strings.TrimSpace(s.GeneratedShortTitle) != "" {
		return strings.TrimSpace(s.GeneratedShortTitle)
	}
	return strings.TrimSpace(s.Summary)
}

// PreferredShortTitle returns the best short title available for the summary.
func (s SessionSummary) PreferredShortTitle() string {
	if strings.TrimSpace(s.Name) != "" {
		return strings.TrimSpace(s.Name)
	}
	if strings.TrimSpace(s.GeneratedShortTitle) != "" {
		return strings.TrimSpace(s.GeneratedShortTitle)
	}
	return strings.TrimSpace(s.Summary)
}

// PreferredLongTitle returns the best long descriptive title available for the summary.
func (s SessionSummary) PreferredLongTitle() string {
	if strings.TrimSpace(s.Name) != "" {
		return strings.TrimSpace(s.Name)
	}
	if strings.TrimSpace(s.GeneratedLongTitle) != "" {
		return strings.TrimSpace(s.GeneratedLongTitle)
	}
	if strings.TrimSpace(s.GeneratedShortTitle) != "" {
		return strings.TrimSpace(s.GeneratedShortTitle)
	}
	return strings.TrimSpace(s.Summary)
}

// TruncateSummary returns the first line of content, truncated to 100 chars.
func TruncateSummary(content string) string {
	content = strings.TrimSpace(content)
	if idx := strings.Index(content, "\n"); idx != -1 {
		content = content[:idx]
	}
	if len(content) > 100 {
		content = content[:97] + "..."
	}
	return content
}
