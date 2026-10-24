package peerconnect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditHashesActualRequestAndStreamingResponseWithoutSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	audit, err := NewAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	requestBody := `{"input":"private-prompt-sentinel"}`
	responseBody := "data: private-response-sentinel\n\n"
	handler := auditedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != requestBody {
			t.Error("audit consumed request body")
		}
		w.Header().Set("X-Session-ID", "session-real")
		w.Header().Set("X-Response-ID", "resp-real")
		w.Header().Set("Set-Cookie", "private-cookie-sentinel")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, responseBody)
		w.(http.Flusher).Flush()
	}), audit.Record, func(string) { t.Error("audit failed") })
	req := httptest.NewRequest("POST", "http://paired.invalid/node/agent/v1/responses?secret=private-query-sentinel", strings.NewReader(requestBody))
	req.Header.Set("X-Term-LLM-Request-ID", "actual-request-1")
	req.Header.Set("Authorization", "private-token-sentinel")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !rec.Flushed || rec.Body.String() != responseBody {
		t.Fatal("audit broke streaming")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-prompt-sentinel", "private-response-sentinel", "private-cookie-sentinel", "private-query-sentinel", "private-token-sentinel"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("audit leaked %s", secret)
		}
	}
	var event AuditEvent
	if err = json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	wantReq := sha256.Sum256([]byte(requestBody))
	wantResp := sha256.Sum256([]byte(responseBody))
	if event.RequestBodySHA256 != hex.EncodeToString(wantReq[:]) || event.ResponseBodySHA256 != hex.EncodeToString(wantResp[:]) || event.RequestID != "actual-request-1" || event.ResponseID != "resp-real" || event.SessionID != "session-real" || event.WriteChunks != 1 || event.Route != "node_chat_response" {
		t.Fatalf("incorrect actual audit %+v", event)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("audit not private")
	}
	if _, err = NewAuditFile(path); err == nil {
		t.Fatal("reused/overwrote old audit")
	}
	audit.mu.Lock()
	audit.written = 8 << 20
	audit.mu.Unlock()
	if err = audit.Record(event); err == nil {
		t.Fatal("audit grew past byte cap")
	}
}
