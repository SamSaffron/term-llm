package image

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

type chatGPTImageRoundTripper func(*http.Request) (*http.Response, error)

func (f chatGPTImageRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// newMockChatGPTProvider builds a ChatGPTProvider wired to an in-memory HTTP
// round-tripper that returns the given SSE body. Used for unit tests that
// exercise generate/edit without touching real credentials.
func newMockChatGPTProvider(sseBody string) *ChatGPTProvider {
	client := &llm.ResponsesClient{
		BaseURL:            "https://example.test/responses",
		GetAuthHeader:      func() string { return "Bearer test-token" },
		DisableServerState: true,
		HTTPClient: &http.Client{
			Transport: chatGPTImageRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sseBody)),
				}, nil
			}),
		},
	}
	return &ChatGPTProvider{
		client: client,
		model:  "gpt-5.4-mini",
	}
}

func TestChatGPTProvider_Generate_ReturnsDecodedImage(t *testing.T) {
	payload := []byte("mock-png-bytes")
	encoded := base64.StdEncoding.EncodeToString(payload)
	sse := "event: response.output_item.done\n" +
		"data: {\"item\":{\"type\":\"image_generation_call\",\"status\":\"completed\",\"result\":\"" + encoded + "\",\"revised_prompt\":\"a blue square\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: [DONE]\n\n"

	p := newMockChatGPTProvider(sse)
	result, err := p.Generate(context.Background(), GenerateRequest{Prompt: "a blue square"})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if string(result.Data) != string(payload) {
		t.Errorf("decoded data mismatch: got %q, want %q", string(result.Data), string(payload))
	}
	if result.MimeType != "image/png" {
		t.Errorf("mime type: got %q, want image/png", result.MimeType)
	}
}

func TestChatGPTProvider_Edit_RequiresInputImage(t *testing.T) {
	p := newMockChatGPTProvider("data: [DONE]\n\n")
	_, err := p.Edit(context.Background(), EditRequest{Prompt: "make it red"})
	if err == nil {
		t.Fatal("expected error when no input image provided")
	}
}

func TestChatGPTProvider_Edit_RejectsMultipleImages(t *testing.T) {
	p := newMockChatGPTProvider("data: [DONE]\n\n")
	_, err := p.Edit(context.Background(), EditRequest{
		Prompt: "combine these",
		InputImages: []InputImage{
			{Data: []byte("a"), Path: "a.png"},
			{Data: []byte("b"), Path: "b.png"},
		},
	})
	if err == nil {
		t.Fatal("expected error when more than one input image provided")
	}
	if !strings.Contains(err.Error(), "single") {
		t.Errorf("error %q should mention 'single image'", err.Error())
	}
}

func TestChatGPTProvider_Edit_SendsInputImageDataURL(t *testing.T) {
	var capturedBody []byte
	client := &llm.ResponsesClient{
		BaseURL:            "https://example.test/responses",
		GetAuthHeader:      func() string { return "Bearer test-token" },
		DisableServerState: true,
		HTTPClient: &http.Client{
			Transport: chatGPTImageRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Body != nil {
					b, _ := io.ReadAll(req.Body)
					capturedBody = b
				}
				resultB64 := base64.StdEncoding.EncodeToString([]byte("edited"))
				sse := "event: response.output_item.done\n" +
					"data: {\"item\":{\"type\":\"image_generation_call\",\"result\":\"" + resultB64 + "\"}}\n\n" +
					"event: response.completed\n" +
					"data: {\"response\":{\"id\":\"resp_edit\"}}\n\n" +
					"data: [DONE]\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sse)),
				}, nil
			}),
		},
	}
	p := &ChatGPTProvider{client: client, model: "gpt-5.4-mini"}

	src := []byte("original-bytes")
	_, err := p.Edit(context.Background(), EditRequest{
		Prompt:      "make it red",
		InputImages: []InputImage{{Data: src, Path: "input.png"}},
	})
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}
	body := string(capturedBody)
	if !strings.Contains(body, "input_image") {
		t.Error("request body should include input_image content part")
	}
	wantDataURL := fmt.Sprintf("data:image/png;base64,%s", base64.StdEncoding.EncodeToString(src))
	if !strings.Contains(body, wantDataURL) {
		t.Errorf("request body missing expected data URL\nwant fragment: %s", wantDataURL)
	}
	if !strings.Contains(body, "\"tool_choice\":{\"type\":\"image_generation\"}") {
		t.Errorf("request body missing image_generation tool_choice; got: %s", body)
	}
}

func TestDecorateChatGPTPrompt(t *testing.T) {
	base := "a red square"
	tests := []struct {
		name        string
		size        string
		aspectRatio string
		wantEqual   string
		wantContain []string
	}{
		{name: "no hints", wantEqual: base},
		{name: "whitespace only", size: "   ", aspectRatio: "\t", wantEqual: base},
		{name: "unknown size dropped", size: "8K", wantEqual: base},
		{
			name:        "size only",
			size:        "2K",
			wantContain: []string{base, "2048×2048", "(2K)"},
		},
		{
			name:        "aspect ratio only",
			aspectRatio: "16:9",
			wantContain: []string{base, "Aspect ratio: 16:9."},
		},
		{
			name:        "size and aspect ratio",
			size:        "4K",
			aspectRatio: "1:1",
			wantContain: []string{base, "4096×4096", "Aspect ratio: 1:1."},
		},
		{
			name:        "lowercase size normalized",
			size:        "1k",
			wantContain: []string{base, "1024×1024", "(1K)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decorateChatGPTPrompt(base, tt.size, tt.aspectRatio)
			if tt.wantEqual != "" && got != tt.wantEqual {
				t.Errorf("decorateChatGPTPrompt(%q, %q, %q) = %q, want %q",
					base, tt.size, tt.aspectRatio, got, tt.wantEqual)
			}
			for _, sub := range tt.wantContain {
				if !strings.Contains(got, sub) {
					t.Errorf("decorateChatGPTPrompt output missing %q\ngot: %q", sub, got)
				}
			}
		})
	}
}

func TestChatGPTSizeHint(t *testing.T) {
	tests := map[string]string{
		"1K":  "Target resolution: approximately 1024×1024 pixels (1K).",
		"2K":  "Target resolution: approximately 2048×2048 pixels (2K).",
		"4K":  "Target resolution: approximately 4096×4096 pixels (4K).",
		"1k":  "Target resolution: approximately 1024×1024 pixels (1K).",
		"8K":  "",
		"":    "",
		"foo": "",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := chatGPTSizeHint(in); got != want {
				t.Errorf("chatGPTSizeHint(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestChatGPTProvider_Generate_IncludesSizeHintInRequestBody(t *testing.T) {
	var capturedBody []byte
	client := &llm.ResponsesClient{
		BaseURL:            "https://example.test/responses",
		GetAuthHeader:      func() string { return "Bearer test-token" },
		DisableServerState: true,
		HTTPClient: &http.Client{
			Transport: chatGPTImageRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Body != nil {
					b, _ := io.ReadAll(req.Body)
					capturedBody = b
				}
				resultB64 := base64.StdEncoding.EncodeToString([]byte("png"))
				sse := "event: response.output_item.done\n" +
					"data: {\"item\":{\"type\":\"image_generation_call\",\"result\":\"" + resultB64 + "\"}}\n\n" +
					"event: response.completed\n" +
					"data: {\"response\":{\"id\":\"resp_size\"}}\n\n" +
					"data: [DONE]\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sse)),
				}, nil
			}),
		},
	}
	p := &ChatGPTProvider{client: client, model: "gpt-5.4-mini"}

	_, err := p.Generate(context.Background(), GenerateRequest{
		Prompt:      "a red square",
		Size:        "4K",
		AspectRatio: "16:9",
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	body := string(capturedBody)
	if !strings.Contains(body, "4096") {
		t.Errorf("request body missing 4K resolution hint; got: %s", body)
	}
	if !strings.Contains(body, "16:9") {
		t.Errorf("request body missing aspect ratio hint; got: %s", body)
	}
}
