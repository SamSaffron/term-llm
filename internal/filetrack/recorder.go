package filetrack

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
)

// Recorder adapts a Store to the tools-facing FileChangeRecorder interface.
// Recording is best-effort: failures are reported once on stderr and never
// surface to the calling tool — tracking must not break editing.
type Recorder struct {
	store    *Store
	warnOnce sync.Once
}

// NewRecorder wraps a store in a Recorder.
func NewRecorder(store *Store) *Recorder {
	return &Recorder{store: store}
}

// RecordAttributedChange persists one explicitly classified attributed transition.
func (r *Recorder) RecordAttributedChange(ctx context.Context, rec ChangeRecord) (*llm.FileChange, error) {
	change, err := r.store.RecordAttributedChange(ctx, rec)
	if err != nil {
		return nil, err
	}
	return llmChange(change), nil
}

// RecordFilesystemObservation persists metadata in the independent sidecar.
func (r *Recorder) RecordFilesystemObservation(ctx context.Context, obs FilesystemObservation) (*llm.FilesystemObservationSummary, error) {
	persisted, err := r.store.RecordFilesystemObservation(ctx, obs)
	if err != nil || persisted == nil {
		return nil, err
	}
	return &llm.FilesystemObservationSummary{ID: persisted.ID, Classification: persisted.Classification,
		Root: persisted.Root, CreatedCount: persisted.CreatedCount, ModifiedCount: persisted.ModifiedCount,
		DeletedCount: persisted.DeletedCount, SampledPaths: append([]string(nil), persisted.SampledPaths...),
		SamplesTruncated: persisted.SamplesTruncated, CoverageStatus: persisted.CoverageStatus, EventSeq: persisted.EventSeq}, nil
}

func (r *Recorder) RecordRunStart(ctx context.Context, run RunRecord) error {
	return r.store.RecordRunStart(ctx, run)
}
func (r *Recorder) RecordRunComplete(ctx context.Context, run RunRecord) error {
	return r.store.RecordRunComplete(ctx, run)
}
func (r *Recorder) RecordFileTrackingRunStart(ctx context.Context, sessionID, runID string) error {
	return r.store.RecordRunStart(ctx, RunRecord{SessionID: sessionID, RunID: runID})
}
func (r *Recorder) RecordFileTrackingRunComplete(ctx context.Context, sessionID, runID string) error {
	return r.store.RecordRunComplete(ctx, RunRecord{SessionID: sessionID, RunID: runID})
}

// RecordChange is the compatibility adapter for older direct integrations.
func (r *Recorder) RecordChange(ctx context.Context, rec ChangeRecord) *llm.FileChange {
	change, err := r.store.RecordChange(ctx, rec)
	if err != nil {
		r.warnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: file change tracking failed: %v\n", err)
		})
		return nil
	}
	return llmChange(change)
}

func llmChange(change *Change) *llm.FileChange {
	if change == nil {
		return nil
	}
	return &llm.FileChange{
		Path: change.Path, Kind: change.Kind, Adds: change.Adds, Dels: change.Dels,
		Seq: change.Seq, EventSeq: change.EventSeq, Truncated: change.Truncated,
		Provenance: change.Provenance, Provenances: append([]string(nil), change.Provenances...),
		BaselineState: change.BaselineState, ContentStatus: change.ContentStatus,
		ContentAvailable: change.ContentAvailable, ClaimCoverage: change.ClaimCoverage,
		TrustedPersisted: true,
	}
}

func (r *Recorder) HasAttributedPath(ctx context.Context, sessionID, path string) bool {
	exists, err := r.store.HasAttributedPath(ctx, sessionID, path)
	return err == nil && exists
}

// SessionPaths returns paths already recorded for the session (best-effort).
func (r *Recorder) SessionPaths(ctx context.Context, sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	paths, err := r.store.SessionPaths(ctx, sessionID)
	if err != nil {
		return nil
	}
	return paths
}

// MaxFileBytes returns the per-file content cap.
func (r *Recorder) MaxFileBytes() int {
	return r.store.MaxFileBytes()
}
