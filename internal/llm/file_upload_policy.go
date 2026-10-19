package llm

import (
	"encoding/base64"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/filemime"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
)

const defaultFileUploadMaxBytes int64 = 20 << 20

// portableTextEmbedMIMETypes lists text-like types that are safe to inline as
// ordinary text on providers without native file support, with the text/*
// wildcard first. The wildcard is intentional: inlining text can never be
// rejected by a provider MIME allowlist. The remaining entries are derived from
// textLikeApplicationMIMETypes in file_mime.go so the portable fallback can never
// be narrower than the canonicalizer's idea of text.
var portableTextEmbedMIMETypes = portableTextEmbedMIMETypeList()

func portableTextEmbedMIMETypeList() []string {
	types := make([]string, 1, len(textLikeApplicationMIMETypes)+1)
	types[0] = "text/*"
	for mediaType := range textLikeApplicationMIMETypes {
		types = append(types, mediaType)
	}
	sort.Strings(types[1:])
	return types
}

// DefaultOpenAIResponsesFileUploadPolicy returns the native file types accepted
// by the OpenAI Responses file input API. The same policy is used for
// ChatGPT/Grok/Copilot Responses transports unless overridden in config.
func DefaultOpenAIResponsesFileUploadPolicy() responses.FileUploadPolicy {
	return responses.FileUploadPolicy{
		NativeMimeTypes:    cloneStrings(openAIResponsesAcceptedFileMIMETypes),
		MaxNativeBytes:     defaultFileUploadMaxBytes,
		TextEmbedMimeTypes: cloneStrings(portableTextEmbedMIMETypes),
		MaxTextEmbedBytes:  defaultFileUploadMaxBytes,
	}
}

// DefaultPortableTextFileUploadPolicy allows no native file forwarding but keeps
// portable text-like uploads useful by inlining their contents.
func DefaultPortableTextFileUploadPolicy() responses.FileUploadPolicy {
	return responses.FileUploadPolicy{
		NativeMimeTypes:    nil,
		MaxNativeBytes:     defaultFileUploadMaxBytes,
		TextEmbedMimeTypes: cloneStrings(portableTextEmbedMIMETypes),
		MaxTextEmbedBytes:  defaultFileUploadMaxBytes,
	}
}

// DefaultFileUploadPolicyForProviderType returns the built-in upload defaults
// for a provider implementation. Only providers with an implemented Responses
// file path get native file MIME types by default.
func DefaultFileUploadPolicyForProviderType(providerType config.ProviderType) responses.FileUploadPolicy {
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
func EffectiveFileUploadPolicyForProviderConfig(providerName string, providerCfg config.ProviderConfig) responses.FileUploadPolicy {
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
func FileUploadPolicyOverrideForProviderConfig(providerName string, providerCfg config.ProviderConfig) *responses.FileUploadPolicy {
	if providerCfg.FileUpload == nil {
		return nil
	}
	policy := EffectiveFileUploadPolicyForProviderConfig(providerName, providerCfg)
	return &policy
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

func toolFileSizeBytes(file *core.ToolFileData) int64 {
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
			v = filemime.NormalizeMediaType(v)
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
	mediaType = filemime.NormalizeMediaType(mediaType)
	if mediaType == "" {
		return false
	}
	for _, candidate := range allowed {
		candidate = strings.TrimSpace(strings.ToLower(candidate))
		if candidate == "" {
			continue
		}
		// Exact matches are the common case (the default policy holds over a
		// hundred literal tokens), so avoid NormalizeMediaType's ParseMediaType
		// allocation for them.
		if candidate == mediaType {
			return true
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
		if filemime.NormalizeMediaType(candidate) == mediaType {
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
