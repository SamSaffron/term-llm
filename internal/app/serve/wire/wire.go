// Package wire owns the shared HTTP JSON, SSE, compression, and steering projection helpers.
package wire

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

func WriteOpenAIError(w http.ResponseWriter, status int, errorType, message string) {
	WriteJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
		},
	})
}

func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := SteeringJSON(w, payload)
	_, _ = w.Write(append(b, '\n'))
}

func DecodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 50<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func RequireJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	if strings.TrimSpace(contentType) == "" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("invalid Content-Type header")
	}
	if mediaType != "application/json" {
		return fmt.Errorf("Content-Type must be application/json")
	}
	return nil
}

func RandomSuffix() string {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func SetSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func WriteSSEEvent(w io.Writer, event string, payload any) error {
	b, err := SteeringJSON(w, payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", SteeringEvent(w, event)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func WriteChatStreamChunk(w io.Writer, payload any) error {
	b, err := SteeringJSON(w, payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}
