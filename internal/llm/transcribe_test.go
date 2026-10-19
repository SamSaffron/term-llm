package llm

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/openaiapi"
)

func TestTranscribeWithConfig_ChatGPTDispatchesToOAuthBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	audio := filepath.Join(t.TempDir(), "memo.ogg")
	if err := os.WriteFile(audio, []byte("audio-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Transcription: config.TranscriptionConfig{Model: "ignored-model"}}
	_, err := TranscribeWithConfig(context.Background(), cfg, audio, "en", "chatgpt")
	if err == nil || !strings.Contains(err.Error(), "ChatGPT transcription is not configured") {
		t.Fatalf("TranscribeWithConfig error = %v, want ChatGPT OAuth setup guidance", err)
	}
}

func TestTranscribeWithConfig_UsesResolvedURLForBuiltins(t *testing.T) {
	origClient := defaultHTTPClient
	origProbe := probeAudioDuration
	defer func() {
		defaultHTTPClient = origClient
		probeAudioDuration = origProbe
	}()
	probeAudioDuration = func(context.Context, string) (time.Duration, error) {
		return time.Minute, nil
	}

	audioFile, err := os.CreateTemp(t.TempDir(), "audio-*.wav")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if _, err := audioFile.WriteString("not really audio"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}

	tests := []struct {
		name             string
		providerOverride string
		cfg              *config.Config
		wantURL          string
	}{
		{
			name:             "openai",
			providerOverride: "openai",
			cfg: &config.Config{
				Providers: map[string]config.ProviderConfig{
					"openai": {
						BaseURL:        "https://base.example.test/v1",
						ResolvedURL:    "https://resolved.example.test/v1",
						ResolvedAPIKey: "test-key",
					},
				},
			},
			wantURL: "https://resolved.example.test/v1/audio/transcriptions",
		},
		{
			name:             "mistral",
			providerOverride: "mistral",
			cfg: &config.Config{
				Providers: map[string]config.ProviderConfig{
					"mistral": {
						BaseURL:        "https://base.mistral.test/v1",
						ResolvedURL:    "https://resolved.mistral.test/v1",
						ResolvedAPIKey: "test-key",
					},
				},
			},
			wantURL: "https://resolved.mistral.test/v1/audio/transcriptions",
		},
		{
			name:             "local",
			providerOverride: "local",
			cfg: &config.Config{
				Providers: map[string]config.ProviderConfig{
					"local_whisper": {
						BaseURL:     "https://base.local.test/v1",
						ResolvedURL: "https://resolved.local.test/v1",
					},
				},
			},
			wantURL: "https://resolved.local.test/v1/inference",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedURL := ""
			defaultHTTPClient = &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					capturedURL = req.URL.String()
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"text":"ok"}`)),
					}, nil
				}),
			}

			text, err := TranscribeWithConfig(context.Background(), tt.cfg, audioFile.Name(), "", tt.providerOverride)
			if err != nil {
				t.Fatalf("TranscribeWithConfig failed: %v", err)
			}
			if text != "ok" {
				t.Fatalf("text = %q, want %q", text, "ok")
			}
			if capturedURL != tt.wantURL {
				t.Fatalf("request URL = %q, want %q", capturedURL, tt.wantURL)
			}
		})
	}
}

func TestTranscribeWithConfig_ElevenLabsPassesTimestampOptions(t *testing.T) {
	origClient := defaultHTTPClient
	origProbe := probeAudioDuration
	defer func() {
		defaultHTTPClient = origClient
		probeAudioDuration = origProbe
	}()
	probeAudioDuration = func(context.Context, string) (time.Duration, error) {
		return time.Minute, nil
	}

	audioFile, err := os.CreateTemp(t.TempDir(), "audio-*.mp3")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if _, err := audioFile.WriteString("not really audio"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}

	var requestBody string
	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			requestBody = string(body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"text":"hello","words":[{"text":"hello","start":0,"end":0.5,"type":"word","logprob":-0.1}]}`)),
			}, nil
		}),
	}

	result, err := TranscribeWithConfig(context.Background(), &config.Config{
		Transcription: config.TranscriptionConfig{
			Timestamps: true,
		},
		Providers: map[string]config.ProviderConfig{
			"elevenlabs": {ResolvedAPIKey: "test-key"},
		},
	}, audioFile.Name(), "", "elevenlabs")
	if err != nil {
		t.Fatalf("TranscribeWithConfig failed: %v", err)
	}
	for _, want := range []string{"timestamps_granularity", "word", "no_verbatim", "true", "tag_audio_events", "false"} {
		if !strings.Contains(requestBody, want) {
			t.Fatalf("request body missing %q: %s", want, requestBody)
		}
	}
	if !strings.Contains(result, `"words": [`) {
		t.Fatalf("result = %q, want timestamp JSON", result)
	}
}

func TestTranscribeWithConfig_TruncatesImplausiblyLongTranscript(t *testing.T) {
	origClient := defaultHTTPClient
	origProbe := probeAudioDuration
	defer func() {
		defaultHTTPClient = origClient
		probeAudioDuration = origProbe
	}()
	probeAudioDuration = func(context.Context, string) (time.Duration, error) {
		return time.Minute, nil
	}

	audioFile, err := os.CreateTemp(t.TempDir(), "audio-*.wav")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if _, err := audioFile.WriteString("not really audio"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}

	// 351 words for 1 minute of audio exceeds 350 WPM cap
	longTranscript := strings.TrimSpace(strings.Repeat("word ", 351))
	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"text":"` + longTranscript + `"}`)),
			}, nil
		}),
	}

	result, err := TranscribeWithConfig(context.Background(), &config.Config{
		Providers: map[string]config.ProviderConfig{
			"openai": {ResolvedAPIKey: "test-key"},
		},
	}, audioFile.Name(), "", "openai")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be truncated to 350 words (ceil(1 minute * 350 WPM))
	wordCount := len(strings.Fields(result))
	if wordCount != 350 {
		t.Fatalf("word count = %d, want 350 (truncated from 351)", wordCount)
	}
}

func TestTranscribeWithConfig_OpenAIUsesSharedEndpoint(t *testing.T) {
	origClient := defaultHTTPClient
	origProbe := probeAudioDuration
	t.Cleanup(func() {
		defaultHTTPClient = origClient
		probeAudioDuration = origProbe
	})
	probeAudioDuration = func(context.Context, string) (time.Duration, error) { return time.Minute, nil }

	audioPath := filepath.Join(t.TempDir(), "audio.wav")
	if err := os.WriteFile(audioPath, []byte("not really audio"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	tests := []struct {
		name     string
		env      map[string]string
		cfg      *config.Config
		wantURL  string
		wantAuth string
		wantOrg  string
		wantProj string
	}{
		{
			name: "env key with env endpoint",
			env: map[string]string{
				"OPENAI_API_KEY": "env-key", openaiapi.EnvBaseURL: "https://env.example/v1",
				openaiapi.EnvOrganization: "org-env", openaiapi.EnvProject: "proj-env",
			},
			cfg:      &config.Config{},
			wantURL:  "https://env.example/v1/audio/transcriptions",
			wantAuth: "Bearer env-key",
			wantOrg:  "org-env",
			wantProj: "proj-env",
		},
		{
			name: "provider base_url and headers",
			env:  map[string]string{openaiapi.EnvOrganization: "org-env", openaiapi.EnvProject: "proj-env"},
			cfg: &config.Config{Providers: map[string]config.ProviderConfig{
				"openai": {
					ResolvedAPIKey: "config-key",
					BaseURL:        "https://gateway.example/v1",
					Headers:        map[string]string{"OpenAI-Organization": "org-config"},
				},
			}},
			wantURL:  "https://gateway.example/v1/audio/transcriptions",
			wantAuth: "Bearer config-key",
			wantOrg:  "org-config",
			wantProj: "proj-env",
		},
		{
			name: "non-native openai entry keeps its own endpoint and no scoping",
			env:  map[string]string{openaiapi.EnvBaseURL: "https://env.example/v1", openaiapi.EnvOrganization: "org-env"},
			cfg: &config.Config{Providers: map[string]config.ProviderConfig{
				"openai": {
					Type:           config.ProviderTypeOpenAICompat,
					ResolvedAPIKey: "compat-key",
					BaseURL:        "https://compat.example/v1",
				},
			}},
			wantURL:  "https://compat.example/v1/audio/transcriptions",
			wantAuth: "Bearer compat-key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"OPENAI_API_KEY", openaiapi.EnvBaseURL, openaiapi.EnvOrganization, openaiapi.EnvProject} {
				t.Setenv(key, tt.env[key])
			}
			var got *http.Request
			defaultHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"text":"ok"}`)),
				}, nil
			})}

			if _, err := TranscribeWithConfig(context.Background(), tt.cfg, audioPath, "", "openai"); err != nil {
				t.Fatalf("TranscribeWithConfig: %v", err)
			}
			if got == nil {
				t.Fatal("no request sent")
			}
			if got.URL.String() != tt.wantURL || got.Header.Get("Authorization") != tt.wantAuth ||
				got.Header.Get("OpenAI-Organization") != tt.wantOrg || got.Header.Get("OpenAI-Project") != tt.wantProj {
				t.Fatalf("request = %s auth=%q org=%q project=%q", got.URL, got.Header.Get("Authorization"), got.Header.Get("OpenAI-Organization"), got.Header.Get("OpenAI-Project"))
			}
		})
	}
}
