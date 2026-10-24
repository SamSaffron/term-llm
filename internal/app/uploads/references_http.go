package uploads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const (
	FilenameHeader    = "X-Term-LLM-Upload-Filename"
	MaxConcurrent     = 2
	readWriteDeadline = 2 * time.Minute
)

// ReferenceHandler owns per-server writer admission. Its zero value is ready.
type ReferenceHandler struct{ writers atomic.Int32 }

// HandleCreate accepts exactly one raw original, independently of the
// response JSON. The node streams to disk; reverse Hub buffers this individual
// bounded body, not the aggregate of all attachments in a message.
func (h *ReferenceHandler) HandleCreate(w http.ResponseWriter, r *http.Request, hubURL string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !sameOriginUploadRequest(r, hubURL) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_origin", "uploads must come from the first-party origin")
		return
	}
	filename, mediaType, err := uploadRequestMetadata(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_upload", err.Error())
		return
	}
	if r.ContentLength > MaxAttachmentBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_upload", ErrUploadTooLarge.Error())
		return
	}
	if h.writers.Add(1) > MaxConcurrent {
		h.writers.Add(-1)
		w.Header().Set("Retry-After", "1")
		writeOpenAIError(w, http.StatusServiceUnavailable, "upload_busy", "too many active uploads; retry shortly")
		return
	}
	defer h.writers.Add(-1)
	ctx, cancel := context.WithTimeout(r.Context(), readWriteDeadline)
	defer cancel()
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	defer controller.SetReadDeadline(time.Time{})
	defer controller.SetWriteDeadline(time.Time{})
	body := http.MaxBytesReader(w, r.Body, MaxAttachmentBytes)
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer stop()
	defer body.Close()
	reader := io.Reader(body)
	if r.ContentLength >= 0 {
		reader = &uploadLengthReader{Reader: body, remaining: r.ContentLength}
	}
	ref, err := StoreReference(ctx, filename, mediaType, reader)
	if err != nil {
		status, errorType := uploadCreateError(err, ctx)
		message := err.Error()
		if status == http.StatusInternalServerError {
			message = "could not store upload"
		}
		writeOpenAIError(w, status, errorType, message)
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

func uploadRequestMetadata(r *http.Request) (string, string, error) {
	if len(r.Header.Values("Content-Encoding")) > 0 {
		return "", "", errors.New("content encoding is not supported")
	}
	values := r.Header.Values(FilenameHeader)
	if len(values) != 1 || len(values[0]) > 600 {
		return "", "", errors.New("one URI-encoded upload filename is required (maximum 200 decoded bytes)")
	}
	filename, err := url.PathUnescape(values[0]) // encodeURIComponent, not form encoding: '+' stays '+'.
	if err != nil {
		return "", "", errors.New("invalid URI-encoded upload filename")
	}
	mediaType := r.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return filename, mediaType, ValidateMetadata(filename, mediaType)
}

func uploadCreateError(err error, ctx context.Context) (int, string) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, ErrUploadTooLarge), errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge, "invalid_upload"
	case ctx.Err() != nil:
		return http.StatusRequestTimeout, "invalid_upload"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, ErrInvalidUpload):
		return http.StatusBadRequest, "invalid_upload"
	default:
		return http.StatusInternalServerError, "upload_error"
	}
}

// A clean EOF must still agree with Content-Length before publication. Real
// net/http bodies also report truncated connections as unexpected EOF.
type uploadLengthReader struct {
	io.Reader
	remaining int64
}

func (r *uploadLengthReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func sameOriginUploadRequest(r *http.Request, hubURL string) bool {
	site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if site == "cross-site" || site == "same-site" {
		return false
	}
	if site == "same-origin" {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	if origin == scheme+"://"+r.Host {
		return true
	}
	// The configured Hub replaces backend Host. Use its configured public
	// origin, never an arbitrary caller-supplied X-Forwarded-Host value.
	if hubURL != "" {
		u, err := url.Parse(hubURL)
		return err == nil && u.Host != "" && r.Header.Get("Origin") == u.Scheme+"://"+u.Host
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeOpenAIError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": errorType, "message": message}})
}
