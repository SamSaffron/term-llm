package ask

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// UploadHooks are the upload and image helpers shared with the serve command.
// They stay in package cmd (serve owns them), so cmd installs them into Uploads
// before preparing ask input.
type UploadHooks struct {
	// SaveBytes stores raw bytes in the app-owned uploads directory.
	SaveBytes func(filename string, raw []byte, maxBytes int64) (string, error)
	// ResizeImage downsizes an image for LLM use, returning bytes and media type.
	ResizeImage func(data []byte, mediaType string) ([]byte, string)
	// ImageDimensions returns EXIF-aware display dimensions (0, 0 on failure).
	ImageDimensions func(raw []byte) (width, height int)
	// NormalizeMediaType returns the canonical media type for an upload.
	NormalizeMediaType func(filename, mediaType string, raw []byte) string
	// Dir returns the uploads directory, or "" when unavailable.
	Dir func() string
	// MaxImagePixels is the decode pixel limit.
	MaxImagePixels int64
}

// Uploads holds the installed helpers. It must be set by the command layer.
var Uploads UploadHooks

// SubagentProgressCallback optionally wraps the renderer's subagent event sink
// (for example with the command layer's coalescing dispatcher).
var SubagentProgressCallback func(send func(callID string, event tools.SubagentEvent)) func(callID string, event tools.SubagentEvent)

// UsageRecorder persists per-session model usage owned by the command layer.
type UsageRecorder struct {
	// RecordTurn records a main-model turn with its tool-call count.
	RecordTurn func(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u llm.Usage, toolCalls int)
	// RecordHelper records helper work (guardian, compaction) against a session.
	RecordHelper func(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u llm.Usage)
}

// Usage holds the installed recorder. It must be set by the command layer.
var Usage UsageRecorder
