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
	xaiImageEndpoint = "https://api.x.ai/v1/images/generations"
	xaiImageModel    = "grok-2-image-1212"
	xaiImageTimeout  = 10 * time.Minute
)

var xaiHTTPClient = &http.Client{
	Timeout: xaiImageTimeout,
}

// XAIProvider implements ImageProvider using xAI's Grok image API
type XAIProvider struct {
	apiKey string
	model  string
}

func NewXAIProvider(apiKey, model string) *XAIProvider {
	if model == "" {
		model = xaiImageModel
	}
	return &XAIProvider{
		apiKey: apiKey,
		model:  model,
	}
}

func (p *XAIProvider) Name() string {
	return "xAI"
}

func (p *XAIProvider) SupportsEdit() bool {
	return false // xAI doesn't support image editing
}

func (p *XAIProvider) SupportsMultiImage() bool {
	return false
}

func (p *XAIProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	genReq := xaiGenerateRequest{
		Model:          p.model,
		Prompt:         req.Prompt,
		N:              1,
		ResponseFormat: "b64_json",
	}

	jsonBody, err := json.Marshal(genReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", xaiImageEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := xaiHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xAI API error (status %d): %s", resp.StatusCode, string(body))
	}

	var apiResp xaiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if apiResp.Error != nil {
		return nil, fmt.Errorf("xAI API error: %s", apiResp.Error.Message)
	}

	if len(apiResp.Data) == 0 {
		return nil, fmt.Errorf("no image data in response")
	}

	// Try base64 data first
	if apiResp.Data[0].B64JSON != "" {
		imageData, err := base64.StdEncoding.DecodeString(apiResp.Data[0].B64JSON)
		if err != nil {
			return nil, fmt.Errorf("failed to decode image: %w", err)
		}
		return &ImageResult{
			Data:     imageData,
			MimeType: "image/jpeg", // xAI returns JPEG
		}, nil
	}

	// Fall back to URL
	if apiResp.Data[0].URL != "" {
		fetchReq, err := http.NewRequestWithContext(ctx, "GET", apiResp.Data[0].URL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create image URL request: %w", err)
		}
		fetchResp, err := xaiHTTPClient.Do(fetchReq)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch image URL: %w", err)
		}
		defer fetchResp.Body.Close()
		if fetchResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(fetchResp.Body)
			return nil, fmt.Errorf("image URL returned status %d: %s", fetchResp.StatusCode, string(body))
		}
		imageData, err := io.ReadAll(fetchResp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read image from URL: %w", err)
		}
		return &ImageResult{
			Data:     imageData,
			MimeType: "image/jpeg",
		}, nil
	}

	return nil, fmt.Errorf("no image data in response (neither b64_json nor url)")
}

func (p *XAIProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	return nil, fmt.Errorf("xAI does not support image editing")
}

// xAI API types
type xaiGenerateRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"` // "url" or "b64_json"
}

type xaiResponse struct {
	Data  []xaiImageData `json:"data"`
	Error *xaiError      `json:"error,omitempty"`
}

type xaiImageData struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type xaiError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}
