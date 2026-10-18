package llm

import "github.com/samsaffron/term-llm/internal/llm/filemime"

// MIME canonicalization lives in filemime so provider families can reuse it
// without importing the root compatibility package.
var (
	openAIResponsesAcceptedFileMIMETypes   = filemime.AcceptedFileMIMETypes()
	openAIResponsesAcceptedFileMIMETypeSet = filemime.AcceptedFileMIMETypeSet()
	textLikeApplicationMIMETypes           = filemime.TextLikeApplicationMIMETypes()
	uploadMediaTypeAliases                 = filemime.UploadMediaTypeAliases()
	extensionCanonicalMediaTypes           = filemime.ExtensionCanonicalMediaTypes()
)

func mimeSet(values []string) map[string]bool { return filemime.MimeSet(values) }

// CanonicalUploadMediaType repairs unrecognized MIME labels for native inputs.
// Deprecated: use filemime.CanonicalUploadMediaType.
func CanonicalUploadMediaType(filename, mediaType string) string {
	return filemime.CanonicalUploadMediaType(filename, mediaType)
}

// CanonicalTextUploadMediaType selects a text-safe MIME for embedded uploads.
// Deprecated: use filemime.CanonicalTextUploadMediaType.
func CanonicalTextUploadMediaType(filename, mediaType string) string {
	return filemime.CanonicalTextUploadMediaType(filename, mediaType)
}

// IsTextLikeMediaType reports whether the MIME token is safe to embed as text.
// Deprecated: use filemime.IsTextLikeMediaType.
func IsTextLikeMediaType(mediaType string) bool { return filemime.IsTextLikeMediaType(mediaType) }
