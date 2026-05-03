package audio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiModelsIncludeSupportedTTSModels(t *testing.T) {
	want := []string{
		"gemini-3.1-flash-tts-preview",
		"gemini-2.5-flash-preview-tts",
		"gemini-2.5-pro-preview-tts",
	}
	for _, model := range want {
		if !contains(GeminiModels, model) {
			t.Fatalf("GeminiModels missing %s", model)
		}
	}
}

func TestGeminiProviderGenerateSingleSpeakerWAV(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gemini-3.1-flash-tts-preview:generateContent" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Fatalf("x-goog-api-key = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gemini-3.1-flash-tts-preview" {
			t.Fatalf("model = %#v", body["model"])
		}
		config := body["generationConfig"].(map[string]any)
		speech := config["speechConfig"].(map[string]any)
		voice := speech["voiceConfig"].(map[string]any)
		prebuilt := voice["prebuiltVoiceConfig"].(map[string]any)
		if prebuilt["voiceName"] != "Kore" {
			t.Fatalf("voiceName = %#v", prebuilt["voiceName"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"parts": []map[string]any{{
						"inlineData": map[string]string{
							"mimeType": "audio/l16; rate=24000; channels=1",
							"data":     "AQIDBA==",
						},
					}},
				},
			}},
		})
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()

	result, err := provider.Generate(context.Background(), Request{Input: "hello", ResponseFormat: "wav"})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if result.MimeType != "audio/wav" || result.Format != "wav" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.HasPrefix(string(result.Data), "RIFF") {
		t.Fatalf("expected WAV header, got %q", result.Data[:4])
	}
}

func TestGeminiProviderGenerateMultiSpeakerPCM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		config := body["generationConfig"].(map[string]any)
		speech := config["speechConfig"].(map[string]any)
		multi := speech["multiSpeakerVoiceConfig"].(map[string]any)
		speakers := multi["speakerVoiceConfigs"].([]any)
		if len(speakers) != 2 {
			t.Fatalf("speaker count = %d", len(speakers))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"parts": []map[string]any{{
						"inlineData": map[string]string{"mimeType": "audio/l16; rate=24000; channels=1", "data": "AQIDBA=="},
					}},
				},
			}},
		})
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()

	result, err := provider.Generate(context.Background(), Request{
		Input:          "Joe: hi\nJane: hello",
		ResponseFormat: "pcm",
		Speaker1:       "Joe",
		Voice1:         "Kore",
		Speaker2:       "Jane",
		Voice2:         "Puck",
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if string(result.Data) != "\x01\x02\x03\x04" {
		t.Fatalf("Data = %q", result.Data)
	}
}
