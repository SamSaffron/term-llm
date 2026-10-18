package llm

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/debug"
)

// The debug provider and JSONL debug logger live in llm/debug so they can be
// tested without root llm. These aliases keep existing callers working.

// Deprecated: use debug.Logger for new package boundaries.
type DebugLogger = debug.Logger

// Deprecated: use debug.DebugProvider for new package boundaries.
type DebugProvider = debug.DebugProvider

// NewDebugLogger creates a JSONL debug logger for a session.
//
// Deprecated: use debug.NewLogger.
func NewDebugLogger(baseDir, sessionID string) (*DebugLogger, error) {
	return debug.NewLogger(baseDir, sessionID)
}

// CleanupOldLogs removes debug logs older than maxAge.
//
// Deprecated: use debug.CleanupOldLogs.
func CleanupOldLogs(baseDir string, maxAge time.Duration) error {
	return debug.CleanupOldLogs(baseDir, maxAge)
}

// NewDebugProvider creates a debug provider with the specified variant.
//
// Deprecated: use debug.NewDebugProvider.
func NewDebugProvider(variant string) *DebugProvider { return debug.NewDebugProvider(variant) }

// GetDebugPresets returns a copy of the available debug provider presets.
//
// Deprecated: use debug.GetPresets.
func GetDebugPresets() map[string]debug.Preset { return debug.GetPresets() }

func withDebugDiagnosticSink(ctx context.Context, logger *DebugLogger) context.Context {
	return debug.WithDiagnosticSink(ctx, logger)
}

func emitDebugDiagnostic(ctx context.Context, name string, data map[string]any) {
	debug.EmitDiagnostic(ctx, name, data)
}

func hasReasoningMetadata(part Part) bool {
	return part.ReasoningContent != "" || part.ReasoningKind != "" || part.ReasoningSummaryTitle != "" || part.ReasoningItemID != "" || part.ReasoningEncryptedContent != ""
}
