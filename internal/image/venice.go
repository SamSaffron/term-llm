package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"time"
)

const (
	veniceBaseURL           = "https://api.venice.ai/api/v1"
	veniceGenerateEndpoint  = veniceBaseURL + "/image/generate"
	veniceEditEndpoint      = veniceBaseURL + "/image/edit"
	veniceMultiEditEndpoint = veniceBaseURL + "/image/multi-edit"
	veniceDefaultModel      = "nano-banana-pro"
	veniceDefaultResolution = "2K"
	veniceHTTPTimeout       = 10 * time.Minute
)

var veniceHTTPClient = &http.Client{
	Timeout: veniceHTTPTimeout,
}

// VeniceProvider implements ImageProvider using Venice AI's native image API.
// - Text-to-image: POST /image/generate (JSON, returns base64 in JSON)
// - Single image edit: POST /image/edit (multipart/form-data, returns raw PNG)
// - Multi-image edit: POST /image/multi-edit (JSON with base64 images, returns raw PNG)
type VeniceProvider struct {
	apiKey     string
	model      string
	resolution string
}

func NewVeniceProvider(apiKey, model, resolution string) *VeniceProvider {
	if model == "" {
		model = veniceDefaultModel
	}
	if resolution == "" {
		resolution = veniceDefaultResolution
	}
	return &VeniceProvider{
		apiKey:     apiKey,
		model:      model,
		resolution: resolution,
	}
}

func (p *VeniceProvider) Name() string {
	return "Venice"
}

func (p *VeniceProvider) SupportsEdit() bool {
	return true
}

func (p *VeniceProvider) SupportsMultiImage() bool {
	return true
}

// Generate calls POST /image/generate — text-to-image.
// Response: JSON { "images": ["<base64>"] }
func (p *VeniceProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	genReq := veniceGenerateRequest{
		Model:         p.model,
		Prompt:        req.Prompt,
		Resolution:    p.resolution,
		SafeMode:      false,
		HideWatermark: true,
		Steps:         20,
		CFGScale:      7,
		Format:        "png",
	}

	jsonBody, err := json.Marshal(genReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", veniceGenerateEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doJSONRequest(httpReq)
}

// Edit dispatches to the appropriate endpoint:
// - 1 image  → POST /image/edit   (multipart/form-data)
// - 2-3 images → POST /image/multi-edit (JSON with base64 images array)
// Both return raw PNG binary.
func (p *VeniceProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	if len(req.InputImages) == 0 {
		return nil, fmt.Errorf("no input image provided")
	}
	if len(req.InputImages) > 3 {
		return nil, fmt.Errorf("Venice supports at most 3 input images, got %d", len(req.InputImages))
	}

	if len(req.InputImages) == 1 {
		return p.singleEdit(ctx, req)
	}
	return p.multiEdit(ctx, req)
}

// singleEdit calls POST /image/edit with multipart/form-data.
func (p *VeniceProvider) singleEdit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	img := req.InputImages[0]

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	mimeType := getMimeType(img.Path)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="%s"`, filepath.Base(img.Path)))
	h.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("failed to create form part: %w", err)
	}
	if _, err := part.Write(img.Data); err != nil {
		return nil, fmt.Errorf("failed to write image data: %w", err)
	}

	writer.WriteField("model", p.model)
	writer.WriteField("prompt", req.Prompt)

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", veniceEditEndpoint, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doRawRequest(httpReq)
}

// multiEdit calls POST /image/multi-edit with JSON body.
// Images are sent as base64-encoded strings. Response is raw PNG binary.
func (p *VeniceProvider) multiEdit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	images := make([]string, len(req.InputImages))
	for i, img := range req.InputImages {
		images[i] = base64.StdEncoding.EncodeToString(img.Data)
	}

	multiReq := veniceMultiEditRequest{
		Model:  p.model,
		Images: images,
		Prompt: req.Prompt,
	}

	jsonBody, err := json.Marshal(multiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", veniceMultiEditEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doRawRequest(httpReq)
}

// doJSONRequest handles responses with JSON { "images": ["<base64>"] }
func (p *VeniceProvider) doJSONRequest(httpReq *http.Request) (*ImageResult, error) {
	resp, err := veniceHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Venice API error (status %d): %s", resp.StatusCode, string(body))
	}

	var apiResp veniceGenerateResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if len(apiResp.Images) == 0 {
		return nil, fmt.Errorf("no image data in response")
	}

	imageData, err := base64.StdEncoding.DecodeString(apiResp.Images[0])
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}
	return &ImageResult{Data: imageData, MimeType: "image/png"}, nil
}

// doRawRequest handles responses that are raw PNG binary.
func (p *VeniceProvider) doRawRequest(httpReq *http.Request) (*ImageResult, error) {
	resp, err := veniceHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Venice API error (status %d): %s", resp.StatusCode, string(body))
	}

	return &ImageResult{Data: body, MimeType: "image/png"}, nil
}

// Venice API request/response types

type veniceGenerateRequest struct {
	Model         string `json:"model"`
	Prompt        string `json:"prompt"`
	Resolution    string `json:"resolution"`
	SafeMode      bool   `json:"safe_mode"`
	HideWatermark bool   `json:"hide_watermark"`
	Steps         int    `json:"steps"`
	CFGScale      int    `json:"cfg_scale"`
	Format        string `json:"format"`
}

type veniceMultiEditRequest struct {
	Model  string   `json:"model"`
	Images []string `json:"images"`
	Prompt string   `json:"prompt"`
}

type veniceGenerateResponse struct {
	Images []string `json:"images"`
}
