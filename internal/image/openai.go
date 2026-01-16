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
	openaiGenerateEndpoint = "https://api.openai.com/v1/images/generations"
	openaiEditEndpoint     = "https://api.openai.com/v1/images/edits"
	openaiModel            = "gpt-image-1"
	openaiHTTPTimeout      = 10 * time.Minute
)

var openaiHTTPClient = &http.Client{
	Timeout: openaiHTTPTimeout,
}

// OpenAIProvider implements ImageProvider using OpenAI's API
type OpenAIProvider struct {
	apiKey string
}

func NewOpenAIProvider(apiKey string) *OpenAIProvider {
	return &OpenAIProvider{apiKey: apiKey}
}

func (p *OpenAIProvider) Name() string {
	return "OpenAI"
}

func (p *OpenAIProvider) SupportsEdit() bool {
	return true
}

func (p *OpenAIProvider) SupportsMultiImage() bool {
	return false
}

func (p *OpenAIProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	genReq := openaiGenerateRequest{
		Model:        openaiModel,
		Prompt:       req.Prompt,
		Size:         "1024x1024",
		Quality:      "auto",
		OutputFormat: "png",
		N:            1,
	}

	jsonBody, err := json.Marshal(genReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", openaiGenerateEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doRequest(httpReq)
}

func (p *OpenAIProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	// OpenAI only supports single image editing
	if len(req.InputImages) == 0 {
		return nil, fmt.Errorf("no input image provided")
	}
	if len(req.InputImages) > 1 {
		return nil, fmt.Errorf("OpenAI only supports single image editing, got %d images", len(req.InputImages))
	}

	inputImg := req.InputImages[0]

	// Build multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add image file with proper mime type
	mimeType := getMimeType(inputImg.Path)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="%s"`, filepath.Base(inputImg.Path)))
	h.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := part.Write(inputImg.Data); err != nil {
		return nil, fmt.Errorf("failed to write image data: %w", err)
	}

	// Add other fields
	writer.WriteField("model", openaiModel)
	writer.WriteField("prompt", req.Prompt)
	writer.WriteField("size", "1024x1024")
	writer.WriteField("quality", "auto")
	writer.WriteField("output_format", "png")
	writer.WriteField("n", "1")

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", openaiEditEndpoint, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doRequest(httpReq)
}

func (p *OpenAIProvider) doRequest(httpReq *http.Request) (*ImageResult, error) {
	resp, err := openaiHTTPClient.Do(httpReq)
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

	var apiResp openaiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", apiResp.Error.Message)
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
			MimeType: "image/png",
		}, nil
	}

	// Fall back to URL
	if apiResp.Data[0].URL != "" {
		fetchReq, err := http.NewRequestWithContext(httpReq.Context(), "GET", apiResp.Data[0].URL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create image URL request: %w", err)
		}
		resp, err := openaiHTTPClient.Do(fetchReq)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch image URL: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("image URL returned status %d: %s", resp.StatusCode, string(body))
		}
		imageData, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read image from URL: %w", err)
		}
		return &ImageResult{
			Data:     imageData,
			MimeType: "image/png",
		}, nil
	}

	return nil, fmt.Errorf("no image data in response (neither b64_json nor url)")
}

// OpenAI API types
type openaiGenerateRequest struct {
	Model        string `json:"model"`
	Prompt       string `json:"prompt"`
	Size         string `json:"size"`
	Quality      string `json:"quality"`
	OutputFormat string `json:"output_format"`
	N            int    `json:"n"`
}

type openaiResponse struct {
	Data  []openaiImageData `json:"data"`
	Error *openaiError      `json:"error,omitempty"`
}

type openaiImageData struct {
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
}

type openaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}
