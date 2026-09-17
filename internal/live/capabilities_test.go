package live

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestConfigCapabilitiesDirectResolvesDefaultsAndCopiesVoices(t *testing.T) {
	got := ConfigCapabilities(config.LiveConfig{})
	if got.Provider != config.LiveProviderChatGPT || got.Model != config.DefaultLiveChatGPTModel || got.Voice != config.DefaultLiveChatGPTVoice || !got.CanSetVoice {
		t.Fatalf("default direct capabilities = %+v", got)
	}
	if !reflect.DeepEqual(got.Voices, config.LiveChatGPTVoices()) {
		t.Fatalf("voices = %v", got.Voices)
	}

	got.Voices[0] = "mutated"
	fresh := ConfigCapabilities(config.LiveConfig{})
	if fresh.Voices[0] == "mutated" {
		t.Fatal("capability voices share mutable backing storage")
	}

	cfg := config.LiveConfig{Provider: " chatgpt "}
	cfg.ChatGPT.Model = " custom-live "
	cfg.ChatGPT.Voice = " maple "
	got = ConfigCapabilities(cfg)
	if got.Provider != "chatgpt" || got.Model != "custom-live" || got.Voice != "maple" || !got.CanSetVoice {
		t.Fatalf("configured direct capabilities = %+v", got)
	}
}

func TestProviderRejectsRemovedCodexTransport(t *testing.T) {
	if _, err := NewProvider(config.LiveConfig{Provider: "codex"}); err == nil {
		t.Fatal("removed Codex transport must not be constructed")
	}
}

func TestConfigCapabilitiesOpenAIUsesPublicDefaultsAndCannotChangeVoice(t *testing.T) {
	cfg := config.LiveConfig{Provider: " openai "}
	got := ConfigCapabilities(cfg)
	if got.Provider != config.LiveProviderOpenAI || got.Model != config.DefaultLiveOpenAIModel || got.Voice != config.DefaultLiveOpenAIVoice || got.CanSetVoice {
		t.Fatalf("default OpenAI capabilities = %+v", got)
	}
	if !reflect.DeepEqual(got.Voices, config.LiveOpenAIVoices()) {
		t.Fatalf("OpenAI voices = %v", got.Voices)
	}
	got.Voices[0] = "mutated"
	if ConfigCapabilities(cfg).Voices[0] == "mutated" {
		t.Fatal("OpenAI capability voices share mutable backing storage")
	}

	cfg.OpenAI.Model = " custom-realtime "
	cfg.OpenAI.Voice = " cedar "
	got = ConfigCapabilities(cfg)
	if got.Model != "custom-realtime" || got.Voice != "cedar" || got.CanSetVoice {
		t.Fatalf("configured OpenAI capabilities = %+v", got)
	}
	context := CapabilityContext(got)
	if !strings.Contains(context, "cannot be changed during this call") {
		t.Fatalf("OpenAI capability context = %q", context)
	}
}

func TestConfigCapabilitiesGeminiUsesPCMDefaults(t *testing.T) {
	cfg := config.LiveConfig{Provider: config.LiveProviderGemini}
	got := ConfigCapabilities(cfg)
	if got.Provider != config.LiveProviderGemini || got.Model != config.DefaultLiveGeminiModel || got.Voice != config.DefaultLiveGeminiVoice || got.CanSetVoice {
		t.Fatalf("Gemini capabilities = %+v", got)
	}
	if !slices.Contains(got.Voices, config.DefaultLiveGeminiVoice) {
		t.Fatalf("Gemini voices = %v", got.Voices)
	}
	cfg.Gemini.Model = "custom-live"
	cfg.Gemini.Voice = "Aoede"
	got = ConfigCapabilities(cfg)
	if got.Model != "custom-live" || got.Voice != "Aoede" {
		t.Fatalf("configured Gemini capabilities = %+v", got)
	}
}

func TestCapabilitiesAreJSONSafeAndContextIsAuthoritative(t *testing.T) {
	capabilities := ConfigCapabilities(config.LiveConfig{})
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"provider"`, `"model"`, `"voice"`, `"voices"`, `"can_set_voice"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("capability JSON %s missing %s", encoded, field)
		}
	}

	context := CapabilityContext(capabilities)
	for _, fact := range []string{"term-llm", capabilities.Provider, capabilities.Model, capabilities.Voice, "Supported voices", "can be requested through the execution backend"} {
		if !strings.Contains(context, fact) {
			t.Fatalf("capability context missing %q: %s", fact, context)
		}
	}
	if strings.Contains(strings.ToLower(context), "token") || strings.Contains(strings.ToLower(context), "credential") {
		t.Fatalf("capability context mentions credentials: %s", context)
	}
}

func TestOpenAIRealtimeCapabilitiesExcludeLiveOnlyVoices(t *testing.T) {
	cfg := config.LiveConfig{Provider: config.LiveProviderOpenAI, OpenAI: config.LiveOpenAIConfig{Model: "gpt-realtime"}}
	got := ConfigCapabilities(cfg)
	if got.Model != "gpt-realtime" || got.Voice != "marin" {
		t.Fatalf("capabilities = %+v", got)
	}
	for _, voice := range got.Voices {
		if voice == "quartz" {
			t.Fatal("Live-only voice advertised for Realtime")
		}
	}
	cfg.OpenAI.Model = "gpt-live-1"
	cfg.OpenAI.Voice = "quartz"
	got = ConfigCapabilities(cfg)
	if got.Voice != "quartz" {
		t.Fatalf("voice = %q", got.Voice)
	}
}
