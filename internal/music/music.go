package music

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/audio"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/mediautil"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	veniceBaseURL           = "https://api.venice.ai/api/v1"
	veniceQueueEndpoint     = "/audio/queue"
	veniceRetrieveEndpoint  = "/audio/retrieve"
	veniceQuoteEndpoint     = "/audio/quote"
	veniceDefaultModel      = config.DefaultMusicVeniceModel
	veniceDefaultFormat     = config.DefaultMusicVeniceFormat
	elevenLabsBaseURL       = "https://api.elevenlabs.io"
	elevenLabsDefaultModel  = config.DefaultMusicElevenLabsModel
	elevenLabsDefaultFormat = config.DefaultMusicElevenLabsFormat
	defaultPollInterval     = 2 * time.Second
	defaultPollTimeout      = 10 * time.Minute
)

var (
	VeniceModels = []string{
		"ace-step-15",
		"elevenlabs-music",
		"minimax-music-v2",
		"minimax-music-v25",
		"minimax-music-v26",
		"stable-audio-25",
		"elevenlabs-sound-effects-v2",
		"mmaudio-v2-text-to-audio",
		"elevenlabs-tts-v3",
		"elevenlabs-tts-multilingual-v2",
	}
	VeniceFormats     = []string{"mp3", "wav", "flac"}
	VeniceVoices      = []string{"Aria", "Roger", "Sarah", "Laura", "Charlie", "George", "Callum", "River", "Liam", "Charlotte", "Alice", "Matilda", "Will", "Jessica", "Eric", "Chris", "Brian", "Daniel", "Lily", "Bill"}
	ElevenLabsModels  = []string{"music_v1"}
	ElevenLabsFormats = []string{
		"alaw_8000",
		"mp3_22050_32", "mp3_24000_48", "mp3_44100_32", "mp3_44100_64", "mp3_44100_96", "mp3_44100_128", "mp3_44100_192",
		"opus_48000_32", "opus_48000_64", "opus_48000_96", "opus_48000_128", "opus_48000_192",
		"pcm_8000", "pcm_16000", "pcm_22050", "pcm_24000", "pcm_32000", "pcm_44100", "pcm_48000",
		"ulaw_8000",
		"wav_8000", "wav_16000", "wav_22050", "wav_24000", "wav_32000", "wav_44100", "wav_48000",
	}
)

type Request struct {
	Prompt                      string
	CompositionPlan             json.RawMessage
	Model                       string
	Format                      string
	DurationSeconds             int
	Seed                        string
	ForceInstrumental           bool
	ForceInstrumentalSet        bool
	LyricsPrompt                string
	LyricsOptimizer             bool
	LyricsOptimizerSet          bool
	Voice                       string
	LanguageCode                string
	Speed                       float64
	Streaming                   bool
	Detailed                    bool
	WithTimestamps              bool
	RespectSectionsDurations    bool
	RespectSectionsDurationsSet bool
	StoreForInpainting          bool
	SignWithC2PA                bool
	DeleteMediaOnCompletion     bool
	QuoteOnly                   bool
	PollInterval                time.Duration
	PollTimeout                 time.Duration
	Debug                       bool
	DebugRaw                    bool
}

type Result struct {
	Data     []byte
	MimeType string
	Format   string
	Quote    *float64
	Metadata json.RawMessage
}

type VeniceProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewVeniceProvider(apiKey string) *VeniceProvider {
	return &VeniceProvider{apiKey: config.NormalizeVeniceAPIKey(apiKey), baseURL: veniceBaseURL, client: &http.Client{Timeout: 2 * time.Minute}}
}

func (p *VeniceProvider) Generate(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	model := firstTrimmed(req.Model, veniceDefaultModel)
	format := firstTrimmed(req.Format, VeniceDefaultFormatForModel(model))
	if err := ValidateVeniceFormat(format); err != nil {
		return nil, err
	}
	payload := map[string]any{"model": model, "prompt": strings.TrimSpace(req.Prompt)}
	if strings.TrimSpace(req.LyricsPrompt) != "" {
		payload["lyrics_prompt"] = strings.TrimSpace(req.LyricsPrompt)
	}
	if req.DurationSeconds > 0 {
		payload["duration_seconds"] = req.DurationSeconds
	}
	if req.ForceInstrumentalSet {
		payload["force_instrumental"] = req.ForceInstrumental
	}
	if req.LyricsOptimizerSet {
		payload["lyrics_optimizer"] = req.LyricsOptimizer
	}
	if strings.TrimSpace(req.Voice) != "" {
		payload["voice"] = strings.TrimSpace(req.Voice)
	}
	if strings.TrimSpace(req.LanguageCode) != "" {
		payload["language_code"] = strings.TrimSpace(req.LanguageCode)
	}
	if req.Speed != 0 {
		payload["speed"] = req.Speed
	}

	if req.QuoteOnly {
		quotePayload := map[string]any{"model": model, "character_count": len(req.Prompt)}
		if req.DurationSeconds > 0 {
			quotePayload["duration_seconds"] = req.DurationSeconds
		}
		quote, err := p.quote(ctx, quotePayload, req.Debug || req.DebugRaw)
		if err != nil {
			return nil, err
		}
		return &Result{Quote: &quote, Format: format}, nil
	}

	var queue struct {
		Model   string `json:"model"`
		QueueID string `json:"queue_id"`
		Status  string `json:"status"`
	}
	if err := p.doJSON(ctx, veniceQueueEndpoint, payload, &queue, req.Debug || req.DebugRaw); err != nil {
		return nil, err
	}
	if queue.QueueID == "" {
		return nil, fmt.Errorf("Venice response did not include queue_id")
	}

	interval := req.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	timeout := req.PollTimeout
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		result, done, err := p.retrieve(pollCtx, model, queue.QueueID, req.DeleteMediaOnCompletion, format, req.Debug || req.DebugRaw)
		if err != nil {
			return nil, err
		}
		if done {
			return result, nil
		}
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("timed out waiting for Venice audio queue %s: %w", queue.QueueID, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

func (p *VeniceProvider) quote(ctx context.Context, payload any, debug bool) (float64, error) {
	var result struct {
		Quote float64 `json:"quote"`
	}
	if err := p.doJSON(ctx, veniceQuoteEndpoint, payload, &result, debug); err != nil {
		return 0, err
	}
	return result.Quote, nil
}

func (p *VeniceProvider) retrieve(ctx context.Context, model, queueID string, deleteMedia bool, format string, debug bool) (*Result, bool, error) {
	payload := map[string]any{"model": model, "queue_id": queueID, "delete_media_on_completion": deleteMedia}
	body, contentType, err := p.do(ctx, veniceRetrieveEndpoint, payload, debug)
	if err != nil {
		return nil, false, err
	}
	mimeType := normalizeMime(contentType)
	if strings.HasPrefix(mimeType, "audio/") || isLikelyBinary(body) {
		if detected := formatFromContentType(mimeType); detected != "" {
			format = detected
		}
		if mimeType == "" || mimeType == "application/octet-stream" {
			mimeType = audio.MimeTypeForFormat(format)
		}
		return &Result{Data: body, MimeType: mimeType, Format: format}, true, nil
	}
	var status struct {
		Status      string `json:"status"`
		Audio       string `json:"audio"`
		AudioBase64 string `json:"audio_base64"`
		URL         string `json:"url"`
		Error       string `json:"error"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, false, fmt.Errorf("decode Venice retrieve response: %w", err)
	}
	if status.Error != "" || strings.EqualFold(status.Status, "FAILED") || strings.EqualFold(status.Status, "ERROR") {
		return nil, false, fmt.Errorf("Venice audio generation failed: %s%s", status.Error, status.Message)
	}
	if dataString := firstTrimmed(status.Audio, status.AudioBase64); dataString != "" {
		data, err := base64.StdEncoding.DecodeString(dataString)
		if err != nil {
			return nil, false, fmt.Errorf("decode Venice audio: %w", err)
		}
		return &Result{Data: data, MimeType: audio.MimeTypeForFormat(format), Format: format, Metadata: body}, true, nil
	}
	if strings.TrimSpace(status.URL) != "" {
		data, mediaType, err := fetchURL(ctx, status.URL)
		if err != nil {
			return nil, false, err
		}
		if mediaType == "" {
			mediaType = audio.MimeTypeForFormat(format)
		}
		return &Result{Data: data, MimeType: mediaType, Format: format, Metadata: body}, true, nil
	}
	return nil, false, nil
}

func (p *VeniceProvider) doJSON(ctx context.Context, endpoint string, payload any, out any, debug bool) error {
	body, _, err := p.do(ctx, endpoint, payload, debug)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode Venice response: %w", err)
	}
	return nil
}

func (p *VeniceProvider) do(ctx context.Context, endpoint string, payload any, debug bool) ([]byte, string, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal Venice music request: %w", err)
	}
	if debug {
		debugLog("Venice Music Request", "POST %s\n%s", p.baseURL+endpoint, string(jsonBody))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, "", fmt.Errorf("create Venice music request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", requestError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read Venice music response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if debug {
		debugLog("Venice Music Response", "status=%d content-type=%s body_len=%d", resp.StatusCode, contentType, len(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", providerhttp.NewStatusError("Venice", resp, body)
	}
	return body, contentType, nil
}

type ElevenLabsProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewElevenLabsProvider(apiKey string) *ElevenLabsProvider {
	return &ElevenLabsProvider{apiKey: strings.TrimSpace(apiKey), baseURL: elevenLabsBaseURL, client: &http.Client{Timeout: 10 * time.Minute}}
}

func (p *ElevenLabsProvider) Generate(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Prompt) == "" && len(req.CompositionPlan) == 0 {
		return nil, fmt.Errorf("prompt or composition plan is required")
	}
	model := firstTrimmed(req.Model, elevenLabsDefaultModel)
	format := firstTrimmed(req.Format, elevenLabsDefaultFormat)
	if err := ValidateElevenLabsFormat(format); err != nil {
		return nil, err
	}
	payload := map[string]any{"model_id": model}
	if strings.TrimSpace(req.Prompt) != "" {
		payload["prompt"] = strings.TrimSpace(req.Prompt)
	}
	if len(req.CompositionPlan) > 0 {
		var plan any
		if err := json.Unmarshal(req.CompositionPlan, &plan); err != nil {
			return nil, fmt.Errorf("decode composition plan: %w", err)
		}
		payload["composition_plan"] = plan
	}
	if req.DurationSeconds > 0 {
		payload["music_length_ms"] = req.DurationSeconds * 1000
	}
	if strings.TrimSpace(req.Seed) != "" {
		payload["seed"] = strings.TrimSpace(req.Seed)
	}
	if req.ForceInstrumentalSet {
		payload["force_instrumental"] = req.ForceInstrumental
	}
	if req.RespectSectionsDurationsSet {
		payload["respect_sections_durations"] = req.RespectSectionsDurations
	}
	if req.StoreForInpainting {
		payload["store_for_inpainting"] = true
	}
	if req.SignWithC2PA {
		payload["sign_with_c2pa"] = true
	}
	if req.WithTimestamps {
		payload["with_timestamps"] = true
	}

	endpoint := "/v1/music"
	if req.Detailed || req.WithTimestamps {
		endpoint = "/v1/music/detailed"
	}
	if req.Streaming {
		endpoint = "/v1/music/stream"
	}
	query := url.Values{}
	query.Set("output_format", format)
	body, contentType, err := p.do(ctx, endpoint+"?"+query.Encode(), payload, req.Debug || req.DebugRaw)
	if err != nil {
		return nil, err
	}
	mimeType := normalizeMime(contentType)
	if strings.HasPrefix(mimeType, "multipart/") {
		data, metadata, detectedFormat, err := extractMultipartAudio(body, contentType)
		if err != nil {
			return nil, err
		}
		if detectedFormat != "" {
			format = detectedFormat
		}
		return &Result{Data: data, MimeType: audio.MimeTypeForFormat(format), Format: format, Metadata: metadata}, nil
	}
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = audio.MimeTypeForFormat(format)
	}
	return &Result{Data: body, MimeType: mimeType, Format: format}, nil
}

func (p *ElevenLabsProvider) do(ctx context.Context, endpoint string, payload any, debug bool) ([]byte, string, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal ElevenLabs music request: %w", err)
	}
	if debug {
		debugLog("ElevenLabs Music Request", "POST %s\n%s", p.baseURL+endpoint, string(jsonBody))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, "", fmt.Errorf("create ElevenLabs music request: %w", err)
	}
	httpReq.Header.Set("xi-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/*")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", requestError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read ElevenLabs music response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if debug {
		debugLog("ElevenLabs Music Response", "status=%d content-type=%s body_len=%d", resp.StatusCode, contentType, len(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", providerhttp.NewStatusError("ElevenLabs", resp, body)
	}
	return body, contentType, nil
}

func Save(data []byte, outputDir, prompt, format string) (string, error) {
	return mediautil.SaveFile(data, outputDir, config.DefaultMusicOutputDir, generateFilename(prompt, format), "music")
}

func ValidateVeniceFormat(format string) error { return validateEnum("format", format, VeniceFormats) }
func ValidateElevenLabsFormat(format string) error {
	return validateEnum("format", format, ElevenLabsFormats)
}

func VeniceDefaultFormatForModel(model string) string {
	switch model {
	case "ace-step-15":
		return "flac"
	case "stable-audio-25", "mmaudio-v2-text-to-audio":
		return "wav"
	default:
		return "mp3"
	}
}

func ExtensionForFormat(format string) string { return audio.ExtensionForFormat(format) }

func validateEnum(label, value string, allowed []string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("invalid %s %q (allowed: %s)", label, value, strings.Join(allowed, ", "))
}

func extractMultipartAudio(body []byte, contentType string) ([]byte, json.RawMessage, string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, nil, "", fmt.Errorf("parse multipart content type: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, "", fmt.Errorf("expected multipart response, got %s", contentType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, nil, "", fmt.Errorf("multipart response missing boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var metadata json.RawMessage
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, "", fmt.Errorf("read multipart response: %w", err)
		}
		partData, err := io.ReadAll(part)
		if err != nil {
			return nil, nil, "", fmt.Errorf("read multipart part: %w", err)
		}
		partType := normalizeMime(part.Header.Get("Content-Type"))
		if strings.HasPrefix(partType, "audio/") || isLikelyBinary(partData) {
			format := formatFromContentType(partType)
			return partData, metadata, format, nil
		}
		if json.Valid(partData) {
			metadata = append(metadata[:0], partData...)
		}
	}
	return nil, metadata, "", fmt.Errorf("multipart response did not include audio data")
}

func fetchURL(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create audio download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", requestError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read audio download response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("audio download error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, normalizeMime(resp.Header.Get("Content-Type")), nil
}

func formatFromContentType(contentType string) string {
	switch normalizeMime(contentType) {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "audio/opus":
		return "opus"
	case "audio/aac":
		return "aac"
	default:
		return ""
	}
}

func isLikelyBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	limit := len(data)
	if limit > 256 {
		limit = 256
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	trimmed := strings.TrimSpace(string(data[:limit]))
	return !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")
}

func normalizeMime(contentType string) string {
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		return strings.TrimSpace(contentType[:idx])
	}
	return strings.TrimSpace(contentType)
}

func firstTrimmed(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func requestError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("request failed: %w", err)
}

func generateFilename(prompt, format string) string {
	timestamp := time.Now().Format("20060102-150405")
	safe := mediautil.SanitizeFilename(prompt)
	if len(safe) > 30 {
		safe = safe[:30]
	}
	if safe == "" {
		safe = "music"
	}
	return fmt.Sprintf("%s-%s.%s", timestamp, safe, ExtensionForFormat(format))
}

func debugLog(title, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n=== %s ===\n%s\n", title, fmt.Sprintf(format, args...))
}
