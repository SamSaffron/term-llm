package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	veniceBaseURL           = "https://api.venice.ai/api/v1"
	veniceGenerateEndpoint  = veniceBaseURL + "/image/generate"
	veniceDefaultModel      = "nano-banana-pro"
	veniceDefaultResolution = "2K"
	veniceHTTPTimeout       = 10 * time.Minute
)

var veniceHTTPClient = &http.Client{
	Timeout: veniceHTTPTimeout,
}

// VeniceProvider implements ImageProvider using Venice AI's native image API.
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
	return false
}

func (p *VeniceProvider) SupportsMultiImage() bool {
	return false
}

func (p *VeniceProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	genReq := veniceGenerateRequest{
		Model:          p.model,
		Prompt:         req.Prompt,
		NegativePrompt: "",
		Resolution:     p.resolution,
		SafeMode:       false,
		HideWatermark:  true,
		Steps:          20,
		CFGScale:       7,
		Format:         "png",
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

	return &ImageResult{
		Data:     imageData,
		MimeType: "image/png",
	}, nil
}

func (p *VeniceProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	return nil, fmt.Errorf("Venice does not support image editing")
}

// Venice API types
type veniceGenerateRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negative_prompt"`
	Resolution     string `json:"resolution"`
	SafeMode       bool   `json:"safe_mode"`
	HideWatermark  bool   `json:"hide_watermark"`
	Steps          int    `json:"steps"`
	CFGScale       int    `json:"cfg_scale"`
	Format         string `json:"format"`
}

type veniceGenerateResponse struct {
	Images []string `json:"images"`
}
