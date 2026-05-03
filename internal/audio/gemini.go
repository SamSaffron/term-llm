package audio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	geminiBaseURL       = "https://generativelanguage.googleapis.com/v1beta/models"
	geminiDefaultModel  = "gemini-3.1-flash-tts-preview"
	geminiDefaultVoice  = "Kore"
	geminiDefaultFormat = "wav"
	geminiHTTPTimeout   = 2 * time.Minute
	geminiSampleRate    = 24000
	geminiChannels      = 1
	geminiSampleWidth   = 2
)

var (
	GeminiModels = []string{
		"gemini-3.1-flash-tts-preview",
		"gemini-2.5-flash-preview-tts",
		"gemini-2.5-pro-preview-tts",
	}
	GeminiFormats = []string{"wav", "pcm"}
	GeminiVoices  = []string{
		"Zephyr", "Puck", "Charon", "Kore", "Fenrir", "Leda", "Orus", "Aoede", "Callirrhoe", "Autonoe", "Enceladus", "Iapetus", "Umbriel", "Algieba", "Despina", "Erinome", "Algenib", "Rasalgethi", "Laomedeia", "Achernar", "Alnilam", "Schedar", "Gacrux", "Pulcherrima", "Achird", "Zubenelgenubi", "Vindemiatrix", "Sadachbia", "Sadaltager", "Sulafat",
	}
)

type GeminiProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewGeminiProvider(apiKey string) *GeminiProvider {
	return &GeminiProvider{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: geminiBaseURL,
		client: &http.Client{
			Timeout: geminiHTTPTimeout,
		},
	}
}

func (p *GeminiProvider) Generate(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Input) == "" {
		return nil, fmt.Errorf("input text is required")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = geminiDefaultModel
	}
	voice := strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = geminiDefaultVoice
	}
	format := strings.TrimSpace(req.ResponseFormat)
	if format == "" {
		format = geminiDefaultFormat
	}
	if err := ValidateGeminiFormat(format); err != nil {
		return nil, err
	}
	if req.Streaming {
		return nil, fmt.Errorf("Gemini TTS does not support streaming")
	}

	prompt := strings.TrimSpace(req.Input)
	if strings.TrimSpace(req.Prompt) != "" {
		prompt = strings.TrimSpace(req.Prompt) + "\n\n### TRANSCRIPT\n" + prompt
	}

	generationConfig := map[string]any{
		"responseModalities": []string{"AUDIO"},
		"speechConfig":       geminiSpeechConfig(req, voice),
	}
	if req.Temperature != nil {
		generationConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		generationConfig["topP"] = *req.TopP
	}

	payload := map[string]any{
		"contents": []map[string]any{{
			"parts": []map[string]string{{"text": prompt}},
		}},
		"generationConfig": generationConfig,
		"model":            model,
	}

	pcm, mimeType, err := p.do(ctx, model, payload, req.Debug || req.DebugRaw)
	if err != nil {
		return nil, err
	}
	if format == "wav" {
		return &Result{Data: wrapPCMAsWAV(pcm), MimeType: "audio/wav", Format: format}, nil
	}
	return &Result{Data: pcm, MimeType: normalizeMime(mimeType), Format: format}, nil
}

func geminiSpeechConfig(req Request, voice string) map[string]any {
	if strings.TrimSpace(req.Speaker1) != "" || strings.TrimSpace(req.Speaker2) != "" {
		speakers := []map[string]any{}
		if strings.TrimSpace(req.Speaker1) != "" {
			speakers = append(speakers, geminiSpeakerVoiceConfig(req.Speaker1, firstTrimmed(req.Voice1, voice)))
		}
		if strings.TrimSpace(req.Speaker2) != "" {
			speakers = append(speakers, geminiSpeakerVoiceConfig(req.Speaker2, firstTrimmed(req.Voice2, voice)))
		}
		return map[string]any{
			"multiSpeakerVoiceConfig": map[string]any{
				"speakerVoiceConfigs": speakers,
			},
		}
	}
	return map[string]any{
		"voiceConfig": map[string]any{
			"prebuiltVoiceConfig": map[string]string{"voiceName": voice},
		},
	}
}

func geminiSpeakerVoiceConfig(speaker, voice string) map[string]any {
	return map[string]any{
		"speaker": strings.TrimSpace(speaker),
		"voiceConfig": map[string]any{
			"prebuiltVoiceConfig": map[string]string{"voiceName": strings.TrimSpace(voice)},
		},
	}
}

func (p *GeminiProvider) do(ctx context.Context, model string, payload any, debug bool) ([]byte, string, error) {
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal Gemini request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/%s:generateContent", p.baseURL, model)
	if debug {
		debugLog("Gemini Audio Request", "POST %s\n%s", endpoint, string(jsonBody))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, "", fmt.Errorf("create Gemini request: %w", err)
	}
	httpReq.Header.Set("x-goog-api-key", p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", geminiRequestError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read Gemini response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if debug {
		debugLog("Gemini Audio Response", "status=%d content-type=%s body_len=%d", resp.StatusCode, contentType, len(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Gemini API error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, "", fmt.Errorf("decode Gemini response: %w", err)
	}
	for _, candidate := range decoded.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.InlineData.Data == "" {
				continue
			}
			pcm, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return nil, "", fmt.Errorf("decode Gemini audio: %w", err)
			}
			return pcm, part.InlineData.MimeType, nil
		}
	}
	return nil, "", fmt.Errorf("Gemini response did not contain audio")
}

func ValidateGeminiFormat(format string) error {
	return validateEnum("Gemini response format", format, GeminiFormats)
}

func wrapPCMAsWAV(pcm []byte) []byte {
	var buf bytes.Buffer
	dataSize := uint32(len(pcm))
	byteRate := uint32(geminiSampleRate * geminiChannels * geminiSampleWidth)
	blockAlign := uint16(geminiChannels * geminiSampleWidth)

	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36)+dataSize)
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(geminiChannels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(geminiSampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, byteRate)
	_ = binary.Write(&buf, binary.LittleEndian, blockAlign)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(geminiSampleWidth*8))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, dataSize)
	buf.Write(pcm)
	return buf.Bytes()
}

func geminiRequestError(err error) error {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("Gemini request timed out: %w", err)
	}
	return fmt.Errorf("Gemini request failed: %w", err)
}

func firstTrimmed(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
