package audio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestElevenLabsModelsIncludeDocumentedTTSModels(t *testing.T) {
	want := []string{
		"eleven_v3",
		"eleven_multilingual_v2",
		"eleven_flash_v2_5",
		"eleven_flash_v2",
		"eleven_turbo_v2_5",
		"eleven_turbo_v2",
		"eleven_monolingual_v1",
		"eleven_multilingual_v1",
	}
	for _, model := range want {
		if !contains(ElevenLabsModels, model) {
			t.Fatalf("ElevenLabsModels missing %s", model)
		}
	}
}

func TestElevenLabsProviderGenerateWithVoiceID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/text-to-speech/JBFqnCBsd6RMkjVDRZzb" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("xi-api-key"); got != "test-key" {
			t.Fatalf("xi-api-key = %q", got)
		}
		if got := r.URL.Query().Get("output_format"); got != "mp3_44100_128" {
			t.Fatalf("output_format = %q", got)
		}
		if got := r.URL.Query().Get("optimize_streaming_latency"); got != "1" {
			t.Fatalf("optimize_streaming_latency = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["text"] != "hello" || body["model_id"] != "eleven_flash_v2_5" || body["language_code"] != "en" {
			t.Fatalf("unexpected body: %#v", body)
		}
		settings, ok := body["voice_settings"].(map[string]any)
		if !ok || settings["stability"] != 0.5 || settings["similarity_boost"] != 0.75 || settings["use_speaker_boost"] != true {
			t.Fatalf("unexpected voice settings: %#v", body["voice_settings"])
		}
		dictionaries, ok := body["pronunciation_dictionary_locators"].([]any)
		if !ok || len(dictionaries) != 1 {
			t.Fatalf("unexpected pronunciation dictionaries: %#v", body["pronunciation_dictionary_locators"])
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fake-mp3"))
	}))
	defer server.Close()

	provider := NewElevenLabsProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()

	result, err := provider.Generate(context.Background(), Request{
		Input:                     "hello",
		Model:                     "eleven_flash_v2_5",
		Voice:                     "JBFqnCBsd6RMkjVDRZzb",
		Language:                  "en",
		ResponseFormat:            "mp3_44100_128",
		Stability:                 0.5,
		SimilarityBoost:           0.75,
		UseSpeakerBoost:           true,
		UseSpeakerBoostSet:        true,
		PronunciationDictionaries: "dict1:v2",
		OptimizeStreamingLatency:  1,
		EnableLogging:             true,
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if result.MimeType != "audio/mpeg" {
		t.Fatalf("MimeType = %q, want audio/mpeg", result.MimeType)
	}
	if string(result.Data) != "fake-mp3" {
		t.Fatalf("Data = %q, want fake-mp3", string(result.Data))
	}
}

func TestElevenLabsProviderResolvesVoiceName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/voices":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"voices":[{"voice_id":"voice123456789012","name":"Rachel"}]}`))
		case "/v1/text-to-speech/voice123456789012":
			_, _ = w.Write([]byte("fake-mp3"))
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := NewElevenLabsProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()
	_, err := provider.Generate(context.Background(), Request{
		Input:          "hello",
		Voice:          "Rachel",
		ResponseFormat: "mp3_44100_128",
		EnableLogging:  true,
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
}

func TestElevenLabsValidation(t *testing.T) {
	if err := ValidateElevenLabsFormat("wav_44100"); err != nil {
		t.Fatalf("ValidateElevenLabsFormat(wav_44100): %v", err)
	}
	if err := ValidateElevenLabsFormat("wav"); err == nil {
		t.Fatal("expected invalid ElevenLabs format")
	}
	if err := ValidateElevenLabsTextNormalization("auto"); err != nil {
		t.Fatalf("ValidateElevenLabsTextNormalization(auto): %v", err)
	}
	if err := ValidateElevenLabsTextNormalization("maybe"); err == nil {
		t.Fatal("expected invalid text normalization")
	}
}
