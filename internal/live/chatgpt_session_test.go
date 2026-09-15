package live

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

// Codex's V3/frameless protocol uses the V1 voice family. Its V2 default,
// marin, produces a misleading 403 "Voice session access denied" on this route.
func TestSessionJSONDefaultsToCodexV3Voice(t *testing.T) {
	payload, err := SessionJSON(config.LiveConfig{}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var session sessionPayload
	if err := json.Unmarshal(payload, &session); err != nil {
		t.Fatal(err)
	}
	if session.Model != "gpt-live-1-codex" || session.Audio.Output.Voice != "cove" {
		t.Fatalf("default V3 model/voice = %q/%q, want gpt-live-1-codex/cove", session.Model, session.Audio.Output.Voice)
	}
}

func TestResolvedInstructionsAppendsContextAfterDefaultOrCustomPrompt(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		opts       SessionOptions
		want       string
	}{
		{name: "default", opts: SessionOptions{Context: "facts"}, want: DefaultInstructions + "\n\nfacts"},
		{name: "configured", configured: "configured prompt", opts: SessionOptions{Context: "facts"}, want: "configured prompt\n\nfacts"},
		{name: "per-session", configured: "configured prompt", opts: SessionOptions{Instructions: "session prompt", Context: "facts"}, want: "session prompt\n\nfacts"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolvedInstructions(test.configured, test.opts); got != test.want {
				t.Fatalf("instructions = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSessionJSONVoiceFamily(t *testing.T) {
	voices := config.LiveChatGPTVoices()
	for _, voice := range append(voices, " cove ") {
		t.Run(voice, func(t *testing.T) {
			cfg := config.LiveConfig{ChatGPT: config.LiveChatGPTConfig{Voice: voice}}
			if _, err := SessionJSON(cfg, SessionOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, voice := range []string{"marin", "cedar", "alloy", "unknown"} {
		t.Run(voice, func(t *testing.T) {
			cfg := config.LiveConfig{ChatGPT: config.LiveChatGPTConfig{Voice: voice}}
			p := NewChatGPTProvider(cfg, func(context.Context, bool) (Auth, error) {
				t.Fatal("invalid voice must fail before accessing credentials or the network")
				return Auth{}, nil
			}, nil)
			_, err := p.Start(context.Background(), "offer", SessionOptions{})
			if err == nil || !strings.Contains(err.Error(), "live.chatgpt.voice") || !strings.Contains(err.Error(), "cove") {
				t.Fatalf("invalid voice error = %v", err)
			}
		})
	}
}

func TestLiveChatGPTVoicesReturnsCopyUsedByValidation(t *testing.T) {
	voices := config.LiveChatGPTVoices()
	if len(voices) == 0 {
		t.Fatal("supported voice list is empty")
	}
	first := voices[0]
	voices[0] = "mutated"
	if fresh := config.LiveChatGPTVoices(); fresh[0] != first {
		t.Fatalf("voice list mutation leaked: %v", fresh)
	}
	for _, voice := range config.LiveChatGPTVoices() {
		if err := (config.LiveChatGPTConfig{Voice: voice}).ValidateVoice(); err != nil {
			t.Fatalf("listed voice %q failed validation: %v", voice, err)
		}
	}
}

func TestDefaultProviderUsesDirectChatGPT(t *testing.T) {
	for _, name := range []string{"", config.DefaultLiveProvider, config.LiveProviderChatGPT} {
		p, err := NewProvider(config.LiveConfig{Provider: name})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.(*ChatGPTProvider); !ok {
			t.Fatalf("provider %q = %T, want direct ChatGPT without a Codex executable", name, p)
		}
		if p.Name() != config.LiveProviderChatGPT {
			t.Fatalf("provider name = %q", p.Name())
		}
	}
}
