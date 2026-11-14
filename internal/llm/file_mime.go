package llm

import "github.com/term-llm/term-llm/internal/llm/filemime"

// MIME canonicalization lives in filemime so provider families can reuse it
// without importing the root llm package.
var (
	openAIResponsesAcceptedFileMIMETypes   = filemime.AcceptedFileMIMETypes()
	openAIResponsesAcceptedFileMIMETypeSet = filemime.AcceptedFileMIMETypeSet()
	textLikeApplicationMIMETypes           = filemime.TextLikeApplicationMIMETypes()
	uploadMediaTypeAliases                 = filemime.UploadMediaTypeAliases()
	extensionCanonicalMediaTypes           = filemime.ExtensionCanonicalMediaTypes()
)
