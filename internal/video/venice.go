package video

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	veniceBaseURL                = "https://api.venice.ai/api/v1"
	veniceVideoQuoteEndpoint     = "/video/quote"
	veniceVideoQueueEndpoint     = "/video/queue"
	veniceVideoRetrieveEndpoint  = "/video/retrieve"
	veniceDefaultTextModel       = "longcat-distilled-text-to-video"
	veniceDefaultImageModel      = "longcat-distilled-image-to-video"
	veniceDefaultDuration        = "5s"
	veniceDefaultResolution      = "720p"
	veniceDefaultAspectRatio     = "16:9"
	veniceDefaultNegativePrompt  = "low resolution, error, worst quality, low quality, defects"
	veniceDefaultPollInterval    = 5 * time.Second
	veniceDefaultTimeout         = 10 * time.Minute
	veniceQuoteRequestTimeout    = 30 * time.Second
	veniceQueueRequestTimeout    = 2 * time.Minute
	veniceRetrieveRequestTimeout = 2 * time.Minute
)

const (
	DefaultDuration       = veniceDefaultDuration
	DefaultResolution     = veniceDefaultResolution
	DefaultAspectRatio    = veniceDefaultAspectRatio
	DefaultNegativePrompt = veniceDefaultNegativePrompt
	DefaultPollInterval   = veniceDefaultPollInterval
	DefaultTimeout        = veniceDefaultTimeout
)

var (
	ValidDurations   = []string{"5s", "10s"}
	ValidResolutions = []string{"480p", "720p", "1080p"}
)

type Request struct {
	Prompt         string
	Model          string
	Duration       string
	AspectRatio    string
	Resolution     string
	Audio          bool
	NegativePrompt string
	ImagePath      string
	ImageData      []byte
	Debug          bool
	DebugRaw       bool
}

type Quote struct {
	Amount float64
}

type QueuedJob struct {
	Model   string
	QueueID string
}

type Retrieval struct {
	Done                 bool
	Status               string
	AverageExecutionTime int64
	ExecutionDuration    int64
	Data                 []byte
	MimeType             string
}

type VeniceProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewVeniceProvider(apiKey string) *VeniceProvider {
	return &VeniceProvider{
		apiKey:  apiKey,
		baseURL: veniceBaseURL,
		client:  &http.Client{},
	}
}

func (p *VeniceProvider) Quote(ctx context.Context, req Request) (*Quote, error) {
	ctx, cancel := withDefaultTimeout(ctx, veniceQuoteRequestTimeout)
	defer cancel()

	payload := map[string]any{
		"model":      req.Model,
		"duration":   req.Duration,
		"resolution": req.Resolution,
	}
	if strings.TrimSpace(req.AspectRatio) != "" {
		payload["aspect_ratio"] = req.AspectRatio
	}
	if req.Audio {
		payload["audio"] = true
	}

	var respBody struct {
		Quote float64 `json:"quote"`
	}
	if err := p.doJSON(ctx, http.MethodPost, veniceVideoQuoteEndpoint, payload, req.Debug || req.DebugRaw, &respBody); err != nil {
		return nil, err
	}
	return &Quote{Amount: respBody.Quote}, nil
}

func (p *VeniceProvider) Queue(ctx context.Context, req Request) (*QueuedJob, error) {
	ctx, cancel := withDefaultTimeout(ctx, veniceQueueRequestTimeout)
	defer cancel()

	payload := map[string]any{
		"model":           req.Model,
		"prompt":          req.Prompt,
		"duration":        req.Duration,
		"negative_prompt": req.NegativePrompt,
		"resolution":      req.Resolution,
	}
	if strings.TrimSpace(req.AspectRatio) != "" {
		payload["aspect_ratio"] = req.AspectRatio
	}
	if req.Audio {
		payload["audio"] = true
	}
	if len(req.ImageData) > 0 {
		payload["image_url"] = dataURL(req.ImagePath, req.ImageData)
	}

	var respBody struct {
		Model   string `json:"model"`
		QueueID string `json:"queue_id"`
	}
	if err := p.doJSON(ctx, http.MethodPost, veniceVideoQueueEndpoint, payload, req.Debug || req.DebugRaw, &respBody); err != nil {
		return nil, err
	}
	return &QueuedJob{Model: respBody.Model, QueueID: respBody.QueueID}, nil
}

func (p *VeniceProvider) Retrieve(ctx context.Context, job QueuedJob, deleteMediaOnCompletion bool, debug bool) (*Retrieval, error) {
	ctx, cancel := withDefaultTimeout(ctx, veniceRetrieveRequestTimeout)
	defer cancel()

	payload := map[string]any{
		"model":                      job.Model,
		"queue_id":                   job.QueueID,
		"delete_media_on_completion": deleteMediaOnCompletion,
	}

	body, contentType, err := p.do(ctx, http.MethodPost, veniceVideoRetrieveEndpoint, payload, debug)
	if err != nil {
		return nil, err
	}

	trimmed := bytes.TrimSpace(body)
	if isLikelyJSON(contentType, trimmed) {
		var respBody struct {
			Status               string `json:"status"`
			AverageExecutionTime int64  `json:"average_execution_time"`
			ExecutionDuration    int64  `json:"execution_duration"`
		}
		if err := json.Unmarshal(trimmed, &respBody); err != nil {
			return nil, fmt.Errorf("parse Venice retrieve response: %w", err)
		}
		return &Retrieval{
			Done:                 false,
			Status:               respBody.Status,
			AverageExecutionTime: respBody.AverageExecutionTime,
			ExecutionDuration:    respBody.ExecutionDuration,
		}, nil
	}

	mimeType := contentType
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(body)
	}

	return &Retrieval{
		Done:     true,
		Data:     body,
		MimeType: mimeType,
	}, nil
}

func (p *VeniceProvider) doJSON(ctx context.Context, method, endpoint string, payload any, debug bool, out any) error {
	body, _, err := p.do(ctx, method, endpoint, payload, debug)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse Venice response: %w", err)
	}
	return nil
}

func (p *VeniceProvider) do(ctx context.Context, method, endpoint string, payload any, debug bool) ([]byte, string, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal Venice request: %w", err)
	}
	if debug {
		debugLog("Venice Request", "%s %s\n%s", method, p.baseURL+endpoint, string(jsonBody))
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, p.baseURL+endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, "", fmt.Errorf("create Venice request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", veniceRequestError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read Venice response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if debug {
		debugLog("Venice Response", "status=%d content-type=%s body_len=%d", resp.StatusCode, contentType, len(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Venice API error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, contentType, nil
}

func ResolveModel(model string, hasInput bool) string {
	if model != "" {
		return model
	}
	if hasInput {
		return veniceDefaultImageModel
	}
	return veniceDefaultTextModel
}

func ValidateDuration(duration string) error {
	return validateEnum("duration", duration, ValidDurations)
}

func ValidateResolution(resolution string) error {
	return validateEnum("resolution", resolution, ValidResolutions)
}

func ValidateAspectRatio(aspectRatio string) error {
	if strings.TrimSpace(aspectRatio) == "" {
		return nil
	}
	parts := strings.Split(aspectRatio, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid aspect ratio %q (expected like 16:9)", aspectRatio)
	}
	return nil
}

func LoadInputImage(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read input image %s: %w", path, err)
	}
	return data, nil
}

func SaveVideo(data []byte, outputDir, prompt string) (string, error) {
	dir := expandPath(outputDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	filename := generateFilename(prompt, ".mp4")
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write video: %w", err)
	}
	return path, nil
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func dataURL(path string, data []byte) string {
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))
}

func isLikelyJSON(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "json") {
		return true
	}
	return len(body) > 0 && body[0] == '{'
}

func validateEnum(name, value string, allowed []string) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("invalid %s %q (valid: %s)", name, value, strings.Join(allowed, ", "))
}

func veniceRequestError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("Venice API request timed out")
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("cancelled")
	}
	return fmt.Errorf("Venice API request failed: %w", err)
}

func debugLog(label, format string, args ...interface{}) {
	ts := time.Now().Format(time.RFC3339Nano)
	fmt.Fprintf(os.Stderr, "\n[%s] %s\n", ts, label)
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	fmt.Fprintf(os.Stderr, "[%s] END %s\n\n", ts, label)
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func generateFilename(prompt, ext string) string {
	timestamp := time.Now().Format("20060102-150405")
	safe := sanitizeForFilename(prompt)
	if len(safe) > 30 {
		safe = safe[:30]
	}
	if safe == "" {
		safe = "video"
	}
	return fmt.Sprintf("%s-%s%s", timestamp, safe, ext)
}

func sanitizeForFilename(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	replacer := strings.NewReplacer("/", "", "\\", "", ":", "", "?", "", "*", "", "\"", "", "<", "", ">", "", "|", "")
	s = replacer.Replace(s)
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	s = strings.Trim(s, "_")
	return strings.ToLower(s)
}
