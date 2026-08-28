package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	maxVoiceUploadBytes    = 25 << 20 // 25 MB of audio
	maxVoiceMultipartBytes = maxVoiceUploadBytes + (1 << 20)
)

func audioExtensionForContentType(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.ToLower(contentType))
	}
	switch strings.ToLower(mediaType) {
	case "audio/webm":
		return ".webm"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/mp4", "audio/m4a", "video/mp4":
		return ".m4a"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/flac", "audio/x-flac":
		return ".flac"
	default:
		return ""
	}
}

func normalizeAudioUploadName(filename, contentType string) (string, error) {
	name := strings.TrimSpace(filepath.Base(filename))
	if name == "" || name == "." || name == "/" {
		name = "voice-note"
	}

	ext := strings.ToLower(filepath.Ext(name))
	contentExt := audioExtensionForContentType(contentType)
	if contentExt != "" {
		// The multipart Content-Type describes the bytes and therefore wins over a
		// stale client filename (notably Safari MP4 mislabeled as recording.webm).
		if ext == "" || ext != contentExt {
			name = strings.TrimSuffix(name, filepath.Ext(name)) + contentExt
			ext = contentExt
		}
	}
	if ext == "" {
		return "", fmt.Errorf("unsupported audio content type %q", contentType)
	}

	if _, err := detectAudioMimeType(name); err != nil {
		return "", err
	}
	return name, nil
}

func (s *serveServer) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceMultipartBytes)
	if err := r.ParseMultipartForm(maxVoiceUploadBytes); err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "audio upload exceeds 25 MB")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request must be multipart/form-data with an audio file")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "file is required")
		return
	}
	defer file.Close()

	filename, err := normalizeAudioUploadName(header.Filename, header.Header.Get("Content-Type"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	tempFile, err := os.CreateTemp("", "term-llm-transcribe-*"+filepath.Ext(filename))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to create temporary upload")
		return
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	written, err := io.Copy(tempFile, io.LimitReader(file, maxVoiceUploadBytes+1))
	if closeErr := tempFile.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to store uploaded audio")
		return
	}
	if written > maxVoiceUploadBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "audio upload exceeds 25 MB")
		return
	}
	if written == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "audio file is empty")
		return
	}

	if s.cfgRef != nil {
		if saveDir := s.cfgRef.Transcription.SaveDir; saveDir != "" {
			if mkErr := os.MkdirAll(saveDir, 0755); mkErr == nil {
				ts := time.Now().Format("20060102-150405")
				savePath := filepath.Join(saveDir, ts+"-"+filename)
				if src, openErr := os.Open(tempPath); openErr == nil {
					if dst, createErr := os.Create(savePath); createErr == nil {
						_, _ = io.Copy(dst, src)
						dst.Close()
					}
					src.Close()
				}
			}
		}
	}

	transcript, err := llm.TranscribeWithConfig(r.Context(), s.cfgRef, tempPath, strings.TrimSpace(r.FormValue("language")), "")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			writeOpenAIError(w, http.StatusGatewayTimeout, "timeout_error", "transcription timed out")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"text": strings.TrimSpace(transcript)})
}
