package llm

import (
	"net/http"

	"github.com/term-llm/term-llm/internal/llm/debug"
	"github.com/term-llm/term-llm/internal/llm/filemime"
	"github.com/term-llm/term-llm/internal/llm/providers/responses"
	"github.com/term-llm/term-llm/internal/llm/schema"
)

// The Responses API client, wire types, request builders and WebSocket
// transport live in providers/responses; this installs the root-owned hooks.

func init() {
	responses.ParentHooks = responses.Hooks{
		DebugRawSection:                DebugRawSection,
		EmitDebugDiagnostic:            debug.EmitDiagnostic,
		HTTPClient:                     func() *http.Client { return defaultHTTPClient },
		OpenAIParametersFromToolSchema: schema.OpenAIParametersFromToolSchema,
		SupportsReasoningMode:          SupportsReasoningMode,
		BaseModelAndEffortForProvider:  BaseModelAndEffortForProvider,
		IsEmbeddedFileText:             IsEmbeddedFileText,
		FormatUploadedFileNotice:       FormatUploadedFileNotice,
		CanonicalUploadMediaType:       filemime.CanonicalUploadMediaType,
		CanonicalTextUploadMediaType:   filemime.CanonicalTextUploadMediaType,
		IsTextLikeMediaType:            filemime.IsTextLikeMediaType,
		DefaultFileUploadPolicy: func() responses.FileUploadPolicy {
			return responses.FileUploadPolicy(DefaultOpenAIResponsesFileUploadPolicy())
		},
	}
}
