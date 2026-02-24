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
)

type TranscribeOptions struct {
	APIKey   string
	Language string // optional, e.g. "en"
	Endpoint string // full URL, e.g. "http://localhost:8080/inference" or "https://api.openai.com/v1/audio/transcriptions"
	Model    string // optional, overrides default model name sent to API
}

type whisperResponse struct {
	Text string `json:"text"`
}

// TranscribeFile sends an audio file to the OpenAI Whisper API and returns the transcript.
// Supported formats: flac, mp3, mp4, mpeg, mpga, m4a, ogg, wav, webm.
func TranscribeFile(ctx context.Context, filePath string, opts TranscribeOptions) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open audio file: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	fw, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", fmt.Errorf("write form file: %w", err)
	}

	if opts.APIKey != "" {
		model := opts.Model
		if model == "" {
			model = "whisper-1"
		}
		_ = mw.WriteField("model", model)
	}
	_ = mw.WriteField("response_format", "json")
	if opts.Language != "" {
		_ = mw.WriteField("language", opts.Language)
	}
	mw.Close()

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1/audio/transcriptions"
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		endpoint, &body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	if opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("whisper API error %d: %s", resp.StatusCode, string(b))
	}

	var result whisperResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode whisper response: %w", err)
	}

	return result.Text, nil
}
