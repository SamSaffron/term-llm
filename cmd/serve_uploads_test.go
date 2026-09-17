package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestServeServerConfigUploadsRoute(t *testing.T) {
	tests := []struct {
		basePath    string
		wantUI      string
		wantUploads string
	}{
		{"/ui", "/ui/", "/ui/uploads/"},
		{"/chat", "/chat/", "/chat/uploads/"},
		{"/app/v2", "/app/v2/", "/app/v2/uploads/"},
	}
	for _, tt := range tests {
		cfg := serveServerConfig{basePath: tt.basePath}
		if got := cfg.uiRoute(); got != tt.wantUI {
			t.Errorf("basePath=%q uiRoute()=%q, want %q", tt.basePath, got, tt.wantUI)
		}
		if got := cfg.uploadsRoute(); got != tt.wantUploads {
			t.Errorf("basePath=%q uploadsRoute()=%q, want %q", tt.basePath, got, tt.wantUploads)
		}
	}
}

// writeUpload creates a file in the isolated uploads directory and returns its
// stored basename.
func writeUpload(t *testing.T, name, body string) string {
	t.Helper()
	dir := serveUploadsDir()
	if dir == "" {
		t.Fatal("serveUploadsDir() is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestHandleUpload(t *testing.T) {
	const body = "private file body"

	tests := []struct {
		name   string
		method string
		setup  func(t *testing.T) string // returns request path (unescaped)
		// rawTarget bypasses escaping so a percent-encoded separator survives
		// into URL.Path and the traversal defence is what actually answers.
		rawTarget       string
		wantStatus      int
		wantBody        string
		wantDisposition string
		wantAllow       string
	}{
		{
			name:   "valid file",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				return "/uploads/" + writeUpload(t, "notes_a1b2.txt", body)
			},
			wantStatus:      http.StatusOK,
			wantBody:        body,
			wantDisposition: `attachment; filename="notes_a1b2.txt"`,
		},
		{
			name:   "head request",
			method: http.MethodHead,
			setup: func(t *testing.T) string {
				return "/uploads/" + writeUpload(t, "notes_a1b2.txt", body)
			},
			wantStatus:      http.StatusOK,
			wantDisposition: `attachment; filename="notes_a1b2.txt"`,
		},
		{
			name:   "empty name",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				return "/uploads/"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			// The encoded separator decodes to /uploads/../secret.txt, and a real
			// file sits exactly there, so only the traversal check can reject it.
			name:      "encoded traversal reaches an existing sibling",
			method:    http.MethodGet,
			rawTarget: "/uploads/..%2Fsecret.txt",
			setup: func(t *testing.T) string {
				writeUpload(t, "notes_a1b2.txt", body)
				sibling := filepath.Join(filepath.Dir(serveUploadsDir()), "secret.txt")
				if err := os.WriteFile(sibling, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "parent traversal",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				return "/uploads/" + writeUpload(t, "notes_a1b2.txt", body) + "/../../secret.txt"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "nested subdirectory",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				dir := serveUploadsDir()
				if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "nested", "deep.txt"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				return "/uploads/nested/deep.txt"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "missing file",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				return "/uploads/gone_a1b2.txt"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "symlink",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				target := writeUpload(t, "notes_a1b2.txt", body)
				dir := serveUploadsDir()
				if err := os.Symlink(filepath.Join(dir, target), filepath.Join(dir, "link_a1b2.txt")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return "/uploads/link_a1b2.txt"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:   "post rejected",
			method: http.MethodPost,
			setup: func(t *testing.T) string {
				return "/uploads/" + writeUpload(t, "notes_a1b2.txt", body)
			},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET, HEAD",
		},
		{
			name:   "non-ascii name",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				return "/uploads/" + writeUpload(t, "café notes_a1b2.txt", body)
			},
			wantStatus:      http.StatusOK,
			wantBody:        body,
			wantDisposition: `attachment; filename="café notes_a1b2.txt"; filename*=UTF-8''caf%C3%A9%20notes_a1b2.txt`,
		},
		{
			name:   "quote and newline sanitized",
			method: http.MethodGet,
			setup: func(t *testing.T) string {
				return "/uploads/" + writeUpload(t, "we\"ird\n_a1b2.txt", body)
			},
			wantStatus:      http.StatusOK,
			wantDisposition: `attachment; filename="we_ird__a1b2.txt"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			reqPath := tt.setup(t)
			// Keep the request target encoded so a literal newline or space in a
			// stored name still travels as a legal URL.
			target := (&url.URL{Path: reqPath}).EscapedPath()
			if tt.rawTarget != "" {
				target = tt.rawTarget
			}
			req := httptest.NewRequest(tt.method, target, nil)
			rr := httptest.NewRecorder()
			srv := &serveServer{}
			srv.handleUpload(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if tt.wantAllow != "" && rr.Header().Get("Allow") != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", rr.Header().Get("Allow"), tt.wantAllow)
			}
			if tt.wantDisposition != "" {
				if got := rr.Header().Get("Content-Disposition"); got != tt.wantDisposition {
					t.Errorf("Content-Disposition = %q, want %q", got, tt.wantDisposition)
				}
			}
			if tt.wantBody != "" && rr.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rr.Body.String(), tt.wantBody)
			}
			if tt.method == http.MethodHead && rr.Body.Len() != 0 {
				t.Errorf("HEAD body = %q, want empty", rr.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				if strings.Contains(rr.Body.String(), body) {
					t.Errorf("rejected request leaked file body: %q", rr.Body.String())
				}
				return
			}
			if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := rr.Header().Get("Cache-Control"); got != "private, max-age=86400" {
				t.Errorf("Cache-Control = %q, want private, max-age=86400", got)
			}
			if vary := rr.Header().Values("Vary"); !strings.Contains(strings.Join(vary, ","), "Authorization") || !strings.Contains(strings.Join(vary, ","), "Cookie") {
				t.Errorf("Vary = %v, want Authorization and Cookie", vary)
			}
			if csp := rr.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
				t.Errorf("Content-Security-Policy = %q, want a sandbox directive", csp)
			}
			if strings.ContainsAny(rr.Header().Get("Content-Disposition"), "\r\n") {
				t.Errorf("Content-Disposition contains a line break: %q", rr.Header().Get("Content-Disposition"))
			}
		})
	}
}

// TestHandleUploadServesThroughHubProxy guards the Hub path: /uploads/ rides the
// generic node proxy, so a hub-mounted download must still be an attachment.
func TestHandleUploadServesThroughHubProxy(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	name := writeUpload(t, "notes_a1b2.txt", "hub body")

	srv := &serveServer{}
	backend := http.StripPrefix("/chat", http.HandlerFunc(srv.handleUpload))
	hub := hubWithBackend(t, "/chat", backend.ServeHTTP)

	rec := httptest.NewRecorder()
	hub.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/node/alpha/uploads/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="`+name+`"` {
		t.Errorf("Content-Disposition = %q, want attachment for %q", got, name)
	}
	if rec.Body.String() != "hub body" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "hub body")
	}
}

// TestUploadRouteRequiresAuth pins the credential gate on the mounted route.
// An anchor download cannot set Authorization, so the GET cookie fallback is
// the only way a browser reaches this route.
func TestUploadRouteRequiresAuth(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	name := writeUpload(t, "notes_a1b2.txt", "guarded body")

	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/chat", requireAuth: true, token: "secret"}}
	handler := srv.httpHandler()
	target := "/chat/uploads/" + name

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "guarded body") {
		t.Error("unauthorized response leaked the file body")
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "guarded body" {
		t.Fatalf("cookie download status = %d body = %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "wrong"})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie status = %d, want 401", rec.Code)
	}
}

// TestUploadFileURLEscapesAndRoundTrips guards the two halves agreeing: stored
// names come from client filenames, so a space or "#" must survive minting and
// be understood again by the handler.
func TestUploadFileURLEscapesAndRoundTrips(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const body = "awkward name body"
	name := writeUpload(t, "my notes #1 100%_a1b2.txt", body)

	srv := &serveServer{}
	got := srv.uploadFileURL(filepath.Join(serveUploadsDir(), name))
	want := "/uploads/my%20notes%20%231%20100%25_a1b2.txt"
	if got != want {
		t.Fatalf("uploadFileURL = %q, want %q", got, want)
	}

	rec := httptest.NewRecorder()
	srv.handleUpload(rec, httptest.NewRequest(http.MethodGet, got, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("round trip status = %d body = %q", rec.Code, rec.Body.String())
	}
}

// TestUploadRouteMountedUnderBasePath drives the real handler so the
// registration beside /images/ and /media/ is covered, not just the handler.
func TestUploadRouteMountedUnderBasePath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	name := writeUpload(t, "notes_a1b2.txt", "mounted body")

	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/chat"}}
	rec := httptest.NewRecorder()
	srv.httpHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat/uploads/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="`+name+`"` {
		t.Errorf("Content-Disposition = %q, want attachment for %q", got, name)
	}
	if rec.Body.String() != "mounted body" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "mounted body")
	}

	// The route must not answer on the un-prefixed path.
	rec = httptest.NewRecorder()
	srv.httpHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/uploads/"+name, nil))
	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("un-prefixed /uploads/ status = %d, want 307 redirect to the UI root", rec.Code)
	}
}

func TestSessionMessageFilePartDownloadURL(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	uploadsDir := serveUploadsDir()
	if err := os.MkdirAll(filepath.Join(uploadsDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(uploadsDir, "notes_a1b2.txt")
	if err := os.WriteFile(inside, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(uploadsDir, "nested", "deep.txt")
	if err := os.WriteFile(nested, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name     string
		filePath string
		wantURL  string
	}{
		{"upload", inside, "/uploads/notes_a1b2.txt"},
		{"missing file", filepath.Join(uploadsDir, "gone_a1b2.txt"), ""},
		{"outside uploads dir", outside, ""},
		{"subdirectory", nested, ""},
		{"empty path", "", ""},
		{"uploads dir itself", uploadsDir, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := &serveServer{}
			entry := srv.sessionMessageFilePart(llm.Part{
				Type:     llm.PartFile,
				FilePath: tt.filePath,
				FileData: &llm.ToolFileData{MediaType: "text/plain", Filename: "notes.txt", SizeBytes: 4},
			})
			if entry.FileURL != tt.wantURL {
				t.Errorf("file_url = %q, want %q", entry.FileURL, tt.wantURL)
			}
			if entry.Kind != filePartKindUpload {
				t.Errorf("kind = %q, want %q", entry.Kind, filePartKindUpload)
			}
			if entry.Text != "notes.txt" || entry.MimeType != "text/plain" || entry.SizeBytes != 4 {
				t.Errorf("chip metadata lost: %#v", entry)
			}
			if strings.Contains(entry.FileURL, filepath.Dir(tt.filePath)) && tt.wantURL == "" {
				t.Errorf("file_url leaked a server path: %q", entry.FileURL)
			}
		})
	}

	// A nil receiver must never panic: the projection is reached from code paths
	// that tolerate an unconfigured server.
	var nilServer *serveServer
	entry := nilServer.sessionMessageFilePart(llm.Part{Type: llm.PartFile, FilePath: inside})
	if entry.FileURL != "" {
		t.Errorf("nil server file_url = %q, want empty", entry.FileURL)
	}
}

func TestSessionMessagePartEntryFileJSONContract(t *testing.T) {
	entry := sessionMessagePartEntry{
		Type:      "file",
		Text:      "archive.zip",
		MimeType:  "application/zip",
		FileURL:   "/ui/uploads/archive_a1b2.zip",
		SizeBytes: 1234,
		Kind:      filePartKindUpload,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"file","text":"archive.zip","mime_type":"application/zip","file_url":"/ui/uploads/archive_a1b2.zip","size_bytes":1234,"kind":"upload"}`
	if string(raw) != want {
		t.Fatalf("file part JSON = %s, want %s", raw, want)
	}

	// A reference chip carries no download metadata at all.
	reference := sessionMessagePartEntry{Type: "file", Text: "docs/notes.md", Kind: filePartKindReference}
	raw, err = json.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"type":"file","text":"docs/notes.md","kind":"reference"}`
	if string(raw) != want {
		t.Fatalf("reference part JSON = %s, want %s", raw, want)
	}
}

func TestUploadDownloadDispositionSanitizes(t *testing.T) {
	for _, tt := range []struct{ label, name, want string }{
		{"plain", "notes.txt", `attachment; filename="notes.txt"`},
		{"quote", "a\"b.txt", `attachment; filename="a_b.txt"`},
		{"crlf", "a\r\nb.txt", `attachment; filename="a__b.txt"`},
		{"windows separators", `..\..\windows.txt`, `attachment; filename=".._.._windows.txt"`},
		{"posix traversal", "../escape.txt", `attachment; filename="escape.txt"`},
		{"empty", "", `attachment; filename="download"`},
		{"dot dot", "..", `attachment; filename="download"`},
		{"non-ascii", "日本語.txt", `attachment; filename="日本語.txt"; filename*=UTF-8''%E6%97%A5%E6%9C%AC%E8%AA%9E.txt`},
	} {
		t.Run(tt.label, func(t *testing.T) {
			if got := uploadDownloadDisposition(tt.name); got != tt.want {
				t.Errorf("uploadDownloadDisposition(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
