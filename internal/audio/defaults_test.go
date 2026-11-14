package audio

import (
	"testing"

	"github.com/term-llm/term-llm/internal/config"
)

func TestAudioDefaultsMatchConfig(t *testing.T) {
	checks := map[string]string{
		"venice.model":      DefaultModel,
		"venice.voice":      DefaultVoice,
		"venice.format":     DefaultFormat,
		"gemini.model":      geminiDefaultModel,
		"gemini.voice":      geminiDefaultVoice,
		"gemini.format":     geminiDefaultFormat,
		"elevenlabs.model":  elevenLabsDefaultModel,
		"elevenlabs.voice":  elevenLabsDefaultVoice,
		"elevenlabs.format": elevenLabsDefaultFormat,
	}
	want := map[string]string{
		"venice.model":      config.DefaultAudioVeniceModel,
		"venice.voice":      config.DefaultAudioVeniceVoice,
		"venice.format":     config.DefaultAudioVeniceFormat,
		"gemini.model":      config.DefaultAudioGeminiModel,
		"gemini.voice":      config.DefaultAudioGeminiVoice,
		"gemini.format":     config.DefaultAudioGeminiFormat,
		"elevenlabs.model":  config.DefaultAudioElevenLabsModel,
		"elevenlabs.voice":  config.DefaultAudioElevenLabsVoice,
		"elevenlabs.format": config.DefaultAudioElevenLabsFormat,
	}
	for key, got := range checks {
		if got != want[key] {
			t.Fatalf("%s default = %q, want %q", key, got, want[key])
		}
	}
	if DefaultSpeed != config.DefaultAudioVeniceSpeed {
		t.Fatalf("venice speed = %v, want %v", DefaultSpeed, config.DefaultAudioVeniceSpeed)
	}
}
