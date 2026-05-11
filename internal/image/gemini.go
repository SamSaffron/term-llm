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
	"time"
)

const (
	geminiBaseURL      = "https://generativelanguage.googleapis.com/v1beta/models/"
	geminiDefaultModel = "gemini-2.5-flash-image"
	geminiHTTPTimeout  = 3 * time.Minute
)

var geminiHTTPClient = &http.Client{
	Timeout: geminiHTTPTimeout,
}

// GeminiProvider implements ImageProvider using Google's Gemini API
type GeminiProvider struct {
	apiKey      string
	model       string
	defaultSize string
}

func NewGeminiProvider(apiKey, model, defaultSize string) *GeminiProvider {
	if model == "" {
		model = geminiDefaultModel
	}
	return &GeminiProvider{apiKey: apiKey, model: model, defaultSize: defaultSize}
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
	return p.doRequest(ctx, parts, req.Size, req.Debug || req.DebugRaw)
}

func (p *GeminiProvider) Edit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	var parts []geminiPart

	// Add all input images as parts
	for _, img := range req.InputImages {
		mimeType := getMimeType(img.Path)
		parts = append(parts, geminiPart{
			InlineData: &geminiInlineData{
				MimeType: mimeType,
				Data:     img.Data,
			},
		})
	}

	// Add the prompt as the final part
	parts = append(parts, geminiPart{Text: req.Prompt})
	return p.doRequest(ctx, parts, req.Size, req.Debug || req.DebugRaw)
}

func (p *GeminiProvider) doRequest(ctx context.Context, parts []geminiPart, size string, debug bool) (*ImageResult, error) {
	genCfg := geminiGenerationConfig{
		ResponseModalities: []string{"TEXT", "IMAGE"},
	}

	// Request size takes precedence, then config default, then omit (API defaults to 1K)
	effectiveSize := size
	if effectiveSize == "" {
		effectiveSize = p.defaultSize
	}
	if effectiveSize != "" {
		genCfg.ImageConfig = &geminiImageConfig{ImageSize: effectiveSize}
	}

	endpoint := geminiBaseURL + p.model + ":generateContent"

	var bodyReader io.Reader
	if debug || !geminiHasInlineData(parts) {
		jsonBody, err := buildGeminiRequestBody(parts, genCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to build request: %w", err)
		}
		if debug {
			debugRawImageLog(debug, "Gemini Request", "POST %s\n%s", endpoint, truncateBase64InJSON(jsonBody))
		}
		bodyReader = bytes.NewReader(jsonBody)
	} else {
		bodyReader = streamGeminiRequestBody(parts, genCfg)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", p.apiKey)

	resp, err := geminiHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if debug {
		debugRawImageLog(debug, "Gemini Response", "status=%d\n%s", resp.StatusCode, truncateBase64InJSON(body))
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

func geminiHasInlineData(parts []geminiPart) bool {
	for _, part := range parts {
		if part.InlineData != nil {
			return true
		}
	}
	return false
}

func buildGeminiRequestBody(parts []geminiPart, genCfg geminiGenerationConfig) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeGeminiRequestJSON(&buf, parts, genCfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func streamGeminiRequestBody(parts []geminiPart, genCfg geminiGenerationConfig) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeGeminiRequestJSON(pw, parts, genCfg))
	}()
	return pr
}

func writeGeminiRequestJSON(w io.Writer, parts []geminiPart, genCfg geminiGenerationConfig) error {
	if _, err := io.WriteString(w, `{"contents":[{"parts":[`); err != nil {
		return err
	}

	for i, part := range parts {
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}

		if part.InlineData != nil {
			mimeType, err := json.Marshal(part.InlineData.MimeType)
			if err != nil {
				return fmt.Errorf("marshal mime type: %w", err)
			}
			if _, err := io.WriteString(w, `{"inlineData":{"mimeType":`); err != nil {
				return err
			}
			if _, err := w.Write(mimeType); err != nil {
				return err
			}
			if _, err := io.WriteString(w, `,"data":"`); err != nil {
				return err
			}
			encoder := base64.NewEncoder(base64.StdEncoding, w)
			if _, err := encoder.Write(part.InlineData.Data); err != nil {
				encoder.Close()
				return err
			}
			if err := encoder.Close(); err != nil {
				return err
			}
			if _, err := io.WriteString(w, `"}}`); err != nil {
				return err
			}
			continue
		}

		text, err := json.Marshal(part.Text)
		if err != nil {
			return fmt.Errorf("marshal text: %w", err)
		}
		if _, err := io.WriteString(w, `{"text":`); err != nil {
			return err
		}
		if _, err := w.Write(text); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `}`); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(w, `]}],"generationConfig":`); err != nil {
		return err
	}
	cfg, err := json.Marshal(genCfg)
	if err != nil {
		return fmt.Errorf("marshal generation config: %w", err)
	}
	if _, err := w.Write(cfg); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `}`); err != nil {
		return err
	}
	return nil
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
	Data     []byte `json:"data"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string           `json:"responseModalities"`
	ImageConfig        *geminiImageConfig `json:"imageConfig,omitempty"`
}

type geminiImageConfig struct {
	ImageSize string `json:"imageSize,omitempty"`
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

// truncateBase64InJSON returns a pretty-printed JSON string with long "data" values truncated.
// This is used for debug logging to avoid dumping huge base64 blobs.
func truncateBase64InJSON(raw []byte) string {
	var obj interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return truncateDebugBody(raw, 2000)
	}
	truncateDataFields(obj)
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return truncateDebugBody(raw, 2000)
	}
	return string(out)
}

// truncateDataFields recursively walks a JSON structure and truncates any long
// string values (base64 image data, etc.) regardless of field name.
func truncateDataFields(v interface{}) {
	const maxLen = 80
	switch val := v.(type) {
	case map[string]interface{}:
		for k, child := range val {
			if s, ok := child.(string); ok && len(s) > maxLen {
				val[k] = s[:maxLen] + fmt.Sprintf("...[truncated, %d chars]", len(s))
			} else {
				truncateDataFields(child)
			}
		}
	case []interface{}:
		for i, item := range val {
			if s, ok := item.(string); ok && len(s) > maxLen {
				val[i] = s[:maxLen] + fmt.Sprintf("...[truncated, %d chars]", len(s))
			} else {
				truncateDataFields(item)
			}
		}
	}
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
