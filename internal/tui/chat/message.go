package chat

import (
	"time"
)

// MessageRole represents who sent the message
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
)

// ChatMessage represents a single message in the conversation
type ChatMessage struct {
	Role       MessageRole `json:"role"`
	Content    string      `json:"content"`
	Files      []string    `json:"files,omitempty"`       // Attached files for user messages
	Tokens     int         `json:"tokens,omitempty"`      // Token count for assistant messages
	DurationMs int64       `json:"duration_ms,omitempty"` // Response time for assistant messages
	WebSearch  bool        `json:"web_search,omitempty"`  // Whether web search was used
	CreatedAt  time.Time   `json:"created_at"`
}

// NewUserMessage creates a new user message
func NewUserMessage(content string, files []string) ChatMessage {
	return ChatMessage{
		Role:      RoleUser,
		Content:   content,
		Files:     files,
		CreatedAt: time.Now(),
	}
}

// NewAssistantMessage creates a new assistant message
func NewAssistantMessage(content string, tokens int, durationMs int64, webSearch bool) ChatMessage {
	return ChatMessage{
		Role:       RoleAssistant,
		Content:    content,
		Tokens:     tokens,
		DurationMs: durationMs,
		WebSearch:  webSearch,
		CreatedAt:  time.Now(),
	}
}
