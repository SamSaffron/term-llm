package audio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewVeniceProviderTrimsAPIKey(t *testing.T) {
	provider := NewVeniceProvider("  Bearer test-key\n")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestValidateOptions(t *testing.T) {
	if err := ValidateFormat("wav"); err != nil {
		t.Fatalf("ValidateFormat(wav): %v", err)
	}
	if err := ValidateFormat("ogg"); err == nil {
		t.Fatal("expected invalid format error")
	}
	if err := ValidateSpeed(0.25); err != nil {
		t.Fatalf("ValidateSpeed(0.25): %v", err)
	}
	if err := ValidateSpeed(4.01); err == nil {
		t.Fatal("expected invalid speed error")
	}
	if err := ValidateTemperature(2); err != nil {
		t.Fatalf("ValidateTemperature(2): %v", err)
	}
	if err := ValidateTemperature(2.1); err == nil {
		t.Fatal("expected invalid temperature error")
	}
	if err := ValidateTopP(1); err != nil {
		t.Fatalf("ValidateTopP(1): %v", err)
	}
	if err := ValidateTopP(1.1); err == nil {
		t.Fatal("expected invalid top-p error")
	}
}

func TestVeniceModelsIncludeAllDocumentedTTSModels(t *testing.T) {
	want := []string{
		"tts-kokoro",
		"tts-qwen3-0-6b",
		"tts-qwen3-1-7b",
		"tts-xai-v1",
		"tts-inworld-1-5-max",
		"tts-chatterbox-hd",
		"tts-orpheus",
		"tts-elevenlabs-turbo-v2-5",
		"tts-minimax-speech-02-hd",
		"tts-gemini-3-1-flash",
	}
	for _, model := range want {
		if !contains(VeniceModels, model) {
			t.Fatalf("VeniceModels missing %s", model)
		}
	}
}

func TestVeniceProviderGenerate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != veniceSpeechEndpoint {
			t.Fatalf("path = %s, want %s", r.URL.Path, veniceSpeechEndpoint)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["input"] != "hello" || body["model"] != "tts-qwen3-0-6b" || body["voice"] != "Vivian" {
			t.Fatalf("unexpected body: %#v", body)
		}
		if body["language"] != "English" || body["prompt"] != "Very happy." || body["response_format"] != "wav" {
			t.Fatalf("missing options in body: %#v", body)
		}
		if body["streaming"] != true || body["temperature"] != 0.9 || body["top_p"] != 0.8 {
			t.Fatalf("missing sampling options in body: %#v", body)
		}
		w.Header().Set("Content-Type", "audio/wav; charset=binary")
		_, _ = w.Write([]byte("fake-wav"))
	}))
	defer server.Close()

	temperature := 0.9
	topP := 0.8
	provider := NewVeniceProvider("test-key")
	provider.baseURL = server.URL
	provider.client = server.Client()

	result, err := provider.Generate(context.Background(), Request{
		Input:          "hello",
		Model:          "tts-qwen3-0-6b",
		Voice:          "Vivian",
		Language:       "English",
		Prompt:         "Very happy.",
		ResponseFormat: "wav",
		Speed:          1.25,
		Streaming:      true,
		Temperature:    &temperature,
		TopP:           &topP,
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if result.MimeType != "audio/wav" {
		t.Fatalf("MimeType = %q, want audio/wav", result.MimeType)
	}
	if string(result.Data) != "fake-wav" {
		t.Fatalf("Data = %q, want fake-wav", string(result.Data))
	}
}

func TestGenerateRequiresInput(t *testing.T) {
	_, err := NewVeniceProvider("test-key").Generate(context.Background(), Request{Input: "   "})
	if err == nil || !strings.Contains(err.Error(), "input text is required") {
		t.Fatalf("err = %v, want input text required", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
