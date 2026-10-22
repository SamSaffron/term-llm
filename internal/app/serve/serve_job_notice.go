package serve

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

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
