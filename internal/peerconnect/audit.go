package peerconnect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AuditEvent contains no SDP, IP addresses, URLs, query strings, headers,
// prompt text, pairing material, cookies or credentials. IDs come from the
// actual request/response, not expected CI values. WriteChunks counts HTTP
// writes, NOT SSE events or DataChannel frames.
type AuditEvent struct {
	SchemaVersion      int       `json:"schema_version"`
	Transport          string    `json:"transport"`
	Route              string    `json:"route"`
	Method             string    `json:"method"`
	RequestID          string    `json:"request_id,omitempty"`
	SessionID          string    `json:"session_id,omitempty"`
	ResponseID         string    `json:"response_id,omitempty"`
	RequestBodySHA256  string    `json:"request_body_sha256"`
	ResponseBodySHA256 string    `json:"response_body_sha256"`
	StartedAt          time.Time `json:"started_at"`
	CompletedAt        time.Time `json:"completed_at"`
	Status             int       `json:"status"`
	ResponseBytes      int64     `json:"response_bytes"`
	WriteChunks        int       `json:"response_write_chunks"`
	Cancelled          bool      `json:"cancelled"`
}

// AuditFile is a private, bounded, new-file-only JSONL sink.
type AuditFile struct {
	mu      sync.Mutex
	f       *os.File
	written int64
}

func NewAuditFile(path string) (*AuditFile, error) {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("peer audit requires private non-symlink parent directory")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	return &AuditFile{f: f}, nil
}
func (a *AuditFile) Record(event AuditEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return errors.New("peer audit closed")
	}
	if a.written+int64(len(data)) > 8<<20 {
		return errors.New("peer audit reached 8MiB limit")
	}
	n, err := a.f.Write(data)
	a.written += int64(n)
	return err
}
func (a *AuditFile) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}

func auditID(s string) string {
	if len(s) > 128 {
		return ""
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
	}
	return s
}
func auditedHandler(next http.Handler, record func(AuditEvent) error, onError func(string)) http.Handler {
	if record == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := "hub"
		if strings.HasPrefix(r.URL.Path, "/node/") {
			route = "node"
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/responses") {
			route = "node_chat_response"
		}
		event := AuditEvent{SchemaVersion: 1, Transport: "webrtc-datachannel", Route: route, Method: r.Method, RequestID: auditID(r.Header.Get("X-Term-LLM-Request-ID")), StartedAt: time.Now().UTC()}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
		if err != nil || len(body) > 64<<10 {
			http.Error(w, "invalid bounded peer request body", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		event.RequestBodySHA256 = hex.EncodeToString(sum[:])
		r.Body = io.NopCloser(bytes.NewReader(body))
		aw := &auditResponseWriter{ResponseWriter: w, digest: sha256.New()}
		defer func() {
			p := recover()
			if p != nil && aw.status == 0 {
				aw.status = http.StatusInternalServerError
			}
			if aw.status == 0 {
				aw.status = http.StatusOK
			}
			event.Status = aw.status
			event.CompletedAt = time.Now().UTC()
			event.Cancelled = r.Context().Err() != nil
			event.ResponseBytes = aw.bytes
			event.WriteChunks = aw.chunks
			event.ResponseBodySHA256 = hex.EncodeToString(aw.digest.Sum(nil))
			event.SessionID = auditID(w.Header().Get("X-Session-ID"))
			event.ResponseID = auditID(w.Header().Get("X-Response-ID"))
			if err := record(event); err != nil {
				onError("Peer request audit unavailable or full; do not claim complete audit evidence.")
			}
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(aw, r)
	})
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
	digest hash.Hash
	bytes  int64
	chunks int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *auditResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		_, _ = w.digest.Write(b[:n])
		w.bytes += int64(n)
		w.chunks++
	}
	return n, err
}
func (w *auditResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *auditResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
