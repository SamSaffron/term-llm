package session

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// Session represents a chat session stored in the database.
type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Summary   string    `json:"summary,omitempty"` // First user message or auto-generated
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	CWD       string    `json:"cwd,omitempty"` // Working directory at session start
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Archived  bool      `json:"archived,omitempty"`
	ParentID  string    `json:"parent_id,omitempty"` // For session branching

	// Session settings (restored on resume unless overridden)
	Search bool   `json:"search,omitempty"` // Web search enabled
	Tools  string `json:"tools,omitempty"`  // Enabled tools (comma-separated)
	MCP    string `json:"mcp,omitempty"`    // Enabled MCP servers (comma-separated)
}

// Message represents a message in a session.
// The Parts field stores the full llm.Message.Parts as JSON to preserve
// tool calls and results exactly.
type Message struct {
	ID          int64      `json:"id"`
	SessionID   string     `json:"session_id"`
	Role        llm.Role   `json:"role"`
	Parts       []llm.Part `json:"parts"`        // Full parts array
	TextContent string     `json:"text_content"` // Extracted text for display/FTS
	DurationMs  int64      `json:"duration_ms,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Sequence    int        `json:"sequence"`
}

// SessionSummary is a lightweight view of a session for listing.
type SessionSummary struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ListOptions configures session listing.
type ListOptions struct {
	Provider string // Filter by provider
	Model    string // Filter by model
	Limit    int    // Max results (0 = use default)
	Offset   int    // Pagination offset
	Archived bool   // Include archived sessions
}

// SearchResult represents a search match.
type SearchResult struct {
	SessionID   string    `json:"session_id"`
	MessageID   int64     `json:"message_id"`
	SessionName string    `json:"session_name"`
	Summary     string    `json:"summary"`
	Snippet     string    `json:"snippet"` // Matched text snippet
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	CreatedAt   time.Time `json:"created_at"`
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
		if p.Type == llm.PartText && p.Text != "" {
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
	return llm.Message{
		Role:  m.Role,
		Parts: m.Parts,
	}
}

// PartsJSON returns the Parts field serialized to JSON for database storage.
func (m *Message) PartsJSON() (string, error) {
	data, err := json.Marshal(m.Parts)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SetPartsFromJSON deserializes JSON into the Parts field.
func (m *Message) SetPartsFromJSON(data string) error {
	if data == "" {
		m.Parts = nil
		return nil
	}
	return json.Unmarshal([]byte(data), &m.Parts)
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
