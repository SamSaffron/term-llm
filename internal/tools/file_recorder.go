package tools

import (
	"bytes"
	"context"
	"path/filepath"
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
type AttributedPathChecker interface {
	HasAttributedPath(ctx context.Context, sessionID, path string) bool
}

type AttributedFileRecorder interface {
	RecordAttributedChange(ctx context.Context, rec filetrack.ChangeRecord) (*llm.FileChange, error)
}

type FilesystemObservationRecorder interface {
	RecordFilesystemObservation(ctx context.Context, obs filetrack.FilesystemObservation) (*llm.FilesystemObservationSummary, error)
}

type RunFileRecorder interface {
	RecordRunStart(ctx context.Context, run filetrack.RunRecord) error
	RecordRunComplete(ctx context.Context, run filetrack.RunRecord) error
}

// FileChangeRecorder retains the compatibility method for embedders. Built-in
// tools require/use the explicit classified interfaces above and never route a
// shell detection through RecordChange.
type FileChangeRecorder interface {
	RecordChange(ctx context.Context, rec filetrack.ChangeRecord) *llm.FileChange
	// SessionPaths returns absolute paths already recorded for a session.
	SessionPaths(ctx context.Context, sessionID string) []string
	// MaxFileBytes is the per-file content cap; callers can use it to bound
	// snapshot reads before handing content to RecordChange.
	MaxFileBytes() int
}

func directBaselineState(ctx context.Context, path string, before []byte, beforeMissing bool) string {
	if beforeMissing {
		return filetrack.BaselineNormal
	}
	resolver := newShellRepoResolver()
	root := resolver.owningRepo(path)
	if root == "" {
		return filetrack.BaselineUnknown
	}
	content := gitShowIndexBatch(ctx, root, []string{filepath.Clean(path)}, len(before)+1, 1, int64(len(before)+1))
	indexed, ok := content[filepath.Clean(path)]
	if !ok {
		return filetrack.BaselinePreexistingDirty
	}
	if bytes.Equal(indexed, before) {
		return filetrack.BaselineNormal
	}
	return filetrack.BaselinePreexistingDirty
}

// fileRecordTimeout bounds best-effort tracking writes that intentionally live
// past request cancellation after the filesystem mutation has already happened.
const fileRecordTimeout = 5 * time.Second

// recordFileChange is the shared helper edit/write tools call after a
// successful write (while still holding the per-path lock). Returns nil when
// recording is disabled or no session is active.
func recordFileChange(ctx context.Context, recorder FileChangeRecorder, toolName, path string, before, after []byte, beforeMissing, afterMissing bool) (*llm.FileChange, *llm.OutputClaimDiagnostic) {
	if recorder == nil {
		return nil, nil
	}
	sessionID := llm.SessionIDFromContext(ctx)
	if sessionID == "" {
		return nil, nil
	}
	callID := llm.CallIDFromContext(ctx)
	// The filesystem mutation has already happened when callers reach this
	// helper. Keep the best-effort DB write alive even if the surrounding request
	// is cancelled at the same moment, but keep a short timeout so tracking can
	// never hang the tool indefinitely.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fileRecordTimeout)
	defer cancel()
	baselineState := filetrack.BaselineUnknown
	if checker, ok := recorder.(AttributedPathChecker); !ok || !checker.HasAttributedPath(recordCtx, sessionID, path) {
		baselineState = directBaselineState(recordCtx, path, before, beforeMissing)
	}
	rec := filetrack.ChangeRecord{
		SessionID:     sessionID,
		RunID:         llm.ToolRunIDFromContext(ctx),
		ToolName:      toolName,
		ToolCallID:    callID,
		Path:          path,
		Before:        before,
		After:         after,
		BeforeMissing: beforeMissing,
		AfterMissing:  afterMissing,
		Provenance:    filetrack.ProvenanceDirect,
		ClaimCoverage: filetrack.CoverageComplete,
		BaselineState: baselineState,
	}
	if explicit, ok := recorder.(AttributedFileRecorder); ok {
		change, err := explicit.RecordAttributedChange(recordCtx, rec)
		if err != nil {
			return nil, &llm.OutputClaimDiagnostic{Reason: "claim_unconfirmed_tracker_error", CoverageStatus: filetrack.CoverageUnavailable, Message: err.Error()}
		}
		return change, nil
	}
	// Compatibility embedders can still record direct writes; shell tracking
	// never uses this fallback.
	return recorder.RecordChange(recordCtx, rec), nil
}
