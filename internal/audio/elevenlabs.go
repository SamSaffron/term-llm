package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	elevenLabsBaseURL       = "https://api.elevenlabs.io"
	elevenLabsDefaultModel  = "eleven_multilingual_v2"
	elevenLabsDefaultVoice  = "JBFqnCBsd6RMkjVDRZzb"
	elevenLabsDefaultFormat = "mp3_44100_128"
	elevenLabsHTTPTimeout   = 2 * time.Minute
)

var (
	ElevenLabsModels = []string{
		"eleven_v3",
		"eleven_multilingual_v2",
		"eleven_flash_v2_5",
		"eleven_flash_v2",
		"eleven_turbo_v2_5",
		"eleven_turbo_v2",
		"eleven_monolingual_v1",
		"eleven_multilingual_v1",
	}
	ElevenLabsFormats = []string{
		"alaw_8000",
		"mp3_22050_32", "mp3_24000_48", "mp3_44100_32", "mp3_44100_64", "mp3_44100_96", "mp3_44100_128", "mp3_44100_192",
		"opus_48000_32", "opus_48000_64", "opus_48000_96", "opus_48000_128", "opus_48000_192",
		"pcm_8000", "pcm_16000", "pcm_22050", "pcm_24000", "pcm_32000", "pcm_44100", "pcm_48000",
		"ulaw_8000",
		"wav_8000", "wav_16000", "wav_22050", "wav_24000", "wav_32000", "wav_44100", "wav_48000",
	}
	ElevenLabsVoices = []string{
		"JBFqnCBsd6RMkjVDRZzb",
		"21m00Tcm4TlvDq8ikWAM",
		"Rachel",
		"Aria",
		"Roger",
		"Sarah",
		"Laura",
		"Charlie",
		"George",
		"Callum",
		"River",
		"Liam",
		"Charlotte",
		"Alice",
		"Matilda",
		"Will",
		"Jessica",
		"Eric",
		"Chris",
		"Brian",
		"Daniel",
		"Lily",
		"Bill",
	}
	ElevenLabsTextNormalization = []string{"auto", "on", "off"}
)

type ElevenLabsProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewElevenLabsProvider(apiKey string) *ElevenLabsProvider {
	return &ElevenLabsProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: elevenLabsBaseURL,
		client:  &http.Client{Timeout: elevenLabsHTTPTimeout},
	}
}

func (p *ElevenLabsProvider) Generate(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Input) == "" {
		return nil, fmt.Errorf("input text is required")
	}
	model := firstTrimmed(req.Model, elevenLabsDefaultModel)
	voice := firstTrimmed(req.Voice, elevenLabsDefaultVoice)
	format := firstTrimmed(req.ResponseFormat, elevenLabsDefaultFormat)
	if err := ValidateElevenLabsFormat(format); err != nil {
		return nil, err
	}
	if err := ValidateElevenLabsVoiceSettings(req); err != nil {
		return nil, err
	}

	voiceID, err := p.resolveVoiceID(ctx, voice, req.Debug || req.DebugRaw)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"text":     req.Input,
		"model_id": model,
	}
	if strings.TrimSpace(req.Language) != "" {
		payload["language_code"] = strings.TrimSpace(req.Language)
	}
	if settings := elevenLabsVoiceSettings(req); len(settings) > 0 {
		payload["voice_settings"] = settings
	}
	if strings.TrimSpace(req.Seed) != "" {
		payload["seed"] = strings.TrimSpace(req.Seed)
	}
	if strings.TrimSpace(req.PreviousText) != "" {
		payload["previous_text"] = strings.TrimSpace(req.PreviousText)
	}
	if strings.TrimSpace(req.NextText) != "" {
		payload["next_text"] = strings.TrimSpace(req.NextText)
	}
	if ids := splitCSV(req.PreviousRequestIDs); len(ids) > 0 {
		payload["previous_request_ids"] = ids
	}
	if ids := splitCSV(req.NextRequestIDs); len(ids) > 0 {
		payload["next_request_ids"] = ids
	}
	if dictionaries := parsePronunciationDictionaries(req.PronunciationDictionaries); len(dictionaries) > 0 {
		payload["pronunciation_dictionary_locators"] = dictionaries
	}
	if req.UsePVCAsIVC {
		payload["use_pvc_as_ivc"] = true
	}
	if strings.TrimSpace(req.ApplyTextNormalization) != "" {
		payload["apply_text_normalization"] = strings.TrimSpace(req.ApplyTextNormalization)
	}
	if req.ApplyLanguageTextNormalization {
		payload["apply_language_text_normalization"] = true
	}

	body, contentType, err := p.doSpeech(ctx, voiceID, format, req.Streaming, req.EnableLogging, req.OptimizeStreamingLatency, payload, req.Debug || req.DebugRaw)
	if err != nil {
		return nil, err
	}
	mimeType := normalizeMime(contentType)
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = MimeTypeForFormat(format)
	}
	return &Result{Data: body, MimeType: mimeType, Format: format}, nil
}

func (p *ElevenLabsProvider) doSpeech(ctx context.Context, voiceID, format string, streaming, enableLogging bool, optimizeStreamingLatency int, payload any, debug bool) ([]byte, string, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal ElevenLabs request: %w", err)
	}
	query := url.Values{}
	query.Set("output_format", format)
	if !enableLogging {
		query.Set("enable_logging", "false")
	}
	if optimizeStreamingLatency >= 0 {
		query.Set("optimize_streaming_latency", fmt.Sprintf("%d", optimizeStreamingLatency))
	}
	path := fmt.Sprintf("/v1/text-to-speech/%s", url.PathEscape(voiceID))
	if streaming {
		path += "/stream"
	}
	endpoint := p.baseURL + path + "?" + query.Encode()
	if debug {
		debugLog("ElevenLabs Audio Request", "POST %s\n%s", endpoint, string(jsonBody))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, "", fmt.Errorf("create ElevenLabs request: %w", err)
	}
	httpReq.Header.Set("xi-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/*")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", elevenLabsRequestError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read ElevenLabs response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if debug {
		debugLog("ElevenLabs Audio Response", "status=%d content-type=%s body_len=%d", resp.StatusCode, contentType, len(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("ElevenLabs API error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, contentType, nil
}

func (p *ElevenLabsProvider) resolveVoiceID(ctx context.Context, voice string, debug bool) (string, error) {
	voice = strings.TrimSpace(voice)
	if voice == "" {
		return elevenLabsDefaultVoice, nil
	}
	if looksLikeElevenLabsVoiceID(voice) {
		return voice, nil
	}

	voices, err := p.listVoices(ctx, debug)
	if err != nil {
		return "", fmt.Errorf("resolve ElevenLabs voice %q: pass a voice_id or fix voice lookup: %w", voice, err)
	}
	for _, candidate := range voices {
		if strings.EqualFold(candidate.Name, voice) || strings.EqualFold(candidate.VoiceID, voice) {
			return candidate.VoiceID, nil
		}
	}
	return "", fmt.Errorf("ElevenLabs voice %q not found; pass a voice_id or one of your account voice names", voice)
}

type elevenLabsVoice struct {
	VoiceID string `json:"voice_id"`
	Name    string `json:"name"`
}

func (p *ElevenLabsProvider) listVoices(ctx context.Context, debug bool) ([]elevenLabsVoice, error) {
	endpoint := p.baseURL + "/v2/voices"
	if debug {
		debugLog("ElevenLabs Voices Request", "GET %s", endpoint)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create ElevenLabs voices request: %w", err)
	}
	httpReq.Header.Set("xi-api-key", p.apiKey)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, elevenLabsRequestError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read ElevenLabs voices response: %w", err)
	}
	if debug {
		debugLog("ElevenLabs Voices Response", "status=%d body_len=%d", resp.StatusCode, len(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ElevenLabs voices API error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		Voices []elevenLabsVoice `json:"voices"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode ElevenLabs voices response: %w", err)
	}
	return decoded.Voices, nil
}

func ValidateElevenLabsFormat(format string) error {
	return validateEnum("ElevenLabs output format", format, ElevenLabsFormats)
}

func ValidateElevenLabsTextNormalization(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return validateEnum("ElevenLabs text normalization", value, ElevenLabsTextNormalization)
}

func ValidateElevenLabsVoiceSettings(req Request) error {
	for name, value := range map[string]float64{
		"stability":        req.Stability,
		"similarity-boost": req.SimilarityBoost,
		"style":            req.Style,
	} {
		if value != -1 && (value < 0 || value > 1) {
			return fmt.Errorf("invalid %s %.2f (allowed: 0 to 1)", name, value)
		}
	}
	if req.Speed != 0 && (req.Speed < 0.7 || req.Speed > 1.2) {
		return fmt.Errorf("invalid ElevenLabs speed %.2f (allowed: 0.7 to 1.2)", req.Speed)
	}
	if req.OptimizeStreamingLatency < -1 || req.OptimizeStreamingLatency > 4 {
		return fmt.Errorf("invalid optimize-streaming-latency %d (allowed: 0 to 4)", req.OptimizeStreamingLatency)
	}
	if err := ValidateElevenLabsTextNormalization(req.ApplyTextNormalization); err != nil {
		return err
	}
	return nil
}

func elevenLabsVoiceSettings(req Request) map[string]any {
	settings := map[string]any{}
	if req.Stability >= 0 {
		settings["stability"] = req.Stability
	}
	if req.SimilarityBoost >= 0 {
		settings["similarity_boost"] = req.SimilarityBoost
	}
	if req.Style >= 0 {
		settings["style"] = req.Style
	}
	if req.Speed != 0 && req.Speed != 1.0 {
		settings["speed"] = req.Speed
	}
	if req.UseSpeakerBoostSet {
		settings["use_speaker_boost"] = req.UseSpeakerBoost
	}
	return settings
}

func looksLikeElevenLabsVoiceID(voice string) bool {
	if len(voice) < 16 {
		return false
	}
	for _, r := range voice {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parsePronunciationDictionaries(value string) []map[string]string {
	parts := splitCSV(value)
	out := make([]map[string]string, 0, len(parts))
	for _, part := range parts {
		id, version, ok := strings.Cut(part, ":")
		locator := map[string]string{"pronunciation_dictionary_id": strings.TrimSpace(id)}
		if ok && strings.TrimSpace(version) != "" {
			locator["version_id"] = strings.TrimSpace(version)
		}
		if locator["pronunciation_dictionary_id"] != "" {
			out = append(out, locator)
		}
	}
	return out
}

func elevenLabsRequestError(err error) error {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("ElevenLabs request timed out: %w", err)
	}
	return fmt.Errorf("ElevenLabs request failed: %w", err)
}
