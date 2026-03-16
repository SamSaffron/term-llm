package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	veniceBaseURL           = "https://api.venice.ai/api/v1"
	veniceGenerateEndpoint  = veniceBaseURL + "/image/generate"
	veniceEditEndpoint      = veniceBaseURL + "/image/edit"
	veniceMultiEditEndpoint = veniceBaseURL + "/image/multi-edit"
	veniceDefaultModel      = "nano-banana-pro"
	veniceDefaultResolution = "2K"
	veniceHTTPTimeout       = 5 * time.Minute
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
	editMdl    string
	resolution string
}

func NewVeniceProvider(apiKey, model, editModel, resolution string) *VeniceProvider {
	apiKey = strings.TrimSpace(apiKey)
	if model == "" {
		model = veniceDefaultModel
	}
	if resolution == "" {
		resolution = veniceDefaultResolution
	}
	return &VeniceProvider{
		apiKey:     apiKey,
		model:      model,
		editMdl:    editModel,
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

// editModel returns the model ID for edit endpoints.
// Uses the explicit edit_model config if set, otherwise appends "-edit" to
// the generate model (most Venice models follow "name" → "name-edit").
func (p *VeniceProvider) editModel() string {
	if p.editMdl != "" {
		return p.editMdl
	}
	if strings.HasSuffix(p.model, "-edit") {
		return p.model
	}
	return p.model + "-edit"
}

// Generate calls POST /image/generate — text-to-image.
// Response: JSON { "images": ["<base64>"] }
func (p *VeniceProvider) Generate(ctx context.Context, req GenerateRequest) (*ImageResult, error) {
	resolution := p.resolution
	if req.Size != "" {
		resolution = req.Size
	}

	genReq := veniceGenerateRequest{
		Model:         p.model,
		Prompt:        req.Prompt,
		Resolution:    resolution,
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

	debugRaw := req.Debug || req.DebugRaw
	if debugRaw {
		debugRawImageLog(debugRaw, "Venice Request", "POST %s\n%s", veniceGenerateEndpoint, string(jsonBody))
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", veniceGenerateEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doJSONRequest(httpReq, debugRaw)
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
	debugRaw := req.Debug || req.DebugRaw

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

	writer.WriteField("modelId", p.editModel())
	writer.WriteField("prompt", req.Prompt)

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	debugRawImageLog(debugRaw, "Venice Request", "POST %s\nmultipart/form-data modelId=%s prompt=%q image=%s (%d bytes, %s)",
		veniceEditEndpoint, p.editModel(), req.Prompt, filepath.Base(img.Path), len(img.Data), mimeType)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", veniceEditEndpoint, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doRawRequest(httpReq, debugRaw)
}

// multiEdit calls POST /image/multi-edit with JSON body.
// Images are sent as base64-encoded strings. Response is raw PNG binary.
func (p *VeniceProvider) multiEdit(ctx context.Context, req EditRequest) (*ImageResult, error) {
	debugRaw := req.Debug || req.DebugRaw

	images := make([]string, len(req.InputImages))
	for i, img := range req.InputImages {
		images[i] = base64.StdEncoding.EncodeToString(img.Data)
	}

	multiReq := veniceMultiEditRequest{
		ModelID: p.editModel(),
		Images:  images,
		Prompt:  req.Prompt,
	}

	jsonBody, err := json.Marshal(multiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Log truncated body (base64 images are huge)
	debugRawImageLog(debugRaw, "Venice Request", "POST %s\nmodelId=%s prompt=%q images=%d (body %d bytes)",
		veniceMultiEditEndpoint, p.editModel(), req.Prompt, len(images), len(jsonBody))

	httpReq, err := http.NewRequestWithContext(ctx, "POST", veniceMultiEditEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	return p.doRawRequest(httpReq, debugRaw)
}

// doJSONRequest handles responses with JSON { "images": ["<base64>"] }
func (p *VeniceProvider) doJSONRequest(httpReq *http.Request, debugRaw bool) (*ImageResult, error) {
	resp, err := veniceHTTPClient.Do(httpReq)
	if err != nil {
		debugRawImageLog(debugRaw, "Venice Error", "request failed: %v", err)
		return nil, veniceRequestError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if debugRaw {
		debugRawImageLog(debugRaw, "Venice Response", "status=%d content-type=%s body_len=%d\n%s",
			resp.StatusCode, resp.Header.Get("Content-Type"), len(body), truncateBase64InJSON(body))
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
	debugRawImageLog(debugRaw, "Venice Decoded", "image_bytes=%d", len(imageData))
	return &ImageResult{Data: imageData, MimeType: "image/png"}, nil
}

// doRawRequest handles responses that are raw PNG binary.
func (p *VeniceProvider) doRawRequest(httpReq *http.Request, debugRaw bool) (*ImageResult, error) {
	resp, err := veniceHTTPClient.Do(httpReq)
	if err != nil {
		debugRawImageLog(debugRaw, "Venice Error", "request failed: %v", err)
		return nil, veniceRequestError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	debugRawImageLog(debugRaw, "Venice Response", "status=%d content-type=%s body_len=%d",
		resp.StatusCode, resp.Header.Get("Content-Type"), len(body))

	if resp.StatusCode != http.StatusOK {
		if debugRaw {
			debugRawImageLog(debugRaw, "Venice Error Body", "%s", truncateDebugBody(body, 1024))
		}
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
	ModelID string   `json:"modelId"`
	Images  []string `json:"images"`
	Prompt  string   `json:"prompt"`
}

type veniceGenerateResponse struct {
	Images []string `json:"images"`
}

// veniceRequestError returns a clean error message for HTTP client failures.
func veniceRequestError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("Venice API request timed out after %s", veniceHTTPTimeout)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("cancelled")
	}
	return fmt.Errorf("Venice API request failed: %w", err)
}

// debugRawImageLog prints a timestamped debug section to stderr.
func debugRawImageLog(enabled bool, label, format string, args ...interface{}) {
	if !enabled {
		return
	}
	ts := time.Now().Format(time.RFC3339Nano)
	body := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "\n[%s] %s\n", ts, label)
	if body != "" {
		fmt.Fprintln(os.Stderr, body)
	}
	fmt.Fprintf(os.Stderr, "[%s] END %s\n\n", ts, label)
}

// truncateDebugBody returns a string representation of body, truncated to maxLen bytes.
// For binary data (non-UTF8/non-printable), returns a summary instead.
func truncateDebugBody(body []byte, maxLen int) string {
	if len(body) == 0 {
		return "(empty)"
	}
	// Check if it looks like text (JSON, HTML, etc.)
	isText := true
	checkLen := len(body)
	if checkLen > 512 {
		checkLen = 512
	}
	for _, b := range body[:checkLen] {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			isText = false
			break
		}
	}
	if !isText {
		return fmt.Sprintf("(binary data, %d bytes)", len(body))
	}
	s := string(body)
	if len(s) > maxLen {
		return s[:maxLen] + fmt.Sprintf("...[truncated, %d total bytes]", len(body))
	}
	return s
}
