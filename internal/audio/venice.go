package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

const (
	veniceBaseURL        = "https://api.venice.ai/api/v1"
	veniceSpeechEndpoint = "/audio/speech"
	veniceDefaultModel   = "tts-kokoro"
	veniceDefaultVoice   = "af_sky"
	veniceDefaultFormat  = "mp3"
	veniceDefaultSpeed   = 1.0
	veniceHTTPTimeout    = 2 * time.Minute
)

const (
	DefaultModel  = veniceDefaultModel
	DefaultVoice  = veniceDefaultVoice
	DefaultFormat = veniceDefaultFormat
	DefaultSpeed  = veniceDefaultSpeed
)

var (
	VeniceModels = []string{
		"tts-kokoro",
		"tts-qwen3-0-6b",
		"tts-qwen3-1-7b",
		"tts-xai-v1",
		"tts-inworld-1-5-max",
		"tts-chatterbox-hd",
		"tts-orpheus",
		"tts-elevenlabs-turbo-v2-5",
		"tts-minimax-speech-02-hd",
		"tts-gemini-3-1-flash",
	}
	VeniceFormats = []string{"mp3", "opus", "aac", "flac", "wav", "pcm"}
	VeniceVoices  = []string{
		"af_alloy", "af_aoede", "af_bella", "af_heart", "af_jadzia", "af_jessica", "af_kore", "af_nicole", "af_nova", "af_river", "af_sarah", "af_sky",
		"am_adam", "am_echo", "am_eric", "am_fenrir", "am_liam", "am_michael", "am_onyx", "am_puck", "am_santa",
		"bf_alice", "bf_emma", "bf_lily", "bm_daniel", "bm_fable", "bm_george", "bm_lewis",
		"zf_xiaobei", "zf_xiaoni", "zf_xiaoxiao", "zf_xiaoyi", "zm_yunjian", "zm_yunxi", "zm_yunxia", "zm_yunyang",
		"ff_siwis", "hf_alpha", "hf_beta", "hm_omega", "hm_psi", "if_sara", "im_nicola", "jf_alpha", "jf_gongitsune", "jf_nezumi", "jf_tebukuro", "jm_kumo", "pf_dora", "pm_alex", "pm_santa", "ef_dora", "em_alex", "em_santa",
		"Vivian", "Serena", "Ono_Anna", "Sohee", "Uncle_Fu", "Dylan", "Eric", "Ryan", "Aiden",
		"eve", "ara", "rex", "sal", "leo",
		"Craig", "Ashley", "Olivia", "Sarah", "Elizabeth", "Priya", "Alex", "Edward", "Theodore", "Ronald", "Mark", "Hades", "Luna", "Pixie",
		"Aurora", "Britney", "Siobhan", "Vicky", "Blade", "Carl", "Cliff", "Richard", "Rico",
		"tara", "leah", "jess", "mia", "zoe", "dan", "zac",
		"Rachel", "Aria", "Laura", "Charlotte", "Alice", "Matilda", "Jessica", "Lily", "Roger", "Charlie", "George", "Callum", "River", "Liam", "Will", "Chris", "Brian", "Daniel", "Bill",
		"WiseWoman", "FriendlyPerson", "InspirationalGirl", "CalmWoman", "LivelyGirl", "LovelyGirl", "SweetGirl", "ExuberantGirl", "DeepVoiceMan", "CasualGuy", "PatientMan", "YoungKnight", "DeterminedMan", "ImposingManner", "ElegantMan",
		"Achernar", "Achird", "Algenib", "Algieba", "Alnilam", "Aoede", "Autonoe", "Callirrhoe", "Charon", "Despina", "Enceladus", "Erinome", "Fenrir", "Gacrux", "Iapetus", "Kore", "Laomedeia", "Leda", "Orus", "Pulcherrima", "Puck", "Rasalgethi", "Sadachbia", "Sadaltager", "Schedar", "Sulafat", "Umbriel", "Vindemiatrix", "Zephyr", "Zubenelgenubi",
	}
)

type Request struct {
	Input          string
	Model          string
	Voice          string
	Language       string
	Prompt         string
	ResponseFormat string
	Speed          float64
	Streaming      bool
	Temperature    *float64
	TopP           *float64
	Debug          bool
	DebugRaw       bool
}

type Result struct {
	Data     []byte
	MimeType string
	Format   string
}

type VeniceProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewVeniceProvider(apiKey string) *VeniceProvider {
	return &VeniceProvider{
		apiKey:  config.NormalizeVeniceAPIKey(apiKey),
		baseURL: veniceBaseURL,
		client: &http.Client{
			Timeout: veniceHTTPTimeout,
		},
	}
}

func (p *VeniceProvider) Generate(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Input) == "" {
		return nil, fmt.Errorf("input text is required")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = veniceDefaultModel
	}
	voice := strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = veniceDefaultVoice
	}
	format := strings.TrimSpace(req.ResponseFormat)
	if format == "" {
		format = veniceDefaultFormat
	}
	if err := ValidateFormat(format); err != nil {
		return nil, err
	}
	if err := ValidateSpeed(req.Speed); err != nil {
		return nil, err
	}

	payload := map[string]any{
		"input":           req.Input,
		"model":           model,
		"voice":           voice,
		"response_format": format,
	}
	if strings.TrimSpace(req.Language) != "" {
		payload["language"] = req.Language
	}
	if strings.TrimSpace(req.Prompt) != "" {
		payload["prompt"] = req.Prompt
	}
	if req.Speed != 0 && req.Speed != veniceDefaultSpeed {
		payload["speed"] = req.Speed
	}
	if req.Streaming {
		payload["streaming"] = true
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}

	body, contentType, err := p.do(ctx, http.MethodPost, veniceSpeechEndpoint, payload, req.Debug || req.DebugRaw)
	if err != nil {
		return nil, err
	}
	mimeType := normalizeMime(contentType)
	if mimeType == "" {
		mimeType = MimeTypeForFormat(format)
	}
	return &Result{Data: body, MimeType: mimeType, Format: format}, nil
}

func (p *VeniceProvider) do(ctx context.Context, method, endpoint string, payload any, debug bool) ([]byte, string, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal Venice request: %w", err)
	}
	if debug {
		debugLog("Venice Audio Request", "%s %s\n%s", method, p.baseURL+endpoint, string(jsonBody))
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
		debugLog("Venice Audio Response", "status=%d content-type=%s body_len=%d", resp.StatusCode, contentType, len(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Venice API error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, contentType, nil
}

func ValidateFormat(format string) error {
	return validateEnum("response format", format, VeniceFormats)
}

func ValidateSpeed(speed float64) error {
	if speed == 0 {
		return nil
	}
	if speed < 0.25 || speed > 4.0 {
		return fmt.Errorf("invalid speed %.2f (allowed: 0.25 to 4.0)", speed)
	}
	return nil
}

func ValidateTemperature(temperature float64) error {
	if temperature < 0 || temperature > 2 {
		return fmt.Errorf("invalid temperature %.2f (allowed: 0 to 2)", temperature)
	}
	return nil
}

func ValidateTopP(topP float64) error {
	if topP < 0 || topP > 1 {
		return fmt.Errorf("invalid top-p %.2f (allowed: 0 to 1)", topP)
	}
	return nil
}

func Save(data []byte, outputDir, text, format string) (string, error) {
	dir := expandPath(outputDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	filename := generateFilename(text, format)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write audio: %w", err)
	}
	return path, nil
}

func MimeTypeForFormat(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/opus"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/L16"
	default:
		return "application/octet-stream"
	}
}

func ExtensionForFormat(format string) string {
	if format == "" {
		return veniceDefaultFormat
	}
	return format
}

func generateFilename(text, format string) string {
	timestamp := time.Now().Format("20060102-150405")
	safe := sanitizeForFilename(text)
	if len(safe) > 30 {
		safe = safe[:30]
	}
	if safe == "" {
		safe = "audio"
	}
	return fmt.Sprintf("%s-%s.%s", timestamp, safe, ExtensionForFormat(format))
}

func sanitizeForFilename(s string) string {
	replacer := strings.NewReplacer(" ", "_", "/", "", "\\", "", ":", "", "?", "", "*", "", "\"", "", "<", "", ">", "", "|", "")
	s = replacer.Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum || r == '-' {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if r == '_' && !lastUnderscore {
			b.WriteRune(r)
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func normalizeMime(contentType string) string {
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		return contentType[:idx]
	}
	return contentType
}

func validateEnum(name, value string, allowed []string) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("invalid %s %q (allowed: %s)", name, value, strings.Join(allowed, ", "))
}

func veniceRequestError(err error) error {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("Venice request timed out: %w", err)
	}
	return fmt.Errorf("Venice request failed: %w", err)
}

func debugLog(title, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n=== %s ===\n%s\n", title, fmt.Sprintf(format, args...))
}
