package audio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/mediautil"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	veniceBaseURL        = "https://api.venice.ai/api/v1"
	veniceSpeechEndpoint = "/audio/speech"
	veniceHTTPTimeout    = 2 * time.Minute
)

const (
	DefaultModel  = config.DefaultAudioVeniceModel
	DefaultVoice  = config.DefaultAudioVeniceVoice
	DefaultFormat = config.DefaultAudioVeniceFormat
	DefaultSpeed  = config.DefaultAudioVeniceSpeed
)

const (
	veniceDefaultModel  = DefaultModel
	veniceDefaultVoice  = DefaultVoice
	veniceDefaultFormat = DefaultFormat
	veniceDefaultSpeed  = DefaultSpeed
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
	Input                          string
	Model                          string
	Voice                          string
	Voice1                         string
	Voice2                         string
	Speaker1                       string
	Speaker2                       string
	Language                       string
	Prompt                         string
	ResponseFormat                 string
	Speed                          float64
	Streaming                      bool
	Temperature                    *float64
	TopP                           *float64
	Stability                      float64
	SimilarityBoost                float64
	Style                          float64
	UseSpeakerBoost                bool
	UseSpeakerBoostSet             bool
	Seed                           string
	PreviousText                   string
	NextText                       string
	PreviousRequestIDs             string
	NextRequestIDs                 string
	PronunciationDictionaries      string
	UsePVCAsIVC                    bool
	ApplyTextNormalization         string
	ApplyLanguageTextNormalization bool
	OptimizeStreamingLatency       int
	EnableLogging                  bool
	Debug                          bool
	DebugRaw                       bool
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
	return providerhttp.DoJSONRequest(ctx, providerhttp.JSONRequestOptions{
		Client: p.client, Method: method, URL: p.baseURL + endpoint, APIKey: p.apiKey, Payload: payload,
		Provider: "Venice", WrapRequestError: veniceRequestError, Debug: debug, Debugf: debugLog,
		RequestLabel: "Venice Audio Request", ResponseLabel: "Venice Audio Response",
	})
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
	return mediautil.SaveFile(data, outputDir, config.DefaultAudioOutputDir, generateFilename(text, format), "audio")
}

func MimeTypeForFormat(format string) string {
	switch {
	case format == "mp3" || strings.HasPrefix(format, "mp3_"):
		return "audio/mpeg"
	case format == "opus" || strings.HasPrefix(format, "opus_"):
		return "audio/opus"
	case format == "aac":
		return "audio/aac"
	case format == "flac":
		return "audio/flac"
	case format == "wav" || strings.HasPrefix(format, "wav_"):
		return "audio/wav"
	case format == "pcm" || strings.HasPrefix(format, "pcm_"):
		return "audio/L16"
	case strings.HasPrefix(format, "ulaw_"):
		return "audio/basic"
	case strings.HasPrefix(format, "alaw_"):
		return "audio/alaw"
	default:
		return "application/octet-stream"
	}
}

func ExtensionForFormat(format string) string {
	if format == "" {
		return veniceDefaultFormat
	}
	if idx := strings.Index(format, "_"); idx > 0 {
		return format[:idx]
	}
	return format
}

func generateFilename(text, format string) string {
	timestamp := time.Now().Format("20060102-150405")
	safe := mediautil.SanitizeFilename(text)
	if len(safe) > 30 {
		safe = safe[:30]
	}
	if safe == "" {
		safe = "audio"
	}
	return fmt.Sprintf("%s-%s.%s", timestamp, safe, ExtensionForFormat(format))
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
