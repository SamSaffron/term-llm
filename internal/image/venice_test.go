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

func TestNewVeniceProviderTrimsAPIKey(t *testing.T) {
	provider := NewVeniceProvider("  api-key\n", "", "", "")
	if provider.apiKey != "api-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "api-key")
	}
}

func TestNewVeniceProviderStripsBearerPrefix(t *testing.T) {
	provider := NewVeniceProvider("  Bearer api-key\n", "", "", "")
	if provider.apiKey != "api-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "api-key")
	}
}

func TestNewVeniceProviderDefaults(t *testing.T) {
	provider := NewVeniceProvider("api-key", "", "", "")
	if provider.model != veniceDefaultModel {
		t.Errorf("expected default model %q, got %q", veniceDefaultModel, provider.model)
	}
	if provider.resolution != veniceDefaultResolution {
		t.Errorf("expected default resolution %q, got %q", veniceDefaultResolution, provider.resolution)
	}
}

func TestNewVeniceProviderCustom(t *testing.T) {
	provider := NewVeniceProvider("key", "flux-2-max", "", "4K")
	if provider.model != "flux-2-max" {
		t.Errorf("expected model %q, got %q", "flux-2-max", provider.model)
	}
	if provider.resolution != "4K" {
		t.Errorf("expected resolution %q, got %q", "4K", provider.resolution)
	}
}

func TestVeniceEditModel(t *testing.T) {
	tests := []struct {
		model     string
		editModel string
		want      string
	}{
		{"nano-banana-pro", "", "nano-banana-pro-edit"},        // auto-suffix
		{"flux-2-max", "", "flux-2-max-edit"},                  // auto-suffix
		{"seedream-v4", "", "seedream-v4-edit"},                // auto-suffix
		{"qwen-edit", "", "qwen-edit"},                         // already has suffix
		{"nano-banana-pro-edit", "", "nano-banana-pro-edit"},   // already has suffix
		{"nano-banana-pro", "qwen-edit", "qwen-edit"},          // explicit override
		{"flux-2-max", "seedream-v4-edit", "seedream-v4-edit"}, // explicit override
	}
	for _, tt := range tests {
		p := NewVeniceProvider("key", tt.model, tt.editModel, "")
		got := p.editModel()
		if got != tt.want {
			t.Errorf("editModel(model=%q, editModel=%q) = %q, want %q", tt.model, tt.editModel, got, tt.want)
		}
	}
}

func TestVeniceRequestOptions(t *testing.T) {
	seedVeniceSizingCache(t)
	for _, mode := range []string{"generate", "single-edit", "multi-edit"} {
		for _, tc := range []struct {
			name, size, ratio, quality, wantSize, wantQuality string
		}{
			{name: "defaults", wantSize: "2K"},
			{name: "auto_quality", quality: "auto", wantSize: "2K"},
			{name: "low_landscape", size: "4K", ratio: "16:9", quality: "low", wantSize: "4K", wantQuality: "low"},
			{name: "medium_portrait", size: "1K", ratio: "9:16", quality: "medium", wantSize: "1K", wantQuality: "medium"},
			{name: "high_square", size: "2K", ratio: "1:1", quality: "high", wantSize: "2K", wantQuality: "high"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				oldClient := veniceHTTPClient
				t.Cleanup(func() { veniceHTTPClient = oldClient })
				calls := 0
				veniceHTTPClient = &http.Client{Transport: openAIImageRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer key" {
						t.Error("unexpected method or authorization")
					}
					fields := make(map[string]any)
					wantEndpoint := veniceGenerateEndpoint
					if mode == "single-edit" && tc.wantQuality == "" {
						wantEndpoint = veniceEditEndpoint
						if err := r.ParseMultipartForm(1 << 20); err != nil {
							t.Fatal(err)
						}
						defer r.MultipartForm.RemoveAll()
						for key, values := range r.MultipartForm.Value {
							fields[key] = values[0]
						}
						file, _, err := r.FormFile("image")
						if err != nil {
							t.Fatal(err)
						}
						defer file.Close()
						data, err := io.ReadAll(file)
						if err != nil || string(data) != "input-image" {
							t.Fatalf("input image = %q, err %v", data, err)
						}
					} else {
						if r.Header.Get("Content-Type") != "application/json" {
							t.Error("missing JSON content type")
						}
						if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
							t.Fatal(err)
						}
						if mode != "generate" {
							wantEndpoint = veniceMultiEditEndpoint
							wantCount := 1
							if mode == "multi-edit" {
								wantCount = 2
							}
							images, ok := fields["images"].([]any)
							if !ok || len(images) != wantCount || images[0] != base64.StdEncoding.EncodeToString([]byte("input-image")) {
								t.Fatalf("images = %v, want %d encoded inputs", fields["images"], wantCount)
							}
						}
					}
					if r.URL.String() != wantEndpoint {
						t.Errorf("endpoint = %s, want %s", r.URL, wantEndpoint)
					}
					modelKey, model := "model", "test-model"
					if mode != "generate" {
						modelKey, model = "modelId", "test-model-edit"
					}
					for key, want := range map[string]string{modelKey: model, "prompt": "test prompt", "resolution": tc.wantSize} {
						if fields[key] != want {
							t.Errorf("%s = %v, want %q", key, fields[key], want)
						}
					}
					for key, want := range map[string]string{"aspect_ratio": tc.ratio, "quality": tc.wantQuality} {
						got, present := fields[key]
						if want == "" {
							if present {
								t.Errorf("unrequested %s = %v", key, got)
							}
						} else if got != want {
							t.Errorf("%s = %v, want %q", key, got, want)
						}
					}
					for _, key := range []string{"steps", "cfg_scale"} {
						if _, present := fields[key]; present {
							t.Errorf("forced sampling parameter %s", key)
						}
					}
					body := "\x89PNG\r\n\x1a\noutput-image"
					if mode == "generate" {
						body = `{"images":["` + base64.StdEncoding.EncodeToString([]byte(body)) + `"]}`
					} else if fields["output_format"] != "png" {
						t.Errorf("output format = %v, want png", fields["output_format"])
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				p := NewVeniceProvider("key", "test-model", "", "2K")
				var result *ImageResult
				var err error
				if mode == "generate" {
					result, err = p.Generate(context.Background(), GenerateRequest{Prompt: "test prompt", Size: tc.size, AspectRatio: tc.ratio, Quality: tc.quality})
				} else {
					inputs := []InputImage{{Path: "input.png", Data: []byte("input-image")}}
					if mode == "multi-edit" {
						inputs = append(inputs, inputs[0])
					}
					result, err = p.Edit(context.Background(), EditRequest{Prompt: "test prompt", InputImages: inputs, Size: tc.size, AspectRatio: tc.ratio, Quality: tc.quality})
				}
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 || string(result.Data) != "\x89PNG\r\n\x1a\noutput-image" || result.MimeType != "image/png" {
					t.Fatalf("calls=%d result=%+v", calls, result)
				}
			})
		}
	}
}

func TestVeniceRejectsUnsupportedQuality(t *testing.T) {
	oldClient := veniceHTTPClient
	t.Cleanup(func() { veniceHTTPClient = oldClient })
	veniceHTTPClient = &http.Client{Transport: openAIImageRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Fatal("unsupported quality must fail before an API request")
		return nil, nil
	})}
	p := NewVeniceProvider("key", "test-model", "", "")
	for _, quality := range []string{"xhigh", "max", "invalid"} {
		_, err := p.Generate(context.Background(), GenerateRequest{Prompt: "test", Quality: quality})
		if err == nil || !strings.Contains(err.Error(), "quality") {
			t.Errorf("Generate(%q) error = %v", quality, err)
		}
		for _, count := range []int{1, 2} {
			_, err := p.Edit(context.Background(), EditRequest{Prompt: "test", Quality: quality, InputImages: make([]InputImage, count)})
			if err == nil || !strings.Contains(err.Error(), "quality") {
				t.Errorf("Edit(%q, %d images) error = %v", quality, count, err)
			}
		}
	}
}

func TestVeniceResponseImageFormats(t *testing.T) {
	seedVeniceSizingCache(t)
	for _, tc := range []struct {
		name, data, wantMIME string
	}{
		{"png", "\x89PNG\r\n\x1a\nimage", "image/png"},
		{"jpeg_despite_png_request", "\xff\xd8\xffimage", "image/jpeg"},
		{"webp", "RIFF\x00\x00\x00\x00WEBPVP8 image", "image/webp"},
		{"non_image", `{"error":"not an image"}`, ""},
		{"empty", "", ""},
	} {
		for _, generate := range []bool{true, false} {
			name := "edit/"
			if generate {
				name = "generate/"
			}
			t.Run(name+tc.name, func(t *testing.T) {
				oldClient := veniceHTTPClient
				t.Cleanup(func() { veniceHTTPClient = oldClient })
				veniceHTTPClient = &http.Client{Transport: openAIImageRoundTripper(func(r *http.Request) (*http.Response, error) {
					body := tc.data
					if generate {
						body = `{"images":["` + base64.StdEncoding.EncodeToString([]byte(body)) + `"]}`
					}
					// Even a misleading response header must not override the bytes.
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				p := NewVeniceProvider("key", "test-model", "", "")
				var result *ImageResult
				var err error
				if generate {
					result, err = p.Generate(context.Background(), GenerateRequest{Prompt: "test"})
				} else {
					result, err = p.Edit(context.Background(), EditRequest{Prompt: "test", InputImages: []InputImage{{Path: "input.png", Data: []byte("input")}}})
				}
				if tc.wantMIME == "" {
					if err == nil || !strings.Contains(err.Error(), "non-image") {
						t.Fatalf("error = %v, want non-image error", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.MimeType != tc.wantMIME || string(result.Data) != tc.data {
					t.Fatalf("result = %+v, want MIME %s and original data", result, tc.wantMIME)
				}
			})
		}
	}
}

func TestVeniceProviderCapabilities(t *testing.T) {
	provider := NewVeniceProvider("key", "", "", "")
	if !provider.SupportsEdit() {
		t.Error("expected SupportsEdit() = true")
	}
	if !provider.SupportsMultiImage() {
		t.Error("expected SupportsMultiImage() = true")
	}
}
