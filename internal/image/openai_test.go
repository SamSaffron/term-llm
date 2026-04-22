package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

type openAIImageRoundTripper func(*http.Request) (*http.Response, error)

func (f openAIImageRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOpenAILegacySizeFromAspectRatio(t *testing.T) {
	tests := []struct {
		name  string
		ratio string
		want  string
	}{
		{"1:1", "1:1", "1024x1024"},
		{"empty defaults to square", "", "1024x1024"},
		{"16:9", "16:9", "1536x1024"},
		{"4:3", "4:3", "1536x1024"},
		{"3:2", "3:2", "1536x1024"},
		{"9:16", "9:16", "1024x1536"},
		{"3:4", "3:4", "1024x1536"},
		{"2:3", "2:3", "1024x1536"},
		{"unknown defaults to square", "unknown", "1024x1024"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := openaiLegacySizeFromAspectRatio(tt.ratio)
			if got != tt.want {
				t.Errorf("openaiLegacySizeFromAspectRatio(%q) = %q, want %q", tt.ratio, got, tt.want)
			}
		})
	}
}

func TestOpenAISizeFromRequestGPTImage2(t *testing.T) {
	tests := []struct {
		name        string
		size        string
		aspectRatio string
		want        string
	}{
		{"default square", "", "", "1024x1024"},
		{"1K square", "1K", "1:1", "1024x1024"},
		{"1K landscape exact", "1K", "16:9", "1280x720"},
		{"2K landscape exact", "2K", "16:9", "2560x1440"},
		{"2K portrait exact", "2K", "9:16", "1440x2560"},
		{"4K square", "4K", "1:1", "2880x2880"},
		{"4K landscape exact", "4K", "16:9", "3840x2160"},
		{"4K portrait exact", "4K", "9:16", "2160x3840"},
		{"4K four by three", "4K", "4:3", "3264x2448"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := openaiSizeFromRequest("gpt-image-2", tt.size, tt.aspectRatio)
			if got != tt.want {
				t.Errorf("openaiSizeFromRequest(%q, %q, %q) = %q, want %q", "gpt-image-2", tt.size, tt.aspectRatio, got, tt.want)
			}
		})
	}
}

func TestOpenAIProviderGenerateUsesConfiguredModel(t *testing.T) {
	oldClient := openaiHTTPClient
	defer func() { openaiHTTPClient = oldClient }()

	var captured openaiGenerateRequest
	openaiHTTPClient = &http.Client{
		Transport: openAIImageRoundTripper(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(body, &captured); err != nil {
				return nil, err
			}
			resp := `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("png")) + `"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		}),
	}

	p := NewOpenAIProvider("test-key", "gpt-image-2")
	_, err := p.Generate(context.Background(), GenerateRequest{
		Prompt:      "a neon fox",
		Size:        "4K",
		AspectRatio: "16:9",
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if captured.Model != "gpt-image-2" {
		t.Fatalf("model = %q, want %q", captured.Model, "gpt-image-2")
	}
	if captured.Size != "3840x2160" {
		t.Fatalf("size = %q, want %q", captured.Size, "3840x2160")
	}
}

func TestOpenAIProviderEditUsesConfiguredModelAndMultipleImages(t *testing.T) {
	oldClient := openaiHTTPClient
	defer func() { openaiHTTPClient = oldClient }()

	var (
		capturedContentType string
		capturedBody        []byte
	)
	openaiHTTPClient = &http.Client{
		Transport: openAIImageRoundTripper(func(req *http.Request) (*http.Response, error) {
			capturedContentType = req.Header.Get("Content-Type")
			if req.Body != nil {
				var err error
				capturedBody, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
			}
			resp := `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("edited")) + `"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		}),
	}

	p := NewOpenAIProvider("test-key", "gpt-image-2")
	if !p.SupportsMultiImage() {
		t.Fatal("SupportsMultiImage() = false, want true for gpt-image-2")
	}

	_, err := p.Edit(context.Background(), EditRequest{
		Prompt: "combine these references",
		InputImages: []InputImage{
			{Path: "one.png", Data: []byte("img-1")},
			{Path: "two.png", Data: []byte("img-2")},
		},
		Size:        "2K",
		AspectRatio: "9:16",
	})
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}

	mediaType, params, err := mime.ParseMediaType(capturedContentType)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", capturedContentType, err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("content type = %q, want multipart/form-data", mediaType)
	}
	reader := multipart.NewReader(strings.NewReader(string(capturedBody)), params["boundary"])

	var (
		model      string
		size       string
		imageCount int
	)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("ReadAll(part): %v", err)
		}
		switch part.FormName() {
		case "model":
			model = string(data)
		case "size":
			size = string(data)
		case "image[]":
			imageCount++
		}
	}
	if model != "gpt-image-2" {
		t.Fatalf("multipart model = %q, want %q", model, "gpt-image-2")
	}
	if size != "1440x2560" {
		t.Fatalf("multipart size = %q, want %q", size, "1440x2560")
	}
	if imageCount != 2 {
		t.Fatalf("image count = %d, want 2", imageCount)
	}
}
