package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const openrouterImageEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// OpenRouterProvider implements ImageProvider using OpenRouter's API
type OpenRouterProvider struct {
	apiKey string
	model  string
}

func NewOpenRouterProvider(apiKey, model string) *OpenRouterProvider {
	if model == "" {
		model = "google/gemini-2.5-flash-image"
	}
	return &OpenRouterProvider{apiKey: apiKey, model: model}
}

func (p *OpenRouterProvider) Name() string {
	return "OpenRouter"
}

func (p *OpenRouterProvider) SupportsEdit() bool {
	return true
}

func (p *OpenRouterProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	content := []orMessageContent{
		{Type: "text", Text: req.Prompt},
	}
	return p.doRequest(ctx, content, req.Debug)
}

func (p *OpenRouterProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	mimeType := getMimeType(req.InputPath)
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(req.InputImage))

	content := []orMessageContent{
		{
			Type: "image_url",
			ImageURL: &orImageURL{
				URL: dataURL,
			},
		},
		{Type: "text", Text: req.Prompt},
	}
	return p.doRequest(ctx, content, req.Debug)
}

func (p *OpenRouterProvider) doRequest(ctx context.Context, content []orMessageContent, debug bool) (*ImageResult, error) {
	reqBody := orRequest{
		Model: p.model,
		Messages: []orMessage{
			{Role: "user", Content: content},
		},
		Modalities: []string{"image", "text"},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", openrouterImageEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("HTTP-Referer", "https://github.com/samsaffron/term-llm")
	httpReq.Header.Set("X-Title", "term-llm")

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	var apiResp orResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", apiResp.Error.Message)
	}

	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	// Check for images in the response
	images := apiResp.Choices[0].Message.Images
	if len(images) == 0 {
		return nil, fmt.Errorf("no images in response")
	}

	// Parse the data URL (format: data:image/png;base64,...)
	dataURL := images[0].ImageURL.URL
	if !strings.HasPrefix(dataURL, "data:") {
		return nil, fmt.Errorf("unexpected image URL format: %s", dataURL[:min(50, len(dataURL))])
	}

	// Parse data URL to extract mime type and base64 data
	// Format: data:image/png;base64,<data>
	parts := strings.SplitN(dataURL[5:], ",", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid data URL format")
	}

	metadata := parts[0] // e.g., "image/png;base64"
	b64Data := parts[1]

	// Extract mime type
	mimeType := "image/png"
	if idx := strings.Index(metadata, ";"); idx > 0 {
		mimeType = metadata[:idx]
	}

	imageData, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	return &ImageResult{
		Data:     imageData,
		MimeType: mimeType,
	}, nil
}

// OpenRouter API types
type orRequest struct {
	Model      string      `json:"model"`
	Messages   []orMessage `json:"messages"`
	Modalities []string    `json:"modalities"`
}

type orMessage struct {
	Role    string             `json:"role"`
	Content []orMessageContent `json:"content,omitempty"`
	Images  []orImage          `json:"images,omitempty"`
}

type orMessageContent struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *orImageURL `json:"image_url,omitempty"`
}

type orImageURL struct {
	URL string `json:"url"`
}

type orImage struct {
	Type     string     `json:"type"`
	ImageURL orImageURL `json:"image_url"`
}

type orResponse struct {
	Choices []orChoice `json:"choices"`
	Error   *orError   `json:"error,omitempty"`
}

type orChoice struct {
	Message orResponseMessage `json:"message"`
}

type orResponseMessage struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Images  []orImage `json:"images,omitempty"`
}

type orError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}
