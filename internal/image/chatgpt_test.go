package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/credentials"
)

type chatGPTImageRoundTripper func(*http.Request) (*http.Response, error)

func (f chatGPTImageRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newMockChatGPTProvider(roundTrip chatGPTImageRoundTripper) *ChatGPTProvider {
	return &ChatGPTProvider{
		creds:            &credentials.ChatGPTCredentials{AccessToken: "test-token", AccountID: "test-account"},
		client:           &http.Client{Transport: roundTrip},
		model:            "gpt-image-2.5-flare",
		generateEndpoint: "https://example.test/images/generations",
		editEndpoint:     "https://example.test/images/edits",
	}
}

func chatGPTImageSuccess(data []byte) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"created": 1,
		"data": []map[string]string{{
			"b64_json": base64.StdEncoding.EncodeToString(data),
		}},
		"output_format": "png",
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

func TestChatGPTProviderGenerateUsesCodexImagesEndpoint(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte
	provider := newMockChatGPTProvider(func(req *http.Request) (*http.Response, error) {
		captured = req
		capturedBody, _ = io.ReadAll(req.Body)
		return chatGPTImageSuccess([]byte("mock-png-bytes")), nil
	})

	result, err := provider.Generate(context.Background(), GenerateRequest{
		Prompt:      "a blue square",
		Size:        "2K",
		AspectRatio: "16:9",
		Quality:     "low",
		Background:  "transparent",
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if string(result.Data) != "mock-png-bytes" || result.MimeType != "image/png" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if captured.URL.String() != provider.generateEndpoint {
		t.Fatalf("URL = %q, want %q", captured.URL, provider.generateEndpoint)
	}
	for key, want := range map[string]string{
		"Authorization":      "Bearer test-token",
		"ChatGPT-Account-ID": "test-account",
		"originator":         "term-llm",
		"Accept":             "application/json",
	} {
		if got := captured.Header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if got := captured.Header.Get("User-Agent"); !strings.HasPrefix(got, "term-llm/") {
		t.Errorf("User-Agent = %q, want term-llm version", got)
	}

	var payload chatGPTImageGenerationRequest
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if payload.Model != "gpt-image-2.5-flare" || payload.Prompt != "a blue square" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload.Background != "transparent" || payload.Quality != "low" || payload.Size != "2560x1440" {
		t.Fatalf("unexpected image options: %+v", payload)
	}
}

func TestChatGPTProviderEditUsesJSONImageURLs(t *testing.T) {
	var capturedBody []byte
	provider := newMockChatGPTProvider(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://example.test/images/edits" {
			t.Fatalf("URL = %q", req.URL)
		}
		capturedBody, _ = io.ReadAll(req.Body)
		return chatGPTImageSuccess([]byte("edited")), nil
	})

	input := []byte("original-bytes")
	result, err := provider.Edit(context.Background(), EditRequest{
		Prompt:      "make it red",
		InputImages: []InputImage{{Data: input, Path: "input.png"}},
		Quality:     "high",
		Background:  "opaque",
	})
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}
	if string(result.Data) != "edited" {
		t.Fatalf("result = %q", result.Data)
	}

	var payload chatGPTImageEditRequest
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(payload.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(payload.Images))
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(input)
	if payload.Images[0].ImageURL != wantURL {
		t.Fatalf("image URL = %q, want %q", payload.Images[0].ImageURL, wantURL)
	}
	if payload.Size != "auto" || payload.Model != "gpt-image-2.5-flare" || payload.Quality != "high" || payload.Background != "opaque" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestChatGPTProviderEditValidatesInputCount(t *testing.T) {
	provider := newMockChatGPTProvider(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected HTTP request")
		return nil, nil
	})
	if _, err := provider.Edit(context.Background(), EditRequest{Prompt: "red"}); err == nil {
		t.Fatal("expected missing input error")
	}
	_, err := provider.Edit(context.Background(), EditRequest{
		Prompt: "combine",
		InputImages: []InputImage{
			{Data: []byte("a"), Path: "a.png"},
			{Data: []byte("b"), Path: "b.png"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "single image") {
		t.Fatalf("expected single-image error, got %v", err)
	}
}

func TestChatGPTProviderReturnsStatusError(t *testing.T) {
	provider := newMockChatGPTProvider(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"detail":"unsupported model"}`)),
		}, nil
	})
	_, err := provider.Generate(context.Background(), GenerateRequest{Prompt: "test"})
	if err == nil || !strings.Contains(err.Error(), "unsupported model") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeChatGPTImageModel(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "gpt-image-2.5-flare"},
		{"gpt-5.4-mini", "gpt-image-2.5-flare"},
		{"gpt-5.4", "gpt-image-2.5-flare"},
		{"gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst"},
	} {
		if got := normalizeChatGPTImageModel(tc.input); got != tc.want {
			t.Errorf("normalizeChatGPTImageModel(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestChatGPTImageSize(t *testing.T) {
	for _, tc := range []struct {
		size, aspectRatio, want string
	}{
		{"", "", "auto"},
		{"1K", "", "1024x1024"},
		{"", "16:9", "1280x720"},
		{"2K", "16:9", "2560x1440"},
		{"4K", "9:16", "2160x3840"},
	} {
		if got := chatGPTImageSize(tc.size, tc.aspectRatio); got != tc.want {
			t.Errorf("chatGPTImageSize(%q, %q) = %q, want %q", tc.size, tc.aspectRatio, got, tc.want)
		}
	}
}
