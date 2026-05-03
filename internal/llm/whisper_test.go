package llm

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscribeFile_StreamsMultipartBody(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	audioFile, err := os.CreateTemp(t.TempDir(), "audio-*.wav")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if _, err := audioFile.WriteString("audio-bytes"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}

	var (
		contentLength int64
		contentType   string
		bodyBytes     []byte
	)
	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			contentLength = req.ContentLength
			contentType = req.Header.Get("Content-Type")
			bodyBytes, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"text":"ok"}`)),
			}, nil
		}),
	}

	text, err := TranscribeFile(context.Background(), audioFile.Name(), TranscribeOptions{
		Endpoint: "https://transcribe.example.test/v1/audio/transcriptions",
		Language: "en",
	})
	if err != nil {
		t.Fatalf("TranscribeFile failed: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q, want %q", text, "ok")
	}
	if contentLength > 0 {
		t.Fatalf("ContentLength = %d, want streamed request body", contentLength)
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType failed: %v", err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}

	reader := multipart.NewReader(bytes.NewReader(bodyBytes), params["boundary"])
	fields := map[string]string{}
	fileName := ""
	fileBody := ""
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart failed: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("ReadAll part failed: %v", err)
		}
		if part.FormName() == "file" {
			fileName = part.FileName()
			fileBody = string(data)
			continue
		}
		fields[part.FormName()] = string(data)
	}

	if fileName != filepath.Base(audioFile.Name()) {
		t.Fatalf("file name = %q, want %q", fileName, filepath.Base(audioFile.Name()))
	}
	if fileBody != "audio-bytes" {
		t.Fatalf("file body = %q, want %q", fileBody, "audio-bytes")
	}
	if fields["model"] != "whisper-1" {
		t.Fatalf("model = %q, want %q", fields["model"], "whisper-1")
	}
	if fields["response_format"] != "json" {
		t.Fatalf("response_format = %q, want %q", fields["response_format"], "json")
	}
	if fields["language"] != "en" {
		t.Fatalf("language = %q, want %q", fields["language"], "en")
	}
}

func TestTranscribeFile_ElevenLabsDialect(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	audioFile, err := os.CreateTemp(t.TempDir(), "audio-*.mp3")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if _, err := audioFile.WriteString("audio-bytes"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}

	var contentType string
	var bodyBytes []byte
	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("xi-api-key"); got != "test-key" {
				t.Fatalf("xi-api-key = %q, want test-key", got)
			}
			if got := req.Header.Get("Authorization"); got != "" {
				t.Fatalf("Authorization = %q, want empty", got)
			}
			contentType = req.Header.Get("Content-Type")
			bodyBytes, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"text":"hello"}`)),
			}, nil
		}),
	}

	text, err := TranscribeFile(context.Background(), audioFile.Name(), TranscribeOptions{
		APIKey:   "test-key",
		Endpoint: "https://api.elevenlabs.io/v1/speech-to-text",
		Model:    "scribe_v2",
		Language: "en",
		Provider: "elevenlabs",
	})
	if err != nil {
		t.Fatalf("TranscribeFile failed: %v", err)
	}
	if text != "hello" {
		t.Fatalf("text = %q, want hello", text)
	}

	fields := readMultipartFields(t, contentType, bodyBytes)
	if fields["model_id"] != "scribe_v2" {
		t.Fatalf("model_id = %q, want scribe_v2", fields["model_id"])
	}
	if fields["language_code"] != "en" {
		t.Fatalf("language_code = %q, want en", fields["language_code"])
	}
	if _, ok := fields["response_format"]; ok {
		t.Fatalf("response_format should not be sent to ElevenLabs")
	}
}

func TestTranscribeFile_VeniceDialect(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	audioFile, err := os.CreateTemp(t.TempDir(), "audio-*.mp3")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if _, err := audioFile.WriteString("audio-bytes"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}

	var contentType string
	var bodyBytes []byte
	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Fatalf("Authorization = %q, want Bearer test-key", got)
			}
			contentType = req.Header.Get("Content-Type")
			bodyBytes, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"text":"hello"}`)),
			}, nil
		}),
	}

	_, err = TranscribeFile(context.Background(), audioFile.Name(), TranscribeOptions{
		APIKey:     "test-key",
		Endpoint:   "https://api.venice.ai/api/v1/audio/transcriptions",
		Model:      "nvidia/parakeet-tdt-0.6b-v3",
		Provider:   "venice",
		Timestamps: true,
	})
	if err != nil {
		t.Fatalf("TranscribeFile failed: %v", err)
	}

	fields := readMultipartFields(t, contentType, bodyBytes)
	if fields["model"] != "nvidia/parakeet-tdt-0.6b-v3" {
		t.Fatalf("model = %q, want nvidia/parakeet-tdt-0.6b-v3", fields["model"])
	}
	if fields["response_format"] != "json" {
		t.Fatalf("response_format = %q, want json", fields["response_format"])
	}
	if fields["timestamps"] != "true" {
		t.Fatalf("timestamps = %q, want true", fields["timestamps"])
	}
}

func readMultipartFields(t *testing.T, contentType string, bodyBytes []byte) map[string]string {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType failed: %v", err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	reader := multipart.NewReader(bytes.NewReader(bodyBytes), params["boundary"])
	fields := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart failed: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("ReadAll part failed: %v", err)
		}
		if part.FormName() != "file" {
			fields[part.FormName()] = string(data)
		}
	}
	return fields
}

func TestTranscribeFile_LimitsErrorBody(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	audioFile, err := os.CreateTemp(t.TempDir(), "audio-*.wav")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	if _, err := audioFile.WriteString("audio-bytes"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := audioFile.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}

	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", whisperErrorBodyLimit+128))),
			}, nil
		}),
	}

	_, err = TranscribeFile(context.Background(), audioFile.Name(), TranscribeOptions{
		Endpoint: "https://transcribe.example.test/v1/audio/transcriptions",
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "...[truncated]") {
		t.Fatalf("error = %q, want truncated marker", err)
	}
	if len(err.Error()) > whisperErrorBodyLimit+128 {
		t.Fatalf("error length = %d, want capped size", len(err.Error()))
	}
}
