package filemime

import (
	"mime"
	"strings"
)

// AcceptedFileMIMETypes is the set of exact MIME tokens supported by Responses.
func AcceptedFileMIMETypes() []string { return openAIResponsesAcceptedFileMIMETypes }

// AcceptedFileMIMETypeSet is the lookup form of AcceptedFileMIMETypes.
func AcceptedFileMIMETypeSet() map[string]bool { return openAIResponsesAcceptedFileMIMETypeSet }

// TextLikeApplicationMIMETypes lists application/* types that can be embedded as text.
func TextLikeApplicationMIMETypes() map[string]bool { return textLikeApplicationMIMETypes }

// MimeSet indexes exact MIME tokens for membership tests.
func MimeSet(values []string) map[string]bool { return mimeSet(values) }

// UploadMediaTypeAliases is the mapping of common, noncanonical MIME labels.
func UploadMediaTypeAliases() map[string]string { return uploadMediaTypeAliases }

// ExtensionCanonicalMediaTypes maps known file extensions to accepted MIME labels.
func ExtensionCanonicalMediaTypes() map[string]string { return extensionCanonicalMediaTypes }

// NormalizeMediaType lowercases a MIME type and strips optional parameters.
func NormalizeMediaType(mediaType string) string {
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	if mediaType == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = parsed
	} else if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	if mediaType == "image/jpg" {
		return "image/jpeg"
	}
	return mediaType
}
