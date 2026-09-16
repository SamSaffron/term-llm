package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestEffectiveFileUploadPolicyForProviderConfig_DisablesNativeWithEmptyList(t *testing.T) {
	policy := EffectiveFileUploadPolicyForProviderConfig("openai", config.ProviderConfig{
		Type: config.ProviderTypeOpenAI,
		FileUpload: &config.FileUploadConfig{
			NativeMimeTypes: []string{},
		},
	})

	if policy.AllowsNative("application/pdf", 1024) {
		t.Fatal("empty native_mime_types should disable native file forwarding")
	}
	if !policy.AllowsTextEmbed("text/csv", 1024) {
		t.Fatal("text embed defaults should still apply when only native list is overridden")
	}
}

func TestEffectiveFileUploadPolicyForProviderConfig_CustomOpenAIPolicy(t *testing.T) {
	policy := EffectiveFileUploadPolicyForProviderConfig("openai", config.ProviderConfig{
		Type: config.ProviderTypeOpenAI,
		FileUpload: &config.FileUploadConfig{
			NativeMimeTypes: []string{"application/pdf"},
			MaxNativeBytes:  1234,
		},
	})

	if !policy.AllowsNative("application/pdf", 1234) {
		t.Fatal("custom native_mime_types should opt provider into that MIME type")
	}
	if policy.AllowsNative("application/pdf", 1235) {
		t.Fatal("max_native_bytes should be enforced")
	}
	if policy.AllowsNative("application/zip", 10) {
		t.Fatal("unlisted MIME type should not be native")
	}
}

func TestDefaultResponsesFilePolicyRequiresKnownMIME(t *testing.T) {
	policy := DefaultOpenAIResponsesFileUploadPolicy()
	for _, mediaType := range []string{"text/x-ruby-script", "text/x-unknown", "application/x-unknown", "application/octet-stream", ""} {
		if policy.AllowsNative(mediaType, 6) || policy.AllowsTextEmbed(mediaType, 6) {
			t.Errorf("unknown MIME %q should use a path notice", mediaType)
		}
	}
	// Non-Responses providers retain their existing portable text fallback.
	if !DefaultPortableTextFileUploadPolicy().AllowsTextEmbed("text/x-ruby-script", 6) {
		t.Fatal("portable text fallback changed")
	}
}
