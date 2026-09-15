package live

import (
	"encoding/json"
	"reflect"
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

func TestConfigCapabilitiesCodexIsFixed(t *testing.T) {
	cfg := config.LiveConfig{Provider: " codex "}
	cfg.ChatGPT.Model = "ignored"
	cfg.ChatGPT.Voice = "maple"
	got := ConfigCapabilities(cfg)
	wantVoices := []string{config.DefaultLiveChatGPTVoice}
	if got.Provider != config.LiveProviderCodex || got.Model != config.DefaultLiveChatGPTModel || got.Voice != config.DefaultLiveChatGPTVoice || got.CanSetVoice || !reflect.DeepEqual(got.Voices, wantVoices) {
		t.Fatalf("codex capabilities = %+v", got)
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
