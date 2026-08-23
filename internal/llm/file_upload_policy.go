package llm

import (
	"encoding/base64"
	"mime"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

const defaultFileUploadMaxBytes int64 = 20 << 20

var openAIResponsesNativeFileMIMETypes = []string{
	"application/pdf",
	"text/*",
	"text/plain",
	"text/markdown",
	"text/csv",
	"text/tab-separated-values",
	"text/html",
	"text/xml",
	"application/xml",
	"application/json",
	"application/msword",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/rtf",
	"text/rtf",
	"application/vnd.oasis.opendocument.text",
	"application/vnd.ms-excel",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.ms-powerpoint",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

var portableTextEmbedMIMETypes = []string{
	"text/*",
	"text/plain",
	"text/markdown",
	"text/csv",
	"text/tab-separated-values",
	"text/html",
	"text/xml",
	"application/xml",
	"application/json",
	"application/x-ndjson",
	"application/yaml",
	"application/x-yaml",
	"text/yaml",
}

// FileUploadPolicy describes provider-level upload capabilities. Native MIME
// types are allowed to travel as provider-native file/document inputs. Text
// embed MIME types are safe to inline as ordinary text on providers without
// native file support.
type FileUploadPolicy struct {
	NativeMimeTypes    []string
	MaxNativeBytes     int64
	TextEmbedMimeTypes []string
	MaxTextEmbedBytes  int64
}

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

// DefaultOpenAIResponsesFileUploadPolicy returns the native file types currently
// documented for OpenAI Responses-style file input. The same policy is used for
// ChatGPT/Grok/Copilot Responses transports unless overridden in config.
func DefaultOpenAIResponsesFileUploadPolicy() FileUploadPolicy {
	return FileUploadPolicy{
		NativeMimeTypes:    cloneStrings(openAIResponsesNativeFileMIMETypes),
		MaxNativeBytes:     defaultFileUploadMaxBytes,
		TextEmbedMimeTypes: cloneStrings(portableTextEmbedMIMETypes),
		MaxTextEmbedBytes:  defaultFileUploadMaxBytes,
	}
}

// DefaultPortableTextFileUploadPolicy allows no native file forwarding but keeps
// portable text-like uploads useful by inlining their contents.
func DefaultPortableTextFileUploadPolicy() FileUploadPolicy {
	return FileUploadPolicy{
		NativeMimeTypes:    nil,
		MaxNativeBytes:     defaultFileUploadMaxBytes,
		TextEmbedMimeTypes: cloneStrings(portableTextEmbedMIMETypes),
		MaxTextEmbedBytes:  defaultFileUploadMaxBytes,
	}
}

// DefaultFileUploadPolicyForProviderType returns the built-in upload defaults
// for a provider implementation. Only providers with an implemented Responses
// file path get native file MIME types by default.
func DefaultFileUploadPolicyForProviderType(providerType config.ProviderType) FileUploadPolicy {
	switch providerType {
	case config.ProviderTypeOpenAI, config.ProviderTypeChatGPT, config.ProviderTypeGrok, config.ProviderTypeCopilot:
		return DefaultOpenAIResponsesFileUploadPolicy()
	default:
		return DefaultPortableTextFileUploadPolicy()
	}
}

// EffectiveFileUploadPolicyForProviderConfig merges provider defaults with any
// config-level overrides. A configured empty native_mime_types list intentionally
// disables native file forwarding for that provider.
func EffectiveFileUploadPolicyForProviderConfig(providerName string, providerCfg config.ProviderConfig) FileUploadPolicy {
	policy := DefaultFileUploadPolicyForProviderType(config.InferProviderType(providerName, providerCfg.Type))
	if providerCfg.FileUpload == nil {
		return policy
	}
	override := providerCfg.FileUpload
	if override.NativeMimeTypes != nil {
		policy.NativeMimeTypes = normalizeMIMETypes(override.NativeMimeTypes)
	}
	if override.MaxNativeBytes > 0 {
		policy.MaxNativeBytes = override.MaxNativeBytes
	}
	if override.TextEmbedMimeTypes != nil {
		policy.TextEmbedMimeTypes = normalizeMIMETypes(override.TextEmbedMimeTypes)
	}
	if override.MaxTextEmbedBytes > 0 {
		policy.MaxTextEmbedBytes = override.MaxTextEmbedBytes
	}
	return policy
}

// FileUploadPolicyOverrideForProviderConfig returns nil when no file_upload block
// was configured, allowing provider constructors to use their built-in defaults.
func FileUploadPolicyOverrideForProviderConfig(providerName string, providerCfg config.ProviderConfig) *FileUploadPolicy {
	if providerCfg.FileUpload == nil {
		return nil
	}
	policy := EffectiveFileUploadPolicyForProviderConfig(providerName, providerCfg)
	return &policy
}

func (p FileUploadPolicy) AllowsNative(mediaType string, sizeBytes int64) bool {
	if len(p.NativeMimeTypes) == 0 {
		return false
	}
	if p.MaxNativeBytes > 0 && sizeBytes > p.MaxNativeBytes {
		return false
	}
	return mimeAllowed(mediaType, p.NativeMimeTypes)
}

func (p FileUploadPolicy) AllowsTextEmbed(mediaType string, sizeBytes int64) bool {
	if len(p.TextEmbedMimeTypes) == 0 {
		return false
	}
	if p.MaxTextEmbedBytes > 0 && sizeBytes > p.MaxTextEmbedBytes {
		return false
	}
	return mimeAllowed(mediaType, p.TextEmbedMimeTypes)
}

func decodedBase64Size(base64Data string) int64 {
	base64Data = strings.NewReplacer("\r", "", "\n", "").Replace(base64Data)
	if base64Data == "" {
		return 0
	}
	decodedLen := base64.StdEncoding.DecodedLen(len(base64Data))
	if strings.HasSuffix(base64Data, "=") {
		decodedLen--
	}
	if strings.HasSuffix(base64Data, "==") {
		decodedLen--
	}
	if decodedLen < 0 {
		return 0
	}
	return int64(decodedLen)
}

func toolFileSizeBytes(file *ToolFileData) int64 {
	if file == nil {
		return 0
	}
	if file.SizeBytes > 0 {
		return file.SizeBytes
	}
	return decodedBase64Size(file.Base64)
}

func normalizeMIMETypes(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		v := strings.TrimSpace(strings.ToLower(value))
		if v == "" {
			continue
		}
		if v != "*" && v != "*/*" && !strings.HasSuffix(v, "/*") {
			v = NormalizeMediaType(v)
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func mimeAllowed(mediaType string, allowed []string) bool {
	mediaType = NormalizeMediaType(mediaType)
	if mediaType == "" {
		return false
	}
	for _, candidate := range allowed {
		candidate = strings.TrimSpace(strings.ToLower(candidate))
		if candidate == "" {
			continue
		}
		if candidate == "*" || candidate == "*/*" {
			return true
		}
		if strings.HasSuffix(candidate, "/*") {
			prefix := strings.TrimSuffix(candidate, "*")
			if strings.HasPrefix(mediaType, prefix) {
				return true
			}
			continue
		}
		if NormalizeMediaType(candidate) == mediaType {
			return true
		}
	}
	return false
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneFileUploadPolicy(policy *FileUploadPolicy) *FileUploadPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	clone.NativeMimeTypes = cloneStrings(policy.NativeMimeTypes)
	clone.TextEmbedMimeTypes = cloneStrings(policy.TextEmbedMimeTypes)
	return &clone
}

func defaultOpenAIResponsesFileUploadPolicyPtr() *FileUploadPolicy {
	policy := DefaultOpenAIResponsesFileUploadPolicy()
	return &policy
}

func (p *OpenAIProvider) effectiveFileUploadPolicy() *FileUploadPolicy {
	if p != nil && p.fileUploadPolicy != nil {
		return p.fileUploadPolicy
	}
	return defaultOpenAIResponsesFileUploadPolicyPtr()
}

func (p *ChatGPTProvider) effectiveFileUploadPolicy() *FileUploadPolicy {
	if p != nil && p.fileUploadPolicy != nil {
		return p.fileUploadPolicy
	}
	return defaultOpenAIResponsesFileUploadPolicyPtr()
}

func (p *CopilotProvider) effectiveFileUploadPolicy() *FileUploadPolicy {
	if p != nil && p.fileUploadPolicy != nil {
		return p.fileUploadPolicy
	}
	return defaultOpenAIResponsesFileUploadPolicyPtr()
}
