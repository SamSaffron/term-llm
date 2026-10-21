package serve

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type sessionMessagePartEntry struct {
	CreatedAt           int64                           `json:"created_at,omitempty"`
	Type                string                          `json:"type"`
	Text                string                          `json:"text,omitempty"`
	SkillActivation     *core.SkillActivationProvenance `json:"skill_activation,omitempty"`
	PathNote            *core.PathNoteProvenance        `json:"path_note,omitempty"`
	JobNotification     *core.JobNotificationProvenance `json:"job_notification,omitempty"`
	DiffComment         *core.DiffComment               `json:"diff_comment,omitempty"`
	ModelSwap           *core.ModelSwapMarker           `json:"model_swap,omitempty"`
	ToolName            string                          `json:"tool_name,omitempty"`
	ToolInfo            string                          `json:"tool_info,omitempty"`
	ToolArgs            string                          `json:"tool_arguments,omitempty"`
	ToolCallID          string                          `json:"tool_call_id,omitempty"`
	ToolStatus          string                          `json:"tool_status,omitempty"`
	AskUserSummary      string                          `json:"ask_user_summary,omitempty"`
	ImageURL            string                          `json:"image_url,omitempty"`
	Images              []string                        `json:"images,omitempty"`
	Media               []webMediaEntry                 `json:"media,omitempty"`
	ToolError           bool                            `json:"tool_error,omitempty"`
	GuardianReviews     []core.GuardianReview           `json:"guardian_reviews,omitempty"`
	SpawnAgent          *tools.SpawnAgentResult         `json:"spawn_agent,omitempty"`
	SpawnAgentToolCalls *int                            `json:"spawn_agent_tool_calls,omitempty"`
	AgentSessionIDs     []string                        `json:"agent_session_ids,omitempty"`
	MimeType            string                          `json:"mime_type,omitempty"`
	FileURL             string                          `json:"file_url,omitempty"`
	SizeBytes           int64                           `json:"size_bytes,omitempty"`
	Kind                string                          `json:"kind,omitempty"`
	Width               int                             `json:"width,omitempty"`
	Height              int                             `json:"height,omitempty"`
}

// jobNoticeProvenance returns the job-notification metadata of a user message
// carrying a queued-job completion notice.
func jobNoticeProvenance(msg *session.Message) (*core.JobNotificationProvenance, bool) {
	if msg.Role != core.RoleUser {
		return nil, false
	}
	return core.JobNotificationOf(msg.Parts)
}

// appendJobNoticePart adds the single part the web UI renders as a job card.
func appendJobNoticePart(entry *sessionMessageEntry, msg *session.Message, info *core.JobNotificationProvenance) {
	notice := *info
	text := core.MessageText(core.Message{Role: msg.Role, Parts: msg.Parts})
	entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "job_notification", Text: text, JobNotification: &notice})
}
