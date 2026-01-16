package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
)

const geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-image:generateContent"

// GeminiProvider implements ImageProvider using Google's Gemini API
type GeminiProvider struct {
	apiKey string
}

func NewGeminiProvider(apiKey string) *GeminiProvider {
	return &GeminiProvider{apiKey: apiKey}
}

func (p *GeminiProvider) Name() string {
	return "Gemini"
}

func (p *GeminiProvider) SupportsEdit() bool {
	return true
}

func (p *GeminiProvider) SupportsMultiImage() bool {
	return true
}

func (p *GeminiProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	parts := []geminiPart{{Text: req.Prompt}}
	return p.doRequest(ctx, parts, req.Debug)
}

func (p *GeminiProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	var parts []geminiPart

	// Add all input images as parts
	for _, img := range req.InputImages {
		mimeType := getMimeType(img.Path)
		parts = append(parts, geminiPart{
			InlineData: &geminiInlineData{
				MimeType: mimeType,
				Data:     base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}

	// Add the prompt as the final part
	parts = append(parts, geminiPart{Text: req.Prompt})
	return p.doRequest(ctx, parts, req.Debug)
}

func (p *GeminiProvider) doRequest(ctx context.Context, parts []geminiPart, debug bool) (*ImageResult, error) {
	reqBody := geminiRequest{
		Contents: []geminiContent{{Parts: parts}},
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"TEXT", "IMAGE"},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", geminiEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", p.apiKey)

	client := &http.Client{}
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

	var apiResp geminiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", apiResp.Error.Message)
	}

	if len(apiResp.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	for _, part := range apiResp.Candidates[0].Content.Parts {
		if part.InlineData != nil {
			imageData, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("failed to decode image: %w", err)
			}
			return &ImageResult{
				Data:     imageData,
				MimeType: part.InlineData.MimeType,
			}, nil
		}
	}

	return nil, fmt.Errorf("no image data in response")
}

// Gemini API types
type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string `json:"responseModalities"`
}

type geminiResponse struct {
	Candidates []geminiCandidate `json:"candidates"`
	Error      *geminiError      `json:"error,omitempty"`
}

type geminiCandidate struct {
	Content geminiContentResponse `json:"content"`
}

type geminiContentResponse struct {
	Parts []geminiPartResponse `json:"parts"`
}

type geminiPartResponse struct {
	Text       string                    `json:"text,omitempty"`
	InlineData *geminiInlineDataResponse `json:"inlineData,omitempty"`
}

type geminiInlineDataResponse struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// getMimeType returns MIME type based on file extension
func getMimeType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}
