package serve

import (
	"github.com/term-llm/term-llm/internal/codemode/presentation"
	"github.com/term-llm/term-llm/internal/llm/core"
)

func (s *serveServer) toolContentPartsPayload(parts []core.ToolContentPart) []map[string]any {
	return presentation.ContentPartsPayload(parts, func(mediaType, b64 string) (string, bool) {
		path, ok := s.materializeInlineSessionImage(mediaType, b64)
		if !ok {
			return "", false
		}
		return serveRoutePath(s.cfg.imagesRoute(), s.imageOutputDir(), path), true
	})
}
