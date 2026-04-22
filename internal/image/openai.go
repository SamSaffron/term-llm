package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	openaiGenerateEndpoint = "https://api.openai.com/v1/images/generations"
	openaiEditEndpoint     = "https://api.openai.com/v1/images/edits"
	openaiDefaultModel     = "gpt-image-2"
	openaiHTTPTimeout      = 10 * time.Minute

	openaiMinPixels   = 655_360
	openaiMaxPixels   = 8_294_400
	openaiMaxEdge     = 3_840
	openaiSizeStep    = 16
	openai1KPixels    = 1_024 * 1_024
	openai2KPixels    = 2_048 * 2_048
	openaiSquare1K    = "1024x1024"
	openaiQualityAuto = "auto"
	openaiFormatPNG   = "png"
)

var openaiHTTPClient = &http.Client{
	Timeout: openaiHTTPTimeout,
}

// OpenAIProvider implements ImageProvider using OpenAI's Image API.
//
// GPT Image 2 supports both /v1/images/generations and /v1/images/edits, accepts
// multiple reference images on edits, and allows flexible resolutions as long as
// they satisfy OpenAI's documented constraints (multiples of 16, max 3840px edge,
// max 3:1 aspect ratio, pixel count between 655,360 and 8,294,400).
// Older GPT Image models keep the legacy 3-size behavior.
type OpenAIProvider struct {
	apiKey string
	model  string
}

func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	if model == "" {
		model = openaiDefaultModel
	}
	return &OpenAIProvider{apiKey: apiKey, model: model}
}

func (p *OpenAIProvider) Name() string {
	return "OpenAI"
}

func (p *OpenAIProvider) SupportsEdit() bool {
	return true
}

func (p *OpenAIProvider) SupportsMultiImage() bool {
	return true
}

func (p *OpenAIProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	genReq := openaiGenerateRequest{
		Model:        p.model,
		Prompt:       req.Prompt,
		Size:         openaiSizeFromRequest(p.model, req.Size, req.AspectRatio),
		Quality:      openaiQualityAuto,
		OutputFormat: openaiFormatPNG,
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
	if len(req.InputImages) == 0 {
		return nil, fmt.Errorf("no input image provided")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for _, inputImg := range req.InputImages {
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
	}

	fields := map[string]string{
		"model":         p.model,
		"prompt":        req.Prompt,
		"size":          openaiSizeFromRequest(p.model, req.Size, req.AspectRatio),
		"quality":       openaiQualityAuto,
		"output_format": openaiFormatPNG,
		"n":             "1",
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return nil, fmt.Errorf("failed to write form field %q: %w", key, err)
		}
	}

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

func openaiSizeFromRequest(model, size, aspectRatio string) string {
	if strings.HasPrefix(model, "gpt-image-2") {
		return openaiFlexibleSize(size, aspectRatio)
	}
	return openaiLegacySizeFromAspectRatio(aspectRatio)
}

// openaiLegacySizeFromAspectRatio maps a normalized aspect ratio to the legacy
// GPT Image size set used by earlier OpenAI image models.
func openaiLegacySizeFromAspectRatio(ar string) string {
	switch ar {
	case "16:9", "3:2", "4:3":
		return "1536x1024"
	case "9:16", "2:3", "3:4":
		return "1024x1536"
	default:
		return openaiSquare1K
	}
}

// openaiFlexibleSize maps the normalized term-llm size/aspect-ratio inputs onto
// a valid GPT Image 2 resolution. OpenAI documents these constraints for
// gpt-image-2: max edge 3840px, both edges multiples of 16, long/short ratio at
// most 3:1, and total pixels between 655,360 and 8,294,400.
func openaiFlexibleSize(size, aspectRatio string) string {
	wRatio, hRatio := openaiAspectRatioComponents(aspectRatio)
	step := lcm(openaiSizeStep/gcd(wRatio, openaiSizeStep), openaiSizeStep/gcd(hRatio, openaiSizeStep))
	if step <= 0 {
		step = openaiSizeStep
	}

	targetPixels := openaiTargetPixels(size)
	scale := int(math.Sqrt(float64(targetPixels) / float64(wRatio*hRatio)))
	scale = (scale / step) * step
	if scale < step {
		scale = step
	}

	width, height := wRatio*scale, hRatio*scale
	for scale > step && (width > openaiMaxEdge || height > openaiMaxEdge || width*height > openaiMaxPixels) {
		scale -= step
		width, height = wRatio*scale, hRatio*scale
	}
	for width*height < openaiMinPixels {
		nextScale := scale + step
		nextWidth, nextHeight := wRatio*nextScale, hRatio*nextScale
		if nextWidth > openaiMaxEdge || nextHeight > openaiMaxEdge || nextWidth*nextHeight > openaiMaxPixels {
			break
		}
		scale = nextScale
		width, height = nextWidth, nextHeight
	}

	if width <= 0 || height <= 0 {
		return openaiSquare1K
	}
	return fmt.Sprintf("%dx%d", width, height)
}

func openaiTargetPixels(size string) int {
	switch strings.ToUpper(strings.TrimSpace(size)) {
	case "4K":
		return openaiMaxPixels
	case "2K":
		return openai2KPixels
	default:
		return openai1KPixels
	}
}

func openaiAspectRatioComponents(ar string) (int, int) {
	parts := strings.SplitN(strings.TrimSpace(ar), ":", 2)
	if len(parts) != 2 {
		return 1, 1
	}
	w, err := strconv.Atoi(parts[0])
	if err != nil || w <= 0 {
		return 1, 1
	}
	h, err := strconv.Atoi(parts[1])
	if err != nil || h <= 0 {
		return 1, 1
	}
	if max(w, h) > 3*min(w, h) {
		return 1, 1
	}
	divisor := gcd(w, h)
	return w / divisor, h / divisor
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	if a == 0 {
		return 1
	}
	return a
}

func lcm(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	return a / gcd(a, b) * b
}
