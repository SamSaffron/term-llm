package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/config"
)

type TranscribeOptions struct {
	APIKey     string
	Endpoint   string            // full URL, e.g. "http://localhost:8080/inference" or "https://api.mistral.ai/v1/audio/transcriptions"
	Model      string            // optional, overrides default model name sent to API
	Language   string            // optional, e.g. "en"
	Provider   string            // optional request dialect: "openai" (default), "venice", or "elevenlabs"
	Timestamps bool              // Request timestamp metadata in JSON responses where supported
	Headers    map[string]string // optional extra request headers, e.g. OpenAI-Organization
}

type whisperResponse struct {
	Text string `json:"text"`
}

const whisperErrorBodyLimit = 64 << 10

// TranscribeFile sends an audio file to a Whisper-compatible API and returns the transcript.
// Supported formats: flac, mp3, mp4, mpeg, mpga, m4a, ogg, wav, webm.
func TranscribeFile(ctx context.Context, filePath string, opts TranscribeOptions) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open audio file: %w", err)
	}
	defer f.Close()

	model := opts.Model
	if model == "" {
		switch opts.Provider {
		case "venice":
			model = config.DefaultTranscriptionVeniceModel
		case "elevenlabs":
			model = config.DefaultTranscriptionElevenLabsModel
		default:
			model = config.DefaultTranscriptionOpenAIModel
		}
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1/audio/transcriptions"
	}

	bodyReader, contentType := newTranscriptionBody(filePath, f, opts.Provider, model, opts.Language, opts.Timestamps)

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bodyReader)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	// Extra headers first so they cannot replace auth or the multipart boundary.
	for key, value := range opts.Headers {
		req.Header.Set(key, value)
	}
	if opts.APIKey != "" {
		if opts.Provider == "elevenlabs" {
			req.Header.Set("xi-api-key", opts.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+opts.APIKey)
		}
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := readLimitedBody(resp.Body, whisperErrorBodyLimit)
		return "", newHTTPStatusErrorMessageString(
			fmt.Sprintf("whisper API error %d: %s", resp.StatusCode, body),
			resp.StatusCode,
			resp.Status,
			resp.Header,
			body,
		)
	}

	if opts.Provider == "elevenlabs" && opts.Timestamps {
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return "", fmt.Errorf("decode whisper response: %w", err)
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, raw, "", "  "); err != nil {
			return "", fmt.Errorf("format whisper response: %w", err)
		}
		return pretty.String(), nil
	}

	var result whisperResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode whisper response: %w", err)
	}

	return result.Text, nil
}

func newTranscriptionBody(filePath string, file io.Reader, provider, model, language string, timestamps bool) (io.Reader, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		err := writeTranscriptionBody(mw, filePath, file, provider, model, language, timestamps)
		_ = pw.CloseWithError(err)
	}()

	return pr, mw.FormDataContentType()
}

func writeTranscriptionBody(mw *multipart.Writer, filePath string, file io.Reader, provider, model, language string, timestamps bool) (err error) {
	defer func() {
		closeErr := mw.Close()
		if err == nil {
			err = closeErr
		}
	}()

	fw, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(fw, file); err != nil {
		return fmt.Errorf("write form file: %w", err)
	}
	modelField := "model"
	languageField := "language"
	if provider == "elevenlabs" {
		modelField = "model_id"
		languageField = "language_code"
	}
	if err := mw.WriteField(modelField, model); err != nil {
		return fmt.Errorf("write model field: %w", err)
	}
	if provider != "elevenlabs" {
		if err := mw.WriteField("response_format", "json"); err != nil {
			return fmt.Errorf("write response format field: %w", err)
		}
	}
	if provider == "venice" && timestamps {
		if err := mw.WriteField("timestamps", "true"); err != nil {
			return fmt.Errorf("write timestamps field: %w", err)
		}
	}
	if provider == "elevenlabs" && timestamps {
		if err := mw.WriteField("timestamps_granularity", "word"); err != nil {
			return fmt.Errorf("write timestamps granularity field: %w", err)
		}
		if err := mw.WriteField("no_verbatim", "true"); err != nil {
			return fmt.Errorf("write no verbatim field: %w", err)
		}
		if err := mw.WriteField("tag_audio_events", "false"); err != nil {
			return fmt.Errorf("write tag audio events field: %w", err)
		}
	}
	if language != "" {
		if err := mw.WriteField(languageField, language); err != nil {
			return fmt.Errorf("write language field: %w", err)
		}
	}
	return nil
}

func readLimitedBody(r io.Reader, limit int64) string {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return fmt.Sprintf("failed to read error body: %v", err)
	}
	if int64(len(b)) > limit {
		return string(b[:limit]) + "...[truncated]"
	}
	return string(b)
}
