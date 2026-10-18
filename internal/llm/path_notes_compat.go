package llm

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/engine"
)

// PathNotesConfig controls the bounded helper call that extracts useful context
// from turns omitted by a conversation branch.
type PathNotesConfig = engine.PathNotesConfig

// PathNotesResult is a compact, non-authoritative account of work performed on
// the path that a new conversation branch did not copy.
type PathNotesResult = engine.PathNotesResult

// GeneratePathNotes makes one isolated, ephemeral provider request over a
// bounded serialization of the supplied abandoned-path messages. Model limits
// and provider isolation come from the engine environment installed by this
// package.
func GeneratePathNotes(ctx context.Context, provider Provider, model string, source []Message, config PathNotesConfig) (*PathNotesResult, error) {
	return engine.GeneratePathNotes(ctx, provider, model, source, config)
}
