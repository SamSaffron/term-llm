package tools

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/llm"
)

// FileChangeRecorder records file changes made by tools so sessions can expose
// a cumulative diff. An interface keeps the tools package decoupled from
// filetrack storage internals (mirrors ImageRecorder).
//
// Implementations must be best-effort: recording failures never surface to the
// calling tool.
type FileChangeRecorder interface {
	// RecordChange persists one before→after transition. Returns nil when the
	// change is a no-op or was not recorded.
	RecordChange(ctx context.Context, rec filetrack.ChangeRecord) *llm.FileChange
	// SessionPaths returns absolute paths already recorded for a session.
	SessionPaths(ctx context.Context, sessionID string) []string
	// MaxFileBytes is the per-file content cap; callers can use it to bound
	// snapshot reads before handing content to RecordChange.
	MaxFileBytes() int
}

// fileRecordTimeout bounds best-effort tracking writes that intentionally live
// past request cancellation after the filesystem mutation has already happened.
const fileRecordTimeout = 5 * time.Second

// recordFileChange is the shared helper edit/write tools call after a
// successful write (while still holding the per-path lock). Returns nil when
// recording is disabled or no session is active.
func recordFileChange(ctx context.Context, recorder FileChangeRecorder, toolName, path string, before, after []byte, beforeMissing, afterMissing bool) *llm.FileChange {
	if recorder == nil {
		return nil
	}
	sessionID := llm.SessionIDFromContext(ctx)
	if sessionID == "" {
		return nil
	}
	callID := llm.CallIDFromContext(ctx)
	// The filesystem mutation has already happened when callers reach this
	// helper. Keep the best-effort DB write alive even if the surrounding request
	// is cancelled at the same moment, but keep a short timeout so tracking can
	// never hang the tool indefinitely.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fileRecordTimeout)
	defer cancel()
	return recorder.RecordChange(recordCtx, filetrack.ChangeRecord{
		SessionID:     sessionID,
		RunID:         llm.ToolRunIDFromContext(ctx),
		ToolName:      toolName,
		ToolCallID:    callID,
		Path:          path,
		Before:        before,
		After:         after,
		BeforeMissing: beforeMissing,
		AfterMissing:  afterMissing,
	})
}
