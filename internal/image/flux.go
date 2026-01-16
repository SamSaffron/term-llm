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
	fluxBaseURL              = "https://api.bfl.ai/v1"
	fluxDefaultGenerateModel = "flux-2-pro"
	fluxDefaultEditModel     = "flux-kontext-pro"
	fluxHTTPTimeout          = 10 * time.Minute
)

// fluxHTTPClient is a shared HTTP client with reasonable timeouts
var fluxHTTPClient = &http.Client{
	Timeout: fluxHTTPTimeout,
}

// FluxProvider implements ImageProvider using Black Forest Labs' Flux API
type FluxProvider struct {
	apiKey string
	model  string // model override (empty = use defaults)
}

func NewFluxProvider(apiKey, model string) *FluxProvider {
	return &FluxProvider{apiKey: apiKey, model: model}
}

func (p *FluxProvider) Name() string {
	return "Flux"
}

func (p *FluxProvider) SupportsEdit() bool {
	return true
}

func (p *FluxProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	genReq := fluxGenerateRequest{
		Prompt:      req.Prompt,
		AspectRatio: "1:1",
	}

	model := p.model
	if model == "" {
		model = fluxDefaultGenerateModel
	}
	endpoint := fluxBaseURL + "/" + model

	pollingURL, err := p.submitRequest(ctx, endpoint, genReq)
	if err != nil {
		return nil, err
	}

	return p.pollAndDownload(ctx, pollingURL)
}

func (p *FluxProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	// Create data URI for input image
	mimeType := getMimeType(req.InputPath)
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(req.InputImage))

	editReq := fluxKontextRequest{
		Prompt:       req.Prompt,
		InputImage:   dataURI,
		OutputFormat: "png",
	}

	model := p.model
	if model == "" {
		model = fluxDefaultEditModel
	}
	endpoint := fluxBaseURL + "/" + model

	pollingURL, err := p.submitRequest(ctx, endpoint, editReq)
	if err != nil {
		return nil, err
	}

	return p.pollAndDownload(ctx, pollingURL)
}

func (p *FluxProvider) submitRequest(ctx context.Context, endpoint string, reqBody any) (string, error) {
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-key", p.apiKey)

	resp, err := fluxHTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var taskResp fluxTaskResponse
	if err := json.Unmarshal(body, &taskResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	return taskResp.PollingURL, nil
}

func (p *FluxProvider) pollAndDownload(ctx context.Context, pollingURL string) (*ImageResult, error) {
	const (
		maxAttempts       = 60          // Maximum number of poll attempts
		initialBackoff    = time.Second // Start with 1 second
		maxBackoff        = 5 * time.Second
		backoffMultiplier = 1.5
	)

	backoff := initialBackoff

	for i := 0; i < maxAttempts; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		httpReq, err := http.NewRequestWithContext(ctx, "GET", pollingURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create poll request: %w", err)
		}
		httpReq.Header.Set("x-key", p.apiKey)

		resp, err := fluxHTTPClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("poll request failed: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read poll response: %w", err)
		}

		var pollResp fluxPollResponse
		if err := json.Unmarshal(body, &pollResp); err != nil {
			return nil, fmt.Errorf("failed to parse poll response: %w", err)
		}

		switch pollResp.Status {
		case "Ready":
			if pollResp.Result == nil || pollResp.Result.Sample == "" {
				return nil, fmt.Errorf("no image URL in result")
			}
			return p.downloadImage(ctx, pollResp.Result.Sample)
		case "Pending", "Processing":
			time.Sleep(backoff)
			// Exponential backoff with cap
			backoff = time.Duration(float64(backoff) * backoffMultiplier)
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		default:
			return nil, fmt.Errorf("unexpected status: %s (body: %s)", pollResp.Status, string(body))
		}
	}

	return nil, fmt.Errorf("timeout waiting for result")
}

func (p *FluxProvider) downloadImage(ctx context.Context, url string) (*ImageResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create download request: %w", err)
	}

	resp, err := fluxHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	return &ImageResult{
		Data:     imageData,
		MimeType: "image/png",
	}, nil
}

// Flux API types
type fluxGenerateRequest struct {
	Prompt      string `json:"prompt"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

type fluxKontextRequest struct {
	Prompt       string `json:"prompt"`
	InputImage   string `json:"input_image,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
}

type fluxTaskResponse struct {
	ID         string `json:"id"`
	PollingURL string `json:"polling_url"`
}

type fluxPollResponse struct {
	Status string          `json:"status"`
	Result *fluxPollResult `json:"result,omitempty"`
}

type fluxPollResult struct {
	Sample string `json:"sample"`
}
