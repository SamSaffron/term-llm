package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type geminiImageRoundTripper func(*http.Request) (*http.Response, error)

func (f geminiImageRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNewGeminiProviderDefaults(t *testing.T) {
	p := NewGeminiProvider("key", "", "")
	if p.model != geminiDefaultModel {
		t.Errorf("expected default model %q, got %q", geminiDefaultModel, p.model)
	}
	if p.defaultSize != "" {
		t.Errorf("expected empty defaultSize, got %q", p.defaultSize)
	}
}

func TestNewGeminiProviderCustom(t *testing.T) {
	p := NewGeminiProvider("key", "gemini-2.0-flash", "4K")
	if p.model != "gemini-2.0-flash" {
		t.Errorf("expected model %q, got %q", "gemini-2.0-flash", p.model)
	}
	if p.defaultSize != "4K" {
		t.Errorf("expected defaultSize %q, got %q", "4K", p.defaultSize)
	}
}

func TestGeminiImageConfigSerialization(t *testing.T) {
	tests := []struct {
		name       string
		reqSize    string
		defaultSz  string
		wantConfig bool
		wantSize   string
	}{
		{"request size wins", "4K", "2K", true, "4K"},
		{"config default used", "", "2K", true, "2K"},
		{"both empty omits imageConfig", "", "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			genCfg := geminiGenerationConfig{
				ResponseModalities: []string{"TEXT", "IMAGE"},
			}
			effectiveSize := tt.reqSize
			if effectiveSize == "" {
				effectiveSize = tt.defaultSz
			}
			if effectiveSize != "" {
				genCfg.ImageConfig = &geminiImageConfig{ImageSize: effectiveSize}
			}

			data, err := json.Marshal(genCfg)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}

			var m map[string]interface{}
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}

			_, hasConfig := m["imageConfig"]
			if hasConfig != tt.wantConfig {
				t.Errorf("imageConfig present=%v, want %v (json: %s)", hasConfig, tt.wantConfig, data)
			}
			if tt.wantConfig {
				cfg := m["imageConfig"].(map[string]interface{})
				if got := cfg["imageSize"].(string); got != tt.wantSize {
					t.Errorf("imageSize=%q, want %q", got, tt.wantSize)
				}
			}
		})
	}
}

func TestGeminiProviderEditStreamsInlineImages(t *testing.T) {
	oldClient := geminiHTTPClient
	defer func() { geminiHTTPClient = oldClient }()

	var (
		capturedBody        []byte
		capturedContentType string
		streamedBody        bool
	)
	geminiHTTPClient = &http.Client{
		Transport: geminiImageRoundTripper(func(req *http.Request) (*http.Response, error) {
			capturedContentType = req.Header.Get("Content-Type")
			streamedBody = req.GetBody == nil
			if req.Body != nil {
				var err error
				capturedBody, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
			}
			resp := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + base64.StdEncoding.EncodeToString([]byte("edited")) + `"}}]}}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		}),
	}

	prompt := "combine \"these\"\nplease"
	result, err := NewGeminiProvider("test-key", "", "").Edit(context.Background(), EditRequest{
		Prompt: prompt,
		InputImages: []InputImage{
			{Path: "one.png", Data: []byte("img-1")},
			{Path: "two.jpg", Data: []byte("img-2")},
		},
		Size: "2K",
	})
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}

	if !streamedBody {
		t.Fatal("expected edit request with inline images to stream request body")
	}
	if capturedContentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", capturedContentType)
	}

	body := string(capturedBody)
	if !strings.Contains(body, `"mimeType":"image/png"`) {
		t.Fatalf("request body missing png mime type: %s", body)
	}
	if !strings.Contains(body, `"mimeType":"image/jpeg"`) {
		t.Fatalf("request body missing jpeg mime type: %s", body)
	}
	if !strings.Contains(body, base64.StdEncoding.EncodeToString([]byte("img-1"))) {
		t.Fatalf("request body missing first image data: %s", body)
	}
	if !strings.Contains(body, base64.StdEncoding.EncodeToString([]byte("img-2"))) {
		t.Fatalf("request body missing second image data: %s", body)
	}
	promptJSON, err := json.Marshal(prompt)
	if err != nil {
		t.Fatalf("Marshal(prompt): %v", err)
	}
	if !strings.Contains(body, `"text":`+string(promptJSON)) {
		t.Fatalf("request body missing escaped prompt: %s", body)
	}
	if !strings.Contains(body, `"imageSize":"2K"`) {
		t.Fatalf("request body missing image size: %s", body)
	}

	if string(result.Data) != "edited" {
		t.Fatalf("result data = %q, want %q", string(result.Data), "edited")
	}
	if result.MimeType != "image/png" {
		t.Fatalf("result mime type = %q, want %q", result.MimeType, "image/png")
	}
}

func TestTruncateBase64InJSON(t *testing.T) {
	longB64 := strings.Repeat("A", 5000)

	tests := []struct {
		name    string
		input   interface{}
		wantMax int // max length of output (sanity check)
	}{
		{
			"gemini style data field",
			map[string]interface{}{
				"candidates": []interface{}{
					map[string]interface{}{
						"content": map[string]interface{}{
							"parts": []interface{}{
								map[string]interface{}{
									"inlineData": map[string]interface{}{
										"mimeType": "image/png",
										"data":     longB64,
									},
								},
							},
						},
					},
				},
			},
			500,
		},
		{
			"venice style images array",
			map[string]interface{}{
				"images": []interface{}{longB64},
			},
			300,
		},
		{
			"openai style b64_json",
			map[string]interface{}{
				"data": []interface{}{
					map[string]interface{}{
						"b64_json": longB64,
					},
				},
			},
			300,
		},
		{
			"short strings preserved",
			map[string]interface{}{
				"status": "ok",
				"data":   "short",
			},
			200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.input)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}

			result := truncateBase64InJSON(raw)

			if len(result) > tt.wantMax {
				t.Errorf("output too long: got %d chars, want <= %d\noutput: %s", len(result), tt.wantMax, result[:200])
			}
			if strings.Contains(result, longB64) {
				t.Error("output contains full untruncated base64")
			}
			if !strings.Contains(result, "truncated") && len(raw) > 200 {
				t.Error("expected truncation marker in output")
			}
		})
	}
}
