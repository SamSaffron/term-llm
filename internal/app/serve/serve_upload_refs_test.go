package serve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/app/uploads"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func newUploadRequest(body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/chat/uploads", body)
	r.Header.Set(uploads.FilenameHeader, url.PathEscape("café + notes.txt"))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Authorization", "Bearer upload-test-token")
	r.Header.Set("Origin", "http://example.com")
	return r
}

func uploadTestServer() *serveServer {
	return &serveServer{cfg: serveServerConfig{basePath: "/chat", api: true, requireAuth: true, token: "upload-test-token"}}
}

func TestUploadCreateRouteAndMetadata(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := uploadTestServer()
	for _, body := range []string{"hello", ""} {
		rec := httptest.NewRecorder()
		s.httpHandler().ServeHTTP(rec, newUploadRequest(strings.NewReader(body)))
		if rec.Code != 201 {
			t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
		}
		var ref uploads.Reference
		if err := json.Unmarshal(rec.Body.Bytes(), &ref); err != nil {
			t.Fatal(err)
		}
		got, path, raw, err := uploads.ResolveReference(ref.UploadID)
		if err != nil || got.Filename != "café + notes.txt" || string(raw) != body || ref.SizeBytes != int64(len(body)) {
			t.Fatalf("ref = %#v, raw=%q, err=%v", got, raw, err)
		}
		// Existing GET/HEAD download endpoints remain authenticated attachments.
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := httptest.NewRequest(method, "/chat/uploads/"+filepath.Base(path), nil)
			r.Header.Set("Authorization", "Bearer upload-test-token")
			download := httptest.NewRecorder()
			s.httpHandler().ServeHTTP(download, r)
			if download.Code != 200 || !strings.HasPrefix(download.Header().Get("Content-Disposition"), "attachment;") {
				t.Fatalf("download %s = %d %s", method, download.Code, download.Body.String())
			}
		}
	}
}

func TestUploadCreateRejectsBadRequests(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := uploadTestServer()
	for _, tc := range []struct {
		name   string
		modify func(*http.Request)
		want   int
	}{
		{"auth", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"cross-origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, 403},
		{"same-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403},
		{"cross-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"forged-forwarded-host", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.example")
			r.Header.Set("X-Forwarded-Host", "evil.example")
		}, 403},
		{"method", func(r *http.Request) { r.Method = http.MethodGet }, 405},
		{"filename missing", func(r *http.Request) { r.Header.Del(uploads.FilenameHeader) }, 400},
		{"filename bad URI", func(r *http.Request) { r.Header.Set(uploads.FilenameHeader, "%ZZ") }, 400},
		{"filename path", func(r *http.Request) { r.Header.Set(uploads.FilenameHeader, "..%2Fsecret") }, 400},
		{"filename backslash", func(r *http.Request) { r.Header.Set(uploads.FilenameHeader, "..%5Csecret") }, 400},
		{"filename control", func(r *http.Request) { r.Header.Set(uploads.FilenameHeader, "file%00.txt") }, 400},
		{"filename long", func(r *http.Request) { r.Header.Set(uploads.FilenameHeader, strings.Repeat("a", 201)) }, 400},
		{"duplicate filename", func(r *http.Request) { r.Header.Add(uploads.FilenameHeader, "other.txt") }, 400},
		{"content encoding", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 400},
		{"media type", func(r *http.Request) { r.Header.Set("Content-Type", "not a MIME") }, 400},
		{"media type long", func(r *http.Request) { r.Header.Set("Content-Type", "text/"+strings.Repeat("a", 300)) }, 400},
		{"length too large", func(r *http.Request) { r.ContentLength = uploads.MaxAttachmentBytes + 1 }, 413},
		{"length incomplete", func(r *http.Request) { r.ContentLength = 50 }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUploadRequest(strings.NewReader("hi"))
			tc.modify(r)
			rec := httptest.NewRecorder()
			s.httpHandler().ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("status = %d want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// Content-Length is not trusted: enforce the actual streamed count too.
	r := newUploadRequest(io.LimitReader(uploadZeros{}, uploads.MaxAttachmentBytes+1))
	r.ContentLength = -1
	rec := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rec, r)
	if rec.Code != 413 {
		t.Fatalf("stream cap = %d %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"uploads", "upload-refs", "upload-staging"} {
		entries, err := os.ReadDir(filepath.Join(filepath.Dir(uploads.Dir()), name))
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed request leaked %s: %v, %v", name, entries, err)
		}
	}
}

type uploadZeros struct{}

func (uploadZeros) Read(b []byte) (int, error) { clear(b); return len(b), nil }

type blockingUploadBody struct {
	started   chan struct{}
	closed    chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (b *blockingUploadBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, context.Canceled
}
func (b *blockingUploadBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

type uploadDeadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write []time.Time
}

func (w *uploadDeadlineRecorder) SetReadDeadline(d time.Time) error {
	w.read = append(w.read, d)
	return nil
}
func (w *uploadDeadlineRecorder) SetWriteDeadline(d time.Time) error {
	w.write = append(w.write, d)
	return nil
}

func TestUploadWriterAdmissionCancelAndDeadline(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := uploadTestServer()
	var cancels []context.CancelFunc
	var results []chan *uploadDeadlineRecorder
	for range 2 {
		body := &blockingUploadBody{started: make(chan struct{}), closed: make(chan struct{})}
		r := newUploadRequest(nil)
		r.Body = body
		r.ContentLength = -1
		ctx, cancel := context.WithCancel(r.Context())
		cancels = append(cancels, cancel)
		defer cancel()
		result := make(chan *uploadDeadlineRecorder, 1)
		results = append(results, result)
		go func() {
			w := &uploadDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			s.handleUploadCreate(w, r.WithContext(ctx))
			result <- w
		}()
		select {
		case <-body.started:
		case <-time.After(5 * time.Second):
			t.Fatal("upload did not begin")
		}
	}
	r := newUploadRequest(strings.NewReader("third"))
	w := httptest.NewRecorder()
	s.handleUploadCreate(w, r)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("saturated = %d %s", w.Code, w.Body.String())
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, result := range results {
		select {
		case w := <-result:
			if w.Code != 408 || len(w.read) != 2 || len(w.write) != 2 || w.read[0].IsZero() || !w.read[1].IsZero() || !w.write[1].IsZero() {
				t.Fatalf("cancel/deadline = %#v %d", w, w.Code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("canceled upload did not finish")
		}
	}
	w = httptest.NewRecorder()
	s.handleUploadCreate(w, newUploadRequest(strings.NewReader("after cancel")))
	if w.Code != 201 {
		t.Fatalf("released admission = %d %s", w.Code, w.Body.String())
	}
}

func TestUploadRefParserMixedOrderAndReuse(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	imageRaw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(onePixelPNGDataURL, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	imageRef, err := uploads.StoreReference(context.Background(), "original.png", "image/png", bytes.NewReader(imageRaw))
	if err != nil {
		t.Fatal(err)
	}
	fileRef, err := uploads.StoreReference(context.Background(), "notes.txt", "text/plain", strings.NewReader("saved text"))
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(fmt.Sprintf(`[{"type":"input_text","text":"first"},{"type":"input_upload","upload_id":%q},{"type":"input_file","filename":"legacy.txt","file_data":"data:text/plain;base64,bGVnYWN5"},{"type":"input_upload","upload_id":%q}]`, imageRef.UploadID, fileRef.UploadID))
	var imagePath, filePath string
	for range 2 {
		msg, err := parseUserMessageContent(body)
		if err != nil {
			t.Fatal(err)
		}
		if len(msg.Parts) != 4 || msg.Parts[0].Text != "first" || msg.Parts[1].Type != core.PartImage || msg.Parts[2].Type != core.PartFile || !strings.Contains(msg.Parts[3].Text, "saved text") {
			t.Fatalf("parts = %#v", msg.Parts)
		}
		if imagePath != "" && (imagePath != msg.Parts[1].ImagePath || filePath != msg.Parts[3].FilePath) {
			t.Fatal("ref paths changed on replay")
		}
		imagePath, filePath = msg.Parts[1].ImagePath, msg.Parts[3].FilePath
	}
	entries, err := os.ReadDir(uploads.Dir())
	if err != nil || len(entries) != 4 {
		t.Fatalf("originals = %v, %v; only legacy inline should duplicate", entries, err)
	}
}

func TestUploadRefsInvalid(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := uploadTestServer()
	missing := "upl_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, tc := range []struct{ content, errorType string }{
		{`[{"type":"input_upload","upload_id":"../etc/passwd"}]`, "invalid_upload"},
		{fmt.Sprintf(`[{"type":"input_upload","upload_id":%q}]`, missing), "upload_not_found"},
		{`[{"type":"input_upload"}]`, "invalid_upload"},
		{fmt.Sprintf(`[{"type":"input_upload","upload_id":%q,"path":"/etc/passwd"}]`, missing), "invalid_upload"},
		{fmt.Sprintf(`[{"type":"input_upload","upload_id":%q,"file_data":"data:text/plain;base64,eA=="}]`, missing), "invalid_upload"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":[{"type":"message","role":"user","content":`+tc.content+`}]}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleResponses(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"type":"`+tc.errorType+`"`) {
			t.Fatalf("invalid ref = %d %s", w.Code, w.Body.String())
		}
	}
}

func TestUploadRefRejectsHugeNativeImageBeforeDecode(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(onePixelPNGDataURL, "data:image/png;base64,"))
	binary.BigEndian.PutUint32(raw[16:20], 10_000)
	binary.BigEndian.PutUint32(raw[20:24], 5_001)
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	ref, err := uploads.StoreReference(context.Background(), "huge.png", "image/png", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, err = parseUserMessageContent(json.RawMessage(fmt.Sprintf(`[{"type":"input_upload","upload_id":%q}]`, ref.UploadID)))
	if err == nil || !strings.Contains(err.Error(), "exceed 50000000 pixels") {
		t.Fatalf("huge image = %v", err)
	}
}

func TestUploadRefCapabilities(t *testing.T) {
	s := &serveServer{}
	w := httptest.NewRecorder()
	s.handleCapabilities(w, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	var payload struct {
		Attachments struct {
			MaxCount int `json:"max_count"`
			MaxBytes int `json:"max_bytes"`
			Refs     struct {
				Version       int `json:"version"`
				MaxConcurrent int `json:"max_concurrent"`
			} `json:"upload_refs"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Attachments.Refs.Version != 1 || payload.Attachments.Refs.MaxConcurrent != 2 || payload.Attachments.MaxCount != 10 || payload.Attachments.MaxBytes != uploads.MaxAttachmentBytes || !strings.Contains(w.Header().Get("ETag"), "upload-refs-1-2") {
		t.Fatalf("capabilities = %s %s", w.Header().Get("ETag"), w.Body.String())
	}
}

func TestReferenceDownloadsPreserveFilenameMIMEAndSandbox(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := uploadTestServer()
	for _, tc := range []struct{ filename, mediaType, raw string }{
		{"café + notes.txt", "text/plain", "hello"},
		{`quotes".html`, "text/html", "<script>alert(1)</script>"},
		{"diagram.svg", "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`},
	} {
		ref, err := uploads.StoreReference(context.Background(), tc.filename, tc.mediaType, strings.NewReader(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := httptest.NewRequest(method, "/chat/uploads/"+ref.UploadID, nil)
			r.Header.Set("Authorization", "Bearer upload-test-token")
			w := httptest.NewRecorder()
			s.httpHandler().ServeHTTP(w, r)
			disposition, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if w.Code != 200 || err != nil || disposition != "attachment" || params["filename"] != strings.ReplaceAll(tc.filename, `"`, "_") || w.Header().Get("Content-Type") != tc.mediaType || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox;") {
				t.Fatalf("download %s: status=%d headers=%v error=%v", method, w.Code, w.Header(), err)
			}
		}
	}
}

func TestUploadRefInvalidLocalDirectoryIsTypedAndPrivate(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	data := filepath.Dir(uploads.Dir())
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "upload-refs"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := "upl_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	// Ensure no early missing-directory check hides the corrupt metadata root.
	if err := os.Mkdir(uploads.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"input":[{"type":"message","role":"user","content":[{"type":"input_upload","upload_id":%q}]}]}`, missing)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	(&serveServer{}).handleResponses(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"type":"invalid_upload"`) || strings.Contains(w.Body.String(), data) {
		t.Fatalf("invalid directory=%d %s", w.Code, w.Body.String())
	}
	// The same layout fault is not a bad CLIENT upload: creation reports a
	// generic server error, while resolution still treats corrupt refs as400.
	create := newUploadRequest(strings.NewReader("valid client bytes"))
	created := httptest.NewRecorder()
	uploadTestServer().httpHandler().ServeHTTP(created, create)
	if created.Code != 500 || !strings.Contains(created.Body.String(), `"type":"upload_error"`) || strings.Contains(created.Body.String(), data) {
		t.Fatalf("store fault blamed client/leaked path: %d %s", created.Code, created.Body.String())
	}

}

func TestUploadAttachmentLimitsRemainPerMessageForHistory(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ref, err := uploads.StoreReference(context.Background(), "history.txt", "text/plain", strings.NewReader("history"))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []map[string]string{
		{"type": "input_upload", "upload_id": ref.UploadID},
		{"type": "input_image", "filename": "pixel.png", "image_url": onePixelPNGDataURL},
		{"type": "input_file", "filename": "history.txt", "file_data": "data:text/plain;base64,aGlzdG9yeQ=="},
	} {
		t.Run(part["type"], func(t *testing.T) {
			var parts []map[string]string
			for range 4 {
				parts = append(parts, part)
			}
			content, _ := json.Marshal(parts)
			var items []any
			var chats []chatMessage
			for range 3 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": json.RawMessage(content)})
				chats = append(chats, chatMessage{Role: "user", Content: content})
			}
			input, _ := json.Marshal(items)
			responses, replace, err := parseResponsesInput(input)
			if err != nil || !replace || len(responses) != 3 {
				t.Fatalf("12 historical attachments rejected: replace=%v messages=%d error=%v", replace, len(responses), err)
			}
			messages, replace, err := parseChatMessages(chats)
			if err != nil || !replace || len(messages) != 3 {
				t.Fatalf("chat history rejected: replace=%v messages=%d error=%v", replace, len(messages), err)
			}
			parts = nil
			for range 11 {
				parts = append(parts, part)
			}
			tooMany, _ := json.Marshal(parts)
			if _, err := parseUserMessageContent(tooMany); err == nil || !strings.Contains(err.Error(), "too many attachments") {
				t.Fatalf("per-message cap: %v", err)
			}
		})
	}
}
