package responses

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"
)

// Hooks lets the parent llm package supply behaviour that lives in root-only
// files (debug output, model registry, file/MIME policy, JSON-schema lowering)
// and cannot be imported without a cycle. Nil fields fall back to neutral
// defaults: no debug output, no reasoning-mode support, identity MIME handling.
type Hooks struct {
	// DebugRawSection is llm.DebugRawSection.
	DebugRawSection func(enabled bool, label, body string)
	// EmitDebugDiagnostic records a structured diagnostic on the context's debug logger.
	EmitDebugDiagnostic func(ctx context.Context, name string, data map[string]any)
	// HTTPClient returns the shared streaming HTTP client (llm's defaultHTTPClient).
	HTTPClient func() *http.Client
	// OpenAIParametersFromToolSchema lowers a tool schema for OpenAI strict mode.
	OpenAIParametersFromToolSchema func(schema map[string]interface{}, strict bool) map[string]interface{}
	// SupportsReasoningMode is llm.SupportsReasoningMode.
	SupportsReasoningMode func(provider, model string) bool
	// BaseModelAndEffortForProvider is llm.BaseModelAndEffortForProvider.
	BaseModelAndEffortForProvider func(provider, model string) (base, effort string)
	// IsEmbeddedFileText reports whether text carries an embedded-file marker.
	IsEmbeddedFileText func(text string) bool
	// FormatUploadedFileNotice is llm.FormatUploadedFileNotice.
	FormatUploadedFileNotice func(filename, mediaType, localPath string, sizeBytes int64) string
	// CanonicalUploadMediaType is llm.CanonicalUploadMediaType.
	CanonicalUploadMediaType func(filename, mediaType string) string
	// CanonicalTextUploadMediaType is llm.CanonicalTextUploadMediaType.
	CanonicalTextUploadMediaType func(filename, mediaType string) string
	// IsTextLikeMediaType is llm.IsTextLikeMediaType.
	IsTextLikeMediaType func(mediaType string) bool
	// DefaultFileUploadPolicy is llm.DefaultOpenAIResponsesFileUploadPolicy.
	DefaultFileUploadPolicy func() FileUploadPolicy
}

// ParentHooks is installed by the parent llm package (typically in init).
var ParentHooks Hooks

// FilePolicy decides whether an upload may travel as a native provider file or
// be inlined as text. Both this package's FileUploadPolicy and the parent's
// llm.FileUploadPolicy implement it, so callers on either side can pass theirs.
type FilePolicy interface {
	AllowsNative(mediaType string, sizeBytes int64) bool
	AllowsTextEmbed(mediaType string, sizeBytes int64) bool
}

// isNilFilePolicy reports whether policy is nil or a typed nil pointer.
func isNilFilePolicy(policy FilePolicy) bool {
	if policy == nil {
		return true
	}
	v := reflect.ValueOf(policy)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// FileUploadPolicy is the default policy shape supplied through Hooks.
type FileUploadPolicy struct {
	NativeMimeTypes    []string
	MaxNativeBytes     int64
	TextEmbedMimeTypes []string
	MaxTextEmbedBytes  int64
}

// AllowsNative reports whether a file may travel as a provider-native input.
func (p FileUploadPolicy) AllowsNative(mediaType string, sizeBytes int64) bool {
	if len(p.NativeMimeTypes) == 0 {
		return false
	}
	if p.MaxNativeBytes > 0 && sizeBytes > p.MaxNativeBytes {
		return false
	}
	return mimeAllowed(mediaType, p.NativeMimeTypes)
}

// AllowsTextEmbed reports whether a file may be inlined as text.
func (p FileUploadPolicy) AllowsTextEmbed(mediaType string, sizeBytes int64) bool {
	if len(p.TextEmbedMimeTypes) == 0 {
		return false
	}
	if p.MaxTextEmbedBytes > 0 && sizeBytes > p.MaxTextEmbedBytes {
		return false
	}
	return mimeAllowed(mediaType, p.TextEmbedMimeTypes)
}

func debugRawSection(enabled bool, label, body string) {
	if ParentHooks.DebugRawSection != nil {
		ParentHooks.DebugRawSection(enabled, label, body)
	}
}

func emitDebugDiagnostic(ctx context.Context, name string, data map[string]any) {
	if ParentHooks.EmitDebugDiagnostic != nil {
		ParentHooks.EmitDebugDiagnostic(ctx, name, data)
	}
}

var (
	fallbackClientOnce sync.Once
	fallbackClient     *http.Client
)

func defaultHTTPClient() *http.Client {
	if ParentHooks.HTTPClient != nil {
		if c := ParentHooks.HTTPClient(); c != nil {
			return c
		}
	}
	fallbackClientOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
		transport.TLSHandshakeTimeout = 15 * time.Second
		transport.ResponseHeaderTimeout = 2 * time.Minute
		transport.IdleConnTimeout = 90 * time.Second
		transport.MaxIdleConnsPerHost = 100
		fallbackClient = &http.Client{Transport: transport}
	})
	return fallbackClient
}

func openAIParametersFromToolSchema(schema map[string]interface{}, strict bool) map[string]interface{} {
	if ParentHooks.OpenAIParametersFromToolSchema != nil {
		return ParentHooks.OpenAIParametersFromToolSchema(schema, strict)
	}
	return schema
}

func supportsReasoningMode(provider, model string) bool {
	return ParentHooks.SupportsReasoningMode != nil && ParentHooks.SupportsReasoningMode(provider, model)
}

func baseModelAndEffortForProvider(provider, model string) (string, string) {
	if ParentHooks.BaseModelAndEffortForProvider != nil {
		return ParentHooks.BaseModelAndEffortForProvider(provider, model)
	}
	return model, ""
}

func isEmbeddedFileText(text string) bool {
	return ParentHooks.IsEmbeddedFileText != nil && ParentHooks.IsEmbeddedFileText(text)
}

func formatUploadedFileNotice(filename, mediaType, localPath string, sizeBytes int64) string {
	if ParentHooks.FormatUploadedFileNotice != nil {
		return ParentHooks.FormatUploadedFileNotice(filename, mediaType, localPath, sizeBytes)
	}
	return "[User uploaded file: " + filename + "]"
}

func canonicalUploadMediaType(filename, mediaType string) string {
	if ParentHooks.CanonicalUploadMediaType != nil {
		return ParentHooks.CanonicalUploadMediaType(filename, mediaType)
	}
	return mediaType
}

func canonicalTextUploadMediaType(filename, mediaType string) string {
	if ParentHooks.CanonicalTextUploadMediaType != nil {
		return ParentHooks.CanonicalTextUploadMediaType(filename, mediaType)
	}
	return mediaType
}

func isTextLikeMediaType(mediaType string) bool {
	return ParentHooks.IsTextLikeMediaType != nil && ParentHooks.IsTextLikeMediaType(mediaType)
}

func defaultOpenAIResponsesFileUploadPolicy() FileUploadPolicy {
	if ParentHooks.DefaultFileUploadPolicy != nil {
		return ParentHooks.DefaultFileUploadPolicy()
	}
	return FileUploadPolicy{}
}
