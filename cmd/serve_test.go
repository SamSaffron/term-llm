package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type stagedStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	events <-chan llm.Event
}

func (s *stagedStream) Recv() (llm.Event, error) {
	select {
	case event, ok := <-s.events:
		if !ok {
			return llm.Event{}, io.EOF
		}
		return event, nil
	default:
	}

	select {
	case <-s.ctx.Done():
		return llm.Event{}, s.ctx.Err()
	case event, ok := <-s.events:
		if !ok {
			return llm.Event{}, io.EOF
		}
		return event, nil
	}
}

func (s *stagedStream) Close() error {
	s.cancel()
	return nil
}

type stagedProvider struct {
	mu            sync.Mutex
	requests      []llm.Request
	firstChunk    string
	secondChunk   string
	firstSent     chan struct{}
	releaseSecond chan struct{}
	closeFirst    sync.Once
}

func newStagedProvider(firstChunk, secondChunk string) *stagedProvider {
	return &stagedProvider{
		firstChunk:    firstChunk,
		secondChunk:   secondChunk,
		firstSent:     make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (p *stagedProvider) Name() string {
	return "staged"
}

func (p *stagedProvider) Credential() string {
	return "test"
}

func (p *stagedProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (p *stagedProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	streamCtx, cancel := context.WithCancel(ctx)
	ch := make(chan llm.Event, 8)
	go func() {
		defer close(ch)

		select {
		case <-streamCtx.Done():
			return
		case ch <- llm.Event{Type: llm.EventTextDelta, Text: p.firstChunk}:
		}
		p.closeFirst.Do(func() { close(p.firstSent) })

		select {
		case <-streamCtx.Done():
			return
		case <-p.releaseSecond:
		}

		select {
		case <-streamCtx.Done():
			return
		case ch <- llm.Event{Type: llm.EventTextDelta, Text: p.secondChunk}:
		}

		select {
		case <-streamCtx.Done():
			return
		case ch <- llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 1, OutputTokens: 2}}:
		}
	}()

	return &stagedStream{ctx: streamCtx, cancel: cancel, events: ch}, nil
}

func newServeHTTPTestServer(srv *serveServer) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", srv.handleResponses)
	mux.HandleFunc("/v1/responses/", srv.handleResponseByID)
	return httptest.NewServer(mux)
}

func readSSEEvent(t *testing.T, scanner *bufio.Scanner) (string, string, bool) {
	t.Helper()

	var eventName string
	dataLines := make([]string, 0, 1)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return eventName, strings.Join(dataLines, "\n"), true
		}
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	if eventName == "" && len(dataLines) == 0 {
		return "", "", false
	}
	return eventName, strings.Join(dataLines, "\n"), true
}

func TestResolvePlatforms(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		configPlatform []string
		want           []string
		wantErr        string
	}{
		{name: "single web", args: []string{"web"}, want: []string{"web"}},
		{name: "single telegram", args: []string{"telegram"}, want: []string{"telegram"}},
		{name: "multiple platforms", args: []string{"telegram", "web"}, want: []string{"telegram", "web"}},
		{name: "all three", args: []string{"web", "jobs", "telegram"}, want: []string{"web", "jobs", "telegram"}},
		{name: "unknown platform", args: []string{"slack"}, wantErr: `unknown platform "slack"`},
		{name: "mixed valid and invalid", args: []string{"web", "invalid"}, wantErr: `unknown platform "invalid"`},
		{name: "case insensitive", args: []string{"WEB", "Telegram"}, want: []string{"web", "telegram"}},
		{name: "dedup", args: []string{"telegram", "telegram", "web"}, want: []string{"telegram", "web"}},
		{name: "whitespace trimmed", args: []string{" web ", " telegram "}, want: []string{"web", "telegram"}},
		{name: "no args no config", args: nil, wantErr: "no platforms specified"},
		{name: "args override config", args: []string{"web"}, configPlatform: []string{"telegram"}, want: []string{"web"}},
		{name: "config fallback", args: nil, configPlatform: []string{"telegram", "web"}, want: []string{"telegram", "web"}},
		{name: "config dedup", args: nil, configPlatform: []string{"web", "web"}, want: []string{"web"}},
		{name: "config unknown", args: nil, configPlatform: []string{"slack"}, wantErr: `unknown platform "slack"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePlatforms(tt.args, tt.configPlatform)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestPlatformContains(t *testing.T) {
	tests := []struct {
		name      string
		platforms []string
		target    string
		want      bool
	}{
		{name: "found", platforms: []string{"web", "telegram"}, target: "telegram", want: true},
		{name: "not found", platforms: []string{"web", "jobs"}, target: "telegram", want: false},
		{name: "empty list", platforms: nil, target: "web", want: false},
		{name: "single match", platforms: []string{"web"}, target: "web", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := platformContains(tt.platforms, tt.target); got != tt.want {
				t.Fatalf("platformContains(%v, %q) = %v, want %v", tt.platforms, tt.target, got, tt.want)
			}
		})
	}
}

func TestNormalizeBasePath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "default", input: "/ui", want: "/ui"},
		{name: "custom", input: "/chat", want: "/chat"},
		{name: "nested", input: "/app/v2", want: "/app/v2"},
		{name: "trailing slash stripped", input: "/chat/", want: "/chat"},
		{name: "multiple trailing slashes", input: "/chat///", want: "/chat"},
		{name: "no leading slash added", input: "chat", want: "/chat"},
		{name: "whitespace trimmed", input: "  /chat  ", want: "/chat"},
		{name: "root rejected", input: "/", wantErr: true},
		{name: "empty rejected", input: "", wantErr: true},
		{name: "whitespace only rejected", input: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeBasePath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got %q", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeBasePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestServeServerConfig_RouteHelpers(t *testing.T) {
	tests := []struct {
		basePath   string
		wantUI     string
		wantImages string
	}{
		{"/ui", "/ui/", "/ui/images/"},
		{"/chat", "/chat/", "/chat/images/"},
		{"/app/v2", "/app/v2/", "/app/v2/images/"},
	}
	for _, tt := range tests {
		cfg := serveServerConfig{basePath: tt.basePath}
		if got := cfg.uiRoute(); got != tt.wantUI {
			t.Errorf("basePath=%q uiRoute()=%q, want %q", tt.basePath, got, tt.wantUI)
		}
		if got := cfg.imagesRoute(); got != tt.wantImages {
			t.Errorf("basePath=%q imagesRoute()=%q, want %q", tt.basePath, got, tt.wantImages)
		}
	}
}

func TestCustomBasePath_EndToEnd(t *testing.T) {
	// Handlers are called with paths already stripped of basePath by
	// http.StripPrefix in the mux. So "/" is the SPA root, "/app.css" is
	// a static asset, and "/images/" is the images route.
	srv := &serveServer{
		cfg: serveServerConfig{
			ui:       true,
			basePath: "/chat",
		},
	}

	// 1. / serves the SPA with the injected prefix
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/ status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// Prefix should be JSON-escaped (a proper JS string literal, not a template literal)
	if !strings.Contains(body, `TERM_LLM_UI_PREFIX="/chat"`) {
		t.Errorf("/ should inject JSON-escaped TERM_LLM_UI_PREFIX, got:\n%s",
			body[strings.Index(body, "TERM_LLM")-20:strings.Index(body, "TERM_LLM")+60])
	}
	if !strings.Contains(body, `<base href="/chat/">`) {
		t.Error("/ should inject <base> tag with basePath")
	}

	// 2. /app.css serves static assets
	req = httptest.NewRequest(http.MethodGet, "/app.css", nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/app.css status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), ".app {") {
		t.Error("/app.css should contain app styles")
	}

	// 3. /images/ serves images via handleImage (empty filename → 404)
	req = httptest.NewRequest(http.MethodGet, "/images/", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/images/ (empty filename) status = %d, want 404", rr.Code)
	}

	// 4. Traversal attempt also rejected
	req = httptest.NewRequest(http.MethodGet, "/images/..%2Fetc%2Fpasswd", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/images/ traversal status = %d, want 404", rr.Code)
	}
}

func TestServeHTTPHandler_MountsJobsOnlyAtRoot(t *testing.T) {
	srv := &serveServer{
		cfg:    serveServerConfig{basePath: "/ui"},
		jobsV2: &jobsV2Manager{},
	}
	h := srv.httpHandler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/ui/healthz", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/ui/healthz status = %d, want 404", rr.Code)
	}
}

func TestServeHTTPHandler_MountsWebUnderBasePath(t *testing.T) {
	srv := &serveServer{
		cfg: serveServerConfig{ui: true, basePath: "/chat"},
	}
	h := srv.httpHandler()

	req := httptest.NewRequest(http.MethodGet, "/chat/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/chat/healthz status = %d, want 200", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("/healthz status = %d, want 307", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/chat/" {
		t.Fatalf("/healthz redirect location = %q, want %q", loc, "/chat/")
	}
}

func TestNormalizeBasePath_ProducesValidRoutes(t *testing.T) {
	// Verify that normalizeBasePath output always produces valid route helpers
	// and that handleUI/handleImage parse paths correctly.
	inputs := []struct {
		raw          string
		wantBasePath string
	}{
		{"chat", "/chat"},    // no leading slash → added
		{"/chat/", "/chat"},  // trailing slash → stripped
		{"  /app  ", "/app"}, // whitespace → trimmed
		{"/a/b/c", "/a/b/c"}, // nested path
		{"/ui", "/ui"},       // default
	}

	for _, tt := range inputs {
		bp, err := normalizeBasePath(tt.raw)
		if err != nil {
			t.Fatalf("normalizeBasePath(%q) unexpected error: %v", tt.raw, err)
		}
		if bp != tt.wantBasePath {
			t.Fatalf("normalizeBasePath(%q) = %q, want %q", tt.raw, bp, tt.wantBasePath)
		}

		cfg := serveServerConfig{ui: true, basePath: bp}

		// Route helpers produce valid patterns (non-empty, start with /)
		if ui := cfg.uiRoute(); ui == "" || ui[0] != '/' || ui[len(ui)-1] != '/' {
			t.Errorf("basePath=%q: uiRoute()=%q invalid", bp, ui)
		}
		if img := cfg.imagesRoute(); img == "" || img[0] != '/' || img[len(img)-1] != '/' {
			t.Errorf("basePath=%q: imagesRoute()=%q invalid", bp, img)
		}

		// handleUI serves SPA at "/" (basePath stripped by StripPrefix in mux)
		srv := &serveServer{cfg: cfg}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		srv.handleUI(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("basePath=%q: handleUI(/) status=%d, want 200", bp, rr.Code)
		}

		// handleUI serves assets at "/app.css" (basePath stripped)
		req = httptest.NewRequest(http.MethodGet, "/app.css", nil)
		rr = httptest.NewRecorder()
		srv.handleUI(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("basePath=%q: handleUI(/app.css) status=%d, want 200", bp, rr.Code)
		}

		// handleImage rejects empty filename at "/images/" (basePath stripped)
		req = httptest.NewRequest(http.MethodGet, "/images/", nil)
		rr = httptest.NewRecorder()
		srv.handleImage(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("basePath=%q: handleImage(/images/) status=%d, want 404", bp, rr.Code)
		}
	}
}

func TestNormalizeBasePath_RejectsInvalid(t *testing.T) {
	// These must all be rejected — if they weren't, they'd produce
	// empty or root-only routes that would panic or misbehave.
	invalid := []string{"/", "", "   ", "///"}
	for _, input := range invalid {
		_, err := normalizeBasePath(input)
		if err == nil {
			t.Errorf("normalizeBasePath(%q) should have returned an error", input)
		}
	}
}

func TestSingleServeTemplatePlatform(t *testing.T) {
	tests := []struct {
		name      string
		platforms []string
		want      string
	}{
		{name: "web only", platforms: []string{"web"}, want: "web"},
		{name: "telegram only", platforms: []string{"telegram"}, want: "telegram"},
		{name: "jobs only", platforms: []string{"jobs"}, want: "jobs"},
		{name: "web and telegram", platforms: []string{"web", "telegram"}, want: ""},
		{name: "web and jobs", platforms: []string{"web", "jobs"}, want: ""},
		{name: "unknown ignored", platforms: []string{"web", "foo"}, want: "web"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := singleServeTemplatePlatform(tt.platforms); got != tt.want {
				t.Fatalf("singleServeTemplatePlatform(%v) = %q, want %q", tt.platforms, got, tt.want)
			}
		})
	}
}

func TestHandleUI_ReturnsEmbeddedStaticAsset(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}

	// Paths are as seen by the handler after StripPrefix removes basePath.
	tests := []struct {
		name        string
		path        string
		contentType string
		bodySnippet string
	}{
		{name: "css", path: "/app.css", contentType: "text/css", bodySnippet: ".app {"},
		{name: "js", path: "/app-core.js", contentType: "text/javascript", bodySnippet: "window.TermLLMApp"},
		{name: "manifest", path: "/manifest.webmanifest", contentType: "", bodySnippet: `"display": "standalone"`},
		{name: "vendor_subdir_js", path: "/vendor/katex/katex.min.js", contentType: "text/javascript", bodySnippet: "katex"},
		{name: "vendor_subdir_css", path: "/vendor/hljs/github-dark.min.css", contentType: "text/css", bodySnippet: ".hljs"},
		{name: "vendor_woff2", path: "/vendor/katex/fonts/KaTeX_Main-Regular.woff2", contentType: "font/woff2", bodySnippet: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rr := httptest.NewRecorder()

			srv.handleUI(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got := rr.Header().Get("Content-Type"); tt.contentType != "" && !strings.HasPrefix(got, tt.contentType) {
				t.Fatalf("content-type = %q, want %s", got, tt.contentType)
			}
			if tt.bodySnippet != "" && !strings.Contains(rr.Body.String(), tt.bodySnippet) {
				t.Fatalf("expected %q in asset response, got %q", tt.bodySnippet, rr.Body.String())
			}
		})
	}
}

func TestHandleUI_VersionedAssetCaching(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}

	// Versioned asset gets immutable caching.
	req := httptest.NewRequest(http.MethodGet, "/vendor/katex/katex.min.js?v=0.16.38", nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("versioned asset cache-control = %q, want immutable", got)
	}

	// Unversioned asset gets no-cache.
	req = httptest.NewRequest(http.MethodGet, "/app.css", nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("unversioned asset cache-control = %q, want no-cache", got)
	}
}

func TestHandleUI_IndexVersionsShellAssets(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}
	version := serveui.AssetVersion()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, snippet := range []string{
		`href="manifest.webmanifest?v=` + version + `"`,
		`href="icon-512.png?v=` + version + `"`,
		`href="app.css?v=` + version + `"`,
		`src="app-core.js?v=` + version + `"`,
		`src="app-render.js?v=` + version + `"`,
		`src="app-stream.js?v=` + version + `"`,
		`src="app-sessions.js?v=` + version + `"`,
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("expected %q in body", snippet)
		}
	}
}

func TestHandleUI_ServiceWorkerVersionsShellCache(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}
	version := serveui.AssetVersion()

	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, snippet := range []string{
		`term-llm-shell-` + version,
		`'./manifest.webmanifest?v=` + version + `'`,
		`'./icon-512.png?v=` + version + `'`,
		`'./app.css?v=` + version + `'`,
		`'./app-core.js?v=` + version + `'`,
		`'./app-render.js?v=` + version + `'`,
		`'./app-stream.js?v=` + version + `'`,
		`'./app-sessions.js?v=` + version + `'`,
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("expected %q in body", snippet)
		}
	}
}

func TestParseResponsesInput_String(t *testing.T) {
	msgs, replaceHistory, err := parseResponsesInput(json.RawMessage(`"hello"`))
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if replaceHistory {
		t.Fatalf("replaceHistory = true, want false")
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("role = %s, want user", msgs[0].Role)
	}
	if got := msgs[0].Parts[0].Text; got != "hello" {
		t.Fatalf("text = %q, want %q", got, "hello")
	}
}

func TestParseResponsesInput_ImageContent(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},
			{"type":"input_text","text":"describe this image"}
		]}
	]`)
	msgs, _, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.Role != llm.RoleUser {
		t.Fatalf("role = %s, want user", msg.Role)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msg.Parts))
	}
	if msg.Parts[0].Type != llm.PartImage {
		t.Fatalf("parts[0].type = %s, want image", msg.Parts[0].Type)
	}
	if msg.Parts[0].ImageData == nil {
		t.Fatalf("parts[0].ImageData = nil")
	}
	if msg.Parts[0].ImageData.MediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", msg.Parts[0].ImageData.MediaType)
	}
	if msg.Parts[0].ImageData.Base64 != "aGVsbG8=" {
		t.Fatalf("base64 = %q, want aGVsbG8=", msg.Parts[0].ImageData.Base64)
	}
	if msg.Parts[0].ImagePath == "" {
		t.Fatal("parts[0].ImagePath = empty, want saved upload path")
	}
	if _, err := os.Stat(msg.Parts[0].ImagePath); err != nil {
		t.Fatalf("saved upload missing at %q: %v", msg.Parts[0].ImagePath, err)
	}
	if msg.Parts[1].Type != llm.PartText {
		t.Fatalf("parts[1].type = %s, want text", msg.Parts[1].Type)
	}
	if msg.Parts[1].Text != "describe this image" {
		t.Fatalf("parts[1].text = %q, want %q", msg.Parts[1].Text, "describe this image")
	}
}

func TestParseResponsesInput_FileUploadSavesToDisk(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	// base64 of "hello world"
	b64 := "aGVsbG8gd29ybGQ="
	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_file","file_data":"data:application/pdf;base64,` + b64 + `","filename":"doc.pdf"},
			{"type":"input_text","text":"summarize this"}
		]}
	]`)
	msgs, _, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if len(msg.Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msg.Parts))
	}
	if msg.Parts[0].Type != llm.PartText {
		t.Fatalf("parts[0].type = %s, want text", msg.Parts[0].Type)
	}
	if !strings.Contains(msg.Parts[0].Text, "doc.pdf") {
		t.Fatalf("parts[0].text = %q, should mention doc.pdf", msg.Parts[0].Text)
	}
	if msg.Parts[1].Text != "summarize this" {
		t.Fatalf("parts[1].text = %q, want %q", msg.Parts[1].Text, "summarize this")
	}

	// Verify file was actually written to disk with correct content
	uploadsDir := filepath.Join(dataHome, "term-llm", "uploads")
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("uploads dir has %d files, want 1", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(uploadsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("file content = %q, want %q", got, "hello world")
	}
	// Verify restrictive permissions
	info, _ := entries[0].Info()
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("file permissions = %o, want no group/other access", perm)
	}
	// Verify abbreviatePath works when path is under home dir
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(msg.Parts[0].Text, home) {
		t.Fatalf("parts[0].text leaks absolute home path: %q", msg.Parts[0].Text)
	}
}

func TestParseResponsesInput_UnsupportedImageSavesToDisk(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	// image/svg+xml is not a supported LLM image type
	b64 := "PHN2Zz48L3N2Zz4=" // base64 of "<svg></svg>"
	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"data:image/svg+xml;base64,` + b64 + `","filename":"icon.svg"}
		]}
	]`)
	msgs, _, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if len(msg.Parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1", len(msg.Parts))
	}
	if msg.Parts[0].Type != llm.PartText {
		t.Fatalf("parts[0].type = %s, want text (saved to disk)", msg.Parts[0].Type)
	}
	if !strings.Contains(msg.Parts[0].Text, "icon.svg") {
		t.Fatalf("parts[0].text = %q, should mention icon.svg", msg.Parts[0].Text)
	}

	// Verify file on disk
	uploadsDir := filepath.Join(dataHome, "term-llm", "uploads")
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("uploads dir has %d files, want 1", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(uploadsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(got) != "<svg></svg>" {
		t.Fatalf("file content = %q, want %q", got, "<svg></svg>")
	}
}

func TestParseResponsesInput_InvalidBase64ReturnsError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	payload := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_file","file_data":"data:application/pdf;base64,!!!invalid!!!","filename":"bad.pdf"}
		]}
	]`)
	_, _, err := parseResponsesInput(payload)
	if err == nil {
		t.Fatalf("expected error for invalid base64, got nil")
	}
	if !strings.Contains(err.Error(), "bad.pdf") {
		t.Fatalf("error = %q, should mention filename", err.Error())
	}
}

func TestAbbreviatePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	got := abbreviatePath(home + "/foo/bar.txt")
	if got != "~/foo/bar.txt" {
		t.Fatalf("abbreviatePath(%q) = %q, want %q", home+"/foo/bar.txt", got, "~/foo/bar.txt")
	}
	// Paths outside home are returned unchanged
	got = abbreviatePath("/tmp/other.txt")
	if got != "/tmp/other.txt" {
		t.Fatalf("abbreviatePath(%q) = %q, want unchanged", "/tmp/other.txt", got)
	}
}

func TestParseDataURL(t *testing.T) {
	mt, b64 := parseDataURL("data:image/jpeg;base64,/9j/4AAQ")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg", mt)
	}
	if b64 != "/9j/4AAQ" {
		t.Fatalf("base64 = %q, want /9j/4AAQ", b64)
	}

	mt, b64 = parseDataURL("not-a-data-url")
	if mt != "" || b64 != "" {
		t.Fatalf("expected empty for invalid data URL, got %q %q", mt, b64)
	}

	mt, b64 = parseDataURL("data:text/plain;charset=utf-8,hello")
	if mt != "" || b64 != "" {
		t.Fatalf("expected empty for non-base64 data URL, got %q %q", mt, b64)
	}
}

func TestParseResponsesInput_FunctionCallOutput(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"content"}
	]`)
	msgs, replaceHistory, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[1].Role != llm.RoleTool {
		t.Fatalf("second role = %s, want tool", msgs[1].Role)
	}
	if msgs[1].Parts[0].ToolResult == nil || msgs[1].Parts[0].ToolResult.ID != "call_1" {
		t.Fatalf("missing tool result id")
	}
}

func TestParseChatMessages_ToolCallAndToolResult(t *testing.T) {
	msgs, replaceHistory, err := parseChatMessages([]chatMessage{
		{
			Role:    "assistant",
			Content: json.RawMessage(`"running"`),
			ToolCalls: []chatToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "read_file",
					Arguments: `{"path":"a.txt"}`,
				},
			}},
		},
		{
			Role:       "tool",
			ToolCallID: "call_1",
			Content:    json.RawMessage(`"done"`),
		},
	})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleAssistant {
		t.Fatalf("first role = %s, want assistant", msgs[0].Role)
	}
	if msgs[1].Role != llm.RoleTool {
		t.Fatalf("second role = %s, want tool", msgs[1].Role)
	}
	if msgs[1].Parts[0].ToolResult == nil || msgs[1].Parts[0].ToolResult.Name != "read_file" {
		t.Fatalf("tool result name missing")
	}
}

func TestParseToolChoice(t *testing.T) {
	if got := parseToolChoice(json.RawMessage(`"none"`)); got.Mode != llm.ToolChoiceNone {
		t.Fatalf("mode = %s, want none", got.Mode)
	}
	if got := parseToolChoice(json.RawMessage(`{"type":"function","name":"shell"}`)); got.Mode != llm.ToolChoiceName || got.Name != "shell" {
		t.Fatalf("name choice = %#v", got)
	}
}

func TestServeAuthMiddleware(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "secret"}}
	h := srv.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestServeSessionManager_GetOrCreateSingleFactoryCall(t *testing.T) {
	var calls int32
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(25 * time.Millisecond)
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	const workers = 12
	results := make(chan *serveRuntime, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			rt, err := manager.GetOrCreate(context.Background(), "same-id")
			if err != nil {
				errs <- err
				return
			}
			results <- rt
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrCreate error: %v", err)
		}
	}

	var first *serveRuntime
	for rt := range results {
		if first == nil {
			first = rt
			continue
		}
		if rt != first {
			t.Fatalf("expected all calls to return same runtime pointer")
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
}

func TestRequireJSONContentType(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	if err := requireJSONContentType(req); err == nil {
		t.Fatalf("expected error for missing Content-Type")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	if err := requireJSONContentType(req); err == nil {
		t.Fatalf("expected error for non-json Content-Type")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if err := requireJSONContentType(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewServeEngineWithTools_ConfiguresToolManagerAndSpawnWiring(t *testing.T) {
	cfg := &config.Config{}
	settings := SessionSettings{Tools: tools.ReadFileToolName}
	provider := llm.NewMockProvider("mock")

	wireCalls := 0
	gotYolo := false
	wireSpawn := func(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool) error {
		wireCalls++
		if cfg == nil {
			t.Fatalf("cfg = nil")
		}
		if toolMgr == nil {
			t.Fatalf("toolMgr = nil")
		}
		gotYolo = yoloMode
		return nil
	}

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, true, wireSpawn, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if engine == nil {
		t.Fatalf("engine = nil")
	}
	if toolMgr == nil {
		t.Fatalf("toolMgr = nil")
	}
	if !toolMgr.ApprovalMgr.YoloMode {
		t.Fatalf("toolMgr.ApprovalMgr.YoloMode = false, want true")
	}
	if wireCalls != 1 {
		t.Fatalf("wireCalls = %d, want 1", wireCalls)
	}
	if !gotYolo {
		t.Fatalf("yolo mode not passed to spawn wiring")
	}
	if _, ok := engine.Tools().Get(tools.ReadFileToolName); !ok {
		t.Fatalf("expected %q tool to be registered on engine", tools.ReadFileToolName)
	}
}

func TestNewServeEngineWithTools_SkipsToolManagerWhenToolsDisabled(t *testing.T) {
	cfg := &config.Config{}
	settings := SessionSettings{}
	provider := llm.NewMockProvider("mock")

	wireCalls := 0
	wireSpawn := func(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool) error {
		wireCalls++
		return nil
	}

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, false, wireSpawn, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if engine == nil {
		t.Fatalf("engine = nil")
	}
	if toolMgr != nil {
		t.Fatalf("toolMgr != nil, want nil")
	}
	if wireCalls != 0 {
		t.Fatalf("wireCalls = %d, want 0", wireCalls)
	}
}

// echoTool is a minimal tool for testing the agentic loop in serve.
type echoTool struct{}

func (e *echoTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "echo",
		Description: "Echoes input",
		Schema:      map[string]any{"type": "object"},
	}
}

func (e *echoTool) Execute(_ context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("echoed"), nil
}

func (e *echoTool) Preview(_ json.RawMessage) string { return "" }

func TestServeRuntimeRun_PersistsToolCallMessages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// Script: turn 1 = tool call, turn 2 = final text after tool result
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{
		SessionID: "toolcall-persist-test",
		MaxTurns:  5,
		Tools:     []llm.ToolSpec{(&echoTool{}).Spec()},
	}
	_, err = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("call the echo tool"),
	}, req)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Fetch persisted messages
	msgs, err := store.GetMessages(context.Background(), "toolcall-persist-test", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}

	// Expect: user, assistant(tool_call), tool(result), assistant(text)
	var hasToolCall bool
	var hasToolResult bool
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolCall && p.ToolCall != nil && p.ToolCall.Name == "echo" {
				hasToolCall = true
			}
			if p.Type == llm.PartToolResult && p.ToolResult != nil && p.ToolResult.ID == "call-1" {
				hasToolResult = true
			}
		}
	}
	if !hasToolCall {
		t.Fatalf("persisted messages missing assistant tool_call part; messages: %d", len(msgs))
	}
	if !hasToolResult {
		t.Fatalf("persisted messages missing tool_result part; messages: %d", len(msgs))
	}
}

func TestServeRuntimeRun_PersistsSessionAndMessages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock").AddTextResponse("hello from serve")
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
		search:       true,
		toolsSetting: tools.ReadFileToolName,
		mcpSetting:   "playwright",
		agentName:    "reviewer",
	}
	rt.Touch()

	req := llm.Request{
		SessionID: "serve-session-1",
		MaxTurns:  3,
	}
	_, err = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("test persistence"),
	}, req)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	sess, err := store.Get(context.Background(), "serve-session-1")
	if err != nil {
		t.Fatalf("Get session failed: %v", err)
	}
	if sess == nil {
		t.Fatalf("session was not persisted")
	}
	if sess.Summary == "" {
		t.Fatalf("session summary was not set")
	}

	msgs, err := store.GetMessages(context.Background(), "serve-session-1", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("message count = %d, want >= 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("first role = %s, want user", msgs[0].Role)
	}
	if msgs[len(msgs)-1].Role != llm.RoleAssistant {
		t.Fatalf("last role = %s, want assistant", msgs[len(msgs)-1].Role)
	}
}

func TestHandleResponses_GeneratesSessionIDHeaderWhenMissing(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{
		sessionMgr: manager,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := strings.TrimSpace(rr.Header().Get("x-session-id")); got == "" {
		t.Fatalf("x-session-id header missing")
	}
}

func TestResolveServeAuthMode(t *testing.T) {
	mode, err := resolveServeAuthMode(false, "bearer", false, false)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "bearer" {
		t.Fatalf("mode = %q, want bearer", mode)
	}

	mode, err = resolveServeAuthMode(false, "none", false, false)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "none" {
		t.Fatalf("mode = %q, want none", mode)
	}

	mode, err = resolveServeAuthMode(false, "bearer", true, true)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "none" {
		t.Fatalf("mode = %q, want none", mode)
	}

	if _, err := resolveServeAuthMode(true, "bearer", true, true); err == nil {
		t.Fatalf("expected conflict error when --auth and --allow-no-auth disagree")
	}

	if _, err := resolveServeAuthMode(true, "invalid", false, false); err == nil {
		t.Fatalf("expected invalid auth mode error")
	}
}

func TestServeAuthMiddleware_CookieFallback(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "secret"}}
	h := srv.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// No credentials → 401
	req := httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want 401", rr.Code)
	}

	// Valid cookie → allowed
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("valid cookie: status = %d, want 204", rr.Code)
	}

	// Wrong cookie → 401
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "wrong"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: status = %d, want 401", rr.Code)
	}

	// Bearer still works
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer: status = %d, want 204", rr.Code)
	}

	// Cookie on POST → rejected (cookie fallback is GET-only)
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("cookie on POST: status = %d, want 401", rr.Code)
	}

	// URL-encoded cookie → decoded and accepted
	srv2 := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "se+cret/val="}}
	h2 := srv2.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "se%2Bcret%2Fval%3D"})
	rr = httptest.NewRecorder()
	h2(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("url-encoded cookie: status = %d, want 204", rr.Code)
	}
}

func TestHandleImage_ServesFileAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cat.png"), []byte("fake-png"), 0644); err != nil {
		t.Fatalf("write test image: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui"}, cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = dir

	// Paths as seen by handler after StripPrefix removes basePath.
	// Valid file
	req := httptest.NewRequest(http.MethodGet, "/images/cat.png", nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "fake-png" {
		t.Fatalf("body = %q, want %q", got, "fake-png")
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control = %q, want 'private'", cc)
	}
	if vary := rr.Header().Get("Vary"); vary == "" {
		t.Fatalf("missing Vary header")
	}

	// Path traversal with ..
	req = httptest.NewRequest(http.MethodGet, "/images/..%2Fetc%2Fpasswd", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("traversal: status = %d, want 404", rr.Code)
	}

	// Empty filename
	req = httptest.NewRequest(http.MethodGet, "/images/", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("empty: status = %d, want 404", rr.Code)
	}

	// Nonexistent file
	req = httptest.NewRequest(http.MethodGet, "/images/nope.png", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing: status = %d, want 404", rr.Code)
	}
}

func TestEnsureImageServeable_CopiesExternalFile(t *testing.T) {
	outputDir := t.TempDir()
	externalDir := t.TempDir()

	// Create a file outside the image output directory
	externalImg := filepath.Join(externalDir, "photo.png")
	if err := os.WriteFile(externalImg, []byte("external-image-data"), 0644); err != nil {
		t.Fatalf("write external image: %v", err)
	}

	// Create a file already inside the output directory
	internalImg := filepath.Join(outputDir, "generated.png")
	if err := os.WriteFile(internalImg, []byte("internal-image-data"), 0644); err != nil {
		t.Fatalf("write internal image: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui"}, cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = outputDir

	// External file should be copied
	result, ok := srv.ensureImageServeable(externalImg)
	if !ok {
		t.Fatal("ensureImageServeable should succeed for external image")
	}
	if result == externalImg {
		t.Fatal("external image should have been copied, but path is unchanged")
	}
	absResult, _ := filepath.Abs(result)
	absOutputDir, _ := filepath.Abs(outputDir)
	if !strings.HasPrefix(absResult, absOutputDir+string(filepath.Separator)) {
		t.Fatalf("copied image %q should be under output dir %q", absResult, absOutputDir)
	}
	// Verify the copied file contains the correct data
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("read copied image: %v", err)
	}
	if string(data) != "external-image-data" {
		t.Fatalf("copied data = %q, want %q", string(data), "external-image-data")
	}

	// Internal file should be returned as-is
	result, ok = srv.ensureImageServeable(internalImg)
	if !ok {
		t.Fatal("ensureImageServeable should succeed for internal image")
	}
	if result != internalImg {
		t.Fatalf("internal image should be unchanged, got %q want %q", result, internalImg)
	}

	// Verify the copied file is actually serveable via handleImage
	copied, ok := srv.ensureImageServeable(externalImg)
	if !ok {
		t.Fatal("ensureImageServeable should succeed for second external copy")
	}
	copiedName := filepath.Base(copied)
	req := httptest.NewRequest(http.MethodGet, "/images/"+copiedName, nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("serve copied image: status = %d, want 200", rr.Code)
	}
}

func TestHandleSessions_ListsFromStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "test-session-1",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		Summary:   "hello world",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Sessions []struct {
			ID           string `json:"id"`
			Summary      string `json:"summary"`
			CreatedAt    int64  `json:"created_at"`
			MessageCount int    `json:"message_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(body.Sessions))
	}
	if body.Sessions[0].ID != "test-session-1" {
		t.Fatalf("id = %q, want %q", body.Sessions[0].ID, "test-session-1")
	}
	if body.Sessions[0].Summary != "hello world" {
		t.Fatalf("summary = %q, want %q", body.Sessions[0].Summary, "hello world")
	}
}

func TestHandleTranscribe_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("path = %q, want /audio/transcriptions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q, want Bearer test-key", got)
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		f, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer f.Close()
		if header.Filename == "" {
			t.Fatal("expected filename")
		}
		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(data) != "fake audio" {
			t.Fatalf("audio payload = %q, want fake audio", string(data))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "hello from audio"})
	}))
	defer upstream.Close()

	srv := &serveServer{cfgRef: &config.Config{
		Providers: map[string]config.ProviderConfig{
			"openai": {
				ResolvedAPIKey: "test-key",
				BaseURL:        upstream.URL,
			},
		},
	}}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "voice-note.webm")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("fake audio")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/transcribe", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()

	srv.handleTranscribe(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Text != "hello from audio" {
		t.Fatalf("text = %q, want %q", payload.Text, "hello from audio")
	}
}

func TestHandleTranscribe_RejectsUnsupportedType(t *testing.T) {
	srv := &serveServer{cfgRef: &config.Config{}}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="voice-note.bin"`},
		"Content-Type":        {"application/octet-stream"},
	})
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := fw.Write([]byte("nope")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/transcribe", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()

	srv.handleTranscribe(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unsupported") {
		t.Fatalf("body = %q, want unsupported error", rr.Body.String())
	}
}

func TestHandleSessionMessages_ReturnsStructuredParts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-parts",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Message with text + multiple tool calls (the case that was lossy before)
	msg := session.NewMessage("sess-parts", llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "Let me search for that"},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "web_search", Arguments: json.RawMessage(`{"query":"go"}`)}},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-2", Name: "read_url", Arguments: json.RawMessage(`{"url":"https://go.dev"}`)}},
		},
	}, -1)
	if err := store.AddMessage(ctx, "sess-parts", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-parts/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				ToolName   string `json:"tool_name"`
				ToolArgs   string `json:"tool_arguments"`
				ToolCallID string `json:"tool_call_id"`
				ImageURL   string `json:"image_url"`
				MimeType   string `json:"mime_type"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	m := body.Messages[0]
	if m.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", m.Role)
	}
	if len(m.Parts) != 3 {
		t.Fatalf("parts count = %d, want 3", len(m.Parts))
	}

	// Text part
	if m.Parts[0].Type != "text" || m.Parts[0].Text != "Let me search for that" {
		t.Fatalf("part[0] = %+v, want text part", m.Parts[0])
	}
	// First tool call
	if m.Parts[1].Type != "tool_call" || m.Parts[1].ToolName != "web_search" || m.Parts[1].ToolCallID != "call-1" {
		t.Fatalf("part[1] = %+v, want web_search tool_call", m.Parts[1])
	}
	if m.Parts[1].ToolArgs != `{"query":"go"}` {
		t.Fatalf("part[1].args = %q", m.Parts[1].ToolArgs)
	}
	// Second tool call (was lost before due to break)
	if m.Parts[2].Type != "tool_call" || m.Parts[2].ToolName != "read_url" || m.Parts[2].ToolCallID != "call-2" {
		t.Fatalf("part[2] = %+v, want read_url tool_call", m.Parts[2])
	}
}

func TestHandleSessionMessages_IncludesImageParts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-image", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := session.NewMessage("sess-image", llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: llm.PartText, Text: "describe this"},
		},
	}, -1)
	if err := store.AddMessage(ctx, "sess-image", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-image/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
				MimeType string `json:"mime_type"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	m := body.Messages[0]
	if m.Role != "user" {
		t.Fatalf("role = %q, want user", m.Role)
	}
	if len(m.Parts) != 2 {
		t.Fatalf("parts count = %d, want 2", len(m.Parts))
	}
	if m.Parts[0].Type != "image" {
		t.Fatalf("part[0].type = %q, want image", m.Parts[0].Type)
	}
	if m.Parts[0].ImageURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("part[0].image_url = %q, want data URL", m.Parts[0].ImageURL)
	}
	if m.Parts[0].MimeType != "image/png" {
		t.Fatalf("part[0].mime_type = %q, want image/png", m.Parts[0].MimeType)
	}
	if m.Parts[1].Type != "text" || m.Parts[1].Text != "describe this" {
		t.Fatalf("part[1] = %+v, want trailing text part", m.Parts[1])
	}
}

func TestHandleSessionMessages_OmitsToolResults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-tr", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := session.NewMessage("sess-tr", llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "result msg"},
			{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-1", Content: "verbose tool output"}},
		},
	}, -1)
	if err := store.AddMessage(ctx, "sess-tr", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-tr/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Parts []struct{ Type string } `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	for _, p := range body.Messages[0].Parts {
		if p.Type == "tool_result" {
			t.Fatal("tool_result parts should be omitted from API response")
		}
	}
	if len(body.Messages[0].Parts) != 1 {
		t.Fatalf("parts count = %d, want 1 (text only)", len(body.Messages[0].Parts))
	}
}

func TestEnsurePersistedSession_RestoresHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "restore-test",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msgs := []session.Message{
		*session.NewMessage("restore-test", llm.UserText("hello"), -1),
		*session.NewMessage("restore-test", llm.Message{
			Role:  llm.RoleAssistant,
			Parts: []llm.Part{{Type: llm.PartText, Text: "hi there"}},
		}, -1),
	}
	if err := store.ReplaceMessages(ctx, "restore-test", msgs); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	// Simulate a fresh runtime with empty history
	rt := &serveRuntime{
		store:        store,
		defaultModel: "mock-model",
		provider:     llm.NewMockProvider("mock"),
	}

	ok := rt.ensurePersistedSession(ctx, "restore-test", nil)
	if !ok {
		t.Fatalf("ensurePersistedSession returned false")
	}
	if len(rt.history) != 2 {
		t.Fatalf("history len = %d, want 2", len(rt.history))
	}
	if rt.history[0].Role != llm.RoleUser {
		t.Fatalf("history[0].role = %s, want user", rt.history[0].Role)
	}
	if rt.history[1].Role != llm.RoleAssistant {
		t.Fatalf("history[1].role = %s, want assistant", rt.history[1].Role)
	}
	if rt.history[1].Parts[0].Text != "hi there" {
		t.Fatalf("history[1].text = %q, want %q", rt.history[1].Parts[0].Text, "hi there")
	}
}

func TestEnsurePersistedSession_SkipsRestoreWhenHistoryExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "skip-restore",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dbMsg := session.NewMessage("skip-restore", llm.UserText("db message"), -1)
	if err := store.ReplaceMessages(ctx, "skip-restore", []session.Message{*dbMsg}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	// Runtime already has history — should NOT overwrite
	existing := []llm.Message{llm.UserText("existing")}
	rt := &serveRuntime{
		store:        store,
		defaultModel: "mock-model",
		provider:     llm.NewMockProvider("mock"),
		history:      existing,
	}

	ok := rt.ensurePersistedSession(ctx, "skip-restore", nil)
	if !ok {
		t.Fatalf("ensurePersistedSession returned false")
	}
	if len(rt.history) != 1 {
		t.Fatalf("history len = %d, want 1 (unchanged)", len(rt.history))
	}
	if rt.history[0].Parts[0].Text != "existing" {
		t.Fatalf("history was overwritten")
	}
}

type testServeDelayTool struct {
	delay time.Duration
}

func (d *testServeDelayTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "slow_tool", Description: "delay for interjection test"}
}

func (d *testServeDelayTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	select {
	case <-ctx.Done():
		return llm.ToolOutput{}, ctx.Err()
	case <-time.After(d.delay):
	}
	return llm.ToolOutput{Content: "slept"}, nil
}

func (d *testServeDelayTool) Preview(args json.RawMessage) string {
	return ""
}

// newTestServeServer creates a serveServer with a mock factory for testing.
// Each runtime gets its own mock provider with the given responses.
func newTestServeServer(responses ...string) *serveServer {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		for _, r := range responses {
			provider.AddTextResponse(r)
		}
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	return srv
}

// doResponses sends a non-streaming /v1/responses request and returns the parsed response body.
func doResponses(t *testing.T, srv *serveServer, bodyJSON string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	var result map[string]any
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
	}
	return rr.Code, result
}

// doResponsesWithHeader sends a non-streaming /v1/responses request with a session_id header.
func doResponsesWithHeader(t *testing.T, srv *serveServer, bodyJSON, sessionID string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("session_id", sessionID)
	}
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	var result map[string]any
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
	}
	return rr.Code, result
}

// waitForServeCondition polls until fn returns true or timeout elapses.
func waitForServeCondition(t *testing.T, timeout time.Duration, fn func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestHandleResponses_UIAskUserResumeSurvivesDisconnect(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_ask_1", tools.AskUserToolName, map[string]any{
		"questions": []map[string]any{{
			"header":   "Theme",
			"question": "Pick a theme",
			"options": []map[string]any{
				{"label": "Dark", "description": "Use dark mode"},
				{"label": "Light", "description": "Use light mode"},
			},
		}},
	})
	provider.AddTextResponse("All set.")

	engine := llm.NewEngine(provider, nil)
	engine.RegisterTool(tools.NewAskUserTool())

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
	}
	rt.Touch()

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr, store: store}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := `{"stream":true,"input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "resume-session")
	req.Header.Set("X-Term-LLM-UI", "1")
	rr := httptest.NewRecorder()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		srv.handleResponses(rr, req)
	}()

	waitForServeCondition(t, time.Second, func() bool {
		return len(rt.pendingAskUserPrompts()) == 1
	}, "pending ask_user prompt")

	stateReq := httptest.NewRequest(http.MethodGet, "/v1/sessions/resume-session/state", nil)
	stateRR := httptest.NewRecorder()
	srv.handleSessionByID(stateRR, stateReq)
	if stateRR.Code != http.StatusOK {
		t.Fatalf("state status = %d, want 200", stateRR.Code)
	}
	var stateBody struct {
		ActiveRun        bool                `json:"active_run"`
		ActiveResponseID string              `json:"active_response_id"`
		PendingAskUser   *serveAskUserPrompt `json:"pending_ask_user"`
	}
	if err := json.Unmarshal(stateRR.Body.Bytes(), &stateBody); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !stateBody.ActiveRun {
		t.Fatal("expected active_run=true while ask_user is pending")
	}
	if !strings.HasPrefix(stateBody.ActiveResponseID, "resp_") {
		t.Fatalf("active_response_id = %q, want resp_ prefix", stateBody.ActiveResponseID)
	}
	if stateBody.PendingAskUser == nil || stateBody.PendingAskUser.CallID != "call_ask_1" {
		t.Fatalf("unexpected pending ask_user state: %#v", stateBody.PendingAskUser)
	}

	waitForServeCondition(t, time.Second, func() bool {
		msgs, err := store.GetMessages(context.Background(), "resume-session", 0, 0)
		if err != nil || len(msgs) < 2 {
			return false
		}
		for _, msg := range msgs {
			if msg.Role != llm.RoleAssistant {
				continue
			}
			for _, part := range msg.Parts {
				if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.Name == tools.AskUserToolName {
					return true
				}
			}
		}
		return false
	}, "persisted ask_user tool call snapshot")

	cancel()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for streaming request to detach")
	}

	if !rt.hasActiveRun() {
		t.Fatal("expected runtime to remain active after client disconnect")
	}

	submitBody := `{"call_id":"call_ask_1","answers":[{"question_index":0,"header":"Theme","selected":"Dark","is_custom":false}]}`
	submitReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/resume-session/ask_user", strings.NewReader(submitBody))
	submitReq.Header.Set("Content-Type", "application/json")
	submitRR := httptest.NewRecorder()
	srv.handleSessionByID(submitRR, submitReq)
	if submitRR.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body=%s", submitRR.Code, submitRR.Body.String())
	}

	waitForServeCondition(t, time.Second, func() bool {
		if rt.hasActiveRun() {
			return false
		}
		msgs, err := store.GetMessages(context.Background(), "resume-session", 0, 0)
		if err != nil {
			return false
		}
		for _, msg := range msgs {
			if msg.Role == llm.RoleAssistant && strings.Contains(msg.TextContent, "All set.") {
				return true
			}
		}
		return false
	}, "completed resumed ask_user run")
}

func TestHandleSessionState_ConsumesDeferredUIRunError(t *testing.T) {
	rt := &serveRuntime{}
	rt.setLastUIRunError("resume failed")
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions["sess-state"] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-state/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first state status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "resume failed") {
		t.Fatalf("expected first state response to include deferred error, got %s", rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	srv.handleSessionByID(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second state status = %d", rr2.Code)
	}
	if strings.Contains(rr2.Body.String(), "resume failed") {
		t.Fatalf("expected deferred error to be consumed, got %s", rr2.Body.String())
	}
}

func TestHandleResponses_ReturnsStableResponseID(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	code, resp := doResponses(t, srv, `{"input":"hi"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	id, _ := resp["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("response id = %q, want resp_ prefix", id)
	}
}

func TestHandleResponses_IncludesSessionUsage(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	code, resp := doResponses(t, srv, `{"input":"hi"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	sessionUsage, ok := resp["session_usage"].(map[string]any)
	if !ok {
		t.Fatalf("session_usage missing from response")
	}
	// Session usage should have the same structure as usage
	if _, ok := sessionUsage["input_tokens"]; !ok {
		t.Fatalf("session_usage missing input_tokens")
	}
	if _, ok := sessionUsage["output_tokens"]; !ok {
		t.Fatalf("session_usage missing output_tokens")
	}
}

func TestHandleResponses_UnknownPreviousResponseIDReturnsError(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	body := `{"input":"hi","previous_response_id":"resp_does_not_exist"}`
	code, _ := doResponses(t, srv, body)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown previous_response_id", code)
	}
}

func TestHandleResponses_UnknownPreviousResponseIDDoesNotOverwriteHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr, store: store}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()

	// Establish a session with history
	code, resp := doResponsesWithHeader(t, srv, `{"input":"first message"}`, "protect-me")
	if code != http.StatusOK {
		t.Fatalf("setup status = %d, want 200", code)
	}
	respID, _ := resp["id"].(string)
	_ = respID

	// Verify messages were persisted
	msgs, err := store.GetMessages(context.Background(), "protect-me", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	originalCount := len(msgs)
	if originalCount < 2 {
		t.Fatalf("expected at least 2 persisted messages, got %d", originalCount)
	}

	// Now send a request with an unknown previous_response_id and same session_id.
	// This MUST fail and NOT overwrite history.
	code2, _ := doResponsesWithHeader(t, srv,
		`{"input":"bad","previous_response_id":"resp_bogus"}`, "protect-me")
	if code2 != http.StatusBadRequest {
		t.Fatalf("unknown previous_response_id status = %d, want 400", code2)
	}

	// Verify persisted history is unchanged
	msgs2, err := store.GetMessages(context.Background(), "protect-me", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after bad request: %v", err)
	}
	if len(msgs2) != originalCount {
		t.Fatalf("persisted messages changed: was %d, now %d", originalCount, len(msgs2))
	}
}

func TestHandleResponses_StalePreviousResponseIDReturnsConflict(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2", "reply3")
	defer srv.sessionMgr.Close()

	// Send two messages to create two response IDs
	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "stale-test")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)

	body2 := `{"input":"msg2","previous_response_id":"` + respID1 + `"}`
	code, resp2 := doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	_ = resp2["id"].(string) // respID2 is now the latest

	// Try to chain from the OLD response ID (respID1) — should fail
	body3 := `{"input":"msg3","previous_response_id":"` + respID1 + `"}`
	code, _ = doResponses(t, srv, body3)
	if code != http.StatusConflict {
		t.Fatalf("stale previous_response_id status = %d, want 409", code)
	}
}

func TestHandleResponses_PreviousResponseIDChainsSession(t *testing.T) {
	// Each runtime gets 2 text responses so it can handle 2 messages
	srv := newTestServeServer("first reply", "second reply")
	defer srv.sessionMgr.Close()

	// First request: no chaining
	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "sess-chain")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatalf("first response missing id")
	}

	// Second request: chain via previous_response_id
	body2 := `{"input":"msg2","previous_response_id":"` + respID1 + `"}`
	code, resp2 := doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatalf("second response missing id")
	}
	if respID1 == respID2 {
		t.Fatalf("response IDs should be unique, both are %q", respID1)
	}
}

func TestHandleResponses_NoPreviousResponseIDStartsFresh(t *testing.T) {
	// Each runtime gets 2 responses so it can handle being reused
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	// First request with session_id header, no previous_response_id
	code1, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "same-session")
	if code1 != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code1)
	}
	respID1, _ := resp1["id"].(string)

	// Second request with same session_id header but no previous_response_id.
	// Should start fresh (replaceHistory=true), clearing prior conversation.
	code2, resp2 := doResponsesWithHeader(t, srv, `{"input":"msg2"}`, "same-session")
	if code2 != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200; without previous_response_id should start fresh", code2)
	}

	// Both should succeed and have different response IDs
	respID2, _ := resp2["id"].(string)
	if respID1 == respID2 {
		t.Fatalf("response IDs should differ, both are %q", respID1)
	}

	// Verify that the runtime's history was reset: the second request should
	// NOT have seen the first request's messages. We check the mock provider's
	// recorded requests — the second request should have only the system prompt
	// (if any) + the new user message, not the accumulated history.
	rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "same-session")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	provider := rt.provider.(*llm.MockProvider)
	if len(provider.Requests) < 2 {
		t.Fatalf("expected 2 provider requests, got %d", len(provider.Requests))
	}
	secondReq := provider.Requests[1]
	// Count user messages in the second request — should be exactly 1 (fresh start)
	userMsgCount := 0
	for _, msg := range secondReq.Messages {
		if msg.Role == llm.RoleUser {
			userMsgCount++
		}
	}
	if userMsgCount != 1 {
		t.Fatalf("second request has %d user messages, want 1 (fresh start)", userMsgCount)
	}
}

func TestHandleResponses_CumulativeUsageGrows(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	// First request
	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "usage-test")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	su1, _ := resp1["session_usage"].(map[string]any)
	total1 := su1["total_tokens"].(float64)

	// Second request chained
	body2 := `{"input":"msg2","previous_response_id":"` + respID1 + `"}`
	code, resp2 := doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	su2, _ := resp2["session_usage"].(map[string]any)
	total2 := su2["total_tokens"].(float64)

	if total2 < total1 {
		t.Fatalf("cumulative session_usage should grow: first=%v, second=%v", total1, total2)
	}
}

func TestStreamResponses_IncludesResponseIDAndSessionUsage(t *testing.T) {
	srv := newTestServeServer("streamed response")
	defer srv.sessionMgr.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	// Parse SSE events to find response.completed
	var completedData map[string]any
	scanner := bufio.NewScanner(rr.Body)
	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if currentEvent == "response.completed" && data != "[DONE]" {
				if err := json.Unmarshal([]byte(data), &completedData); err != nil {
					t.Fatalf("parse response.completed data: %v", err)
				}
			}
		}
	}

	if completedData == nil {
		t.Fatalf("response.completed event not found in SSE stream")
	}

	response, ok := completedData["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.completed missing response object")
	}

	respID, _ := response["id"].(string)
	if !strings.HasPrefix(respID, "resp_") {
		t.Fatalf("streaming response id = %q, want resp_ prefix", respID)
	}

	sessionUsage, ok := response["session_usage"].(map[string]any)
	if !ok {
		t.Fatalf("streaming response missing session_usage")
	}
	if _, ok := sessionUsage["input_tokens"]; !ok {
		t.Fatalf("session_usage missing input_tokens")
	}
}

func TestStreamResponses_AskUserRoundTrip(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call-ask", tools.AskUserToolName, map[string]any{
		"questions": []map[string]any{{
			"header":   "Color",
			"question": "Pick a color",
			"options": []map[string]any{
				{"label": "Red", "description": "Warm"},
				{"label": "Blue", "description": "Cool"},
			},
		}},
	})
	provider.AddTextResponse("Thanks for answering")

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		engine.RegisterTool(tools.NewAskUserTool())
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}

	sessionID := "sess-ask-user"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", sessionID)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleResponses(rr, req)
		close(done)
	}()

	var runtime *serveRuntime
	pendingReady := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rt, ok := mgr.Get(sessionID)
		if ok {
			runtime = rt
			rt.askUserMu.Lock()
			_, pendingReady = rt.pendingAskUsers["call-ask"]
			rt.askUserMu.Unlock()
			if pendingReady {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runtime == nil || !pendingReady {
		t.Fatal("timed out waiting for pending ask_user prompt")
	}

	submitReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/ask_user",
		strings.NewReader(`{"call_id":"call-ask","answers":[{"selected":"Blue","is_custom":false}]}`))
	submitReq.Header.Set("Content-Type", "application/json")
	submitRR := httptest.NewRecorder()
	srv.handleSessionByID(submitRR, submitReq)
	if submitRR.Code != http.StatusOK {
		t.Fatalf("ask_user submit status = %d, body = %s", submitRR.Code, submitRR.Body.String())
	}

	var submitBody struct {
		Summary string                `json:"summary"`
		Answers []tools.AskUserAnswer `json:"answers"`
	}
	if err := json.Unmarshal(submitRR.Body.Bytes(), &submitBody); err != nil {
		t.Fatalf("decode ask_user submit response: %v", err)
	}
	if submitBody.Summary != "Color: Blue" {
		t.Fatalf("summary = %q, want %q", submitBody.Summary, "Color: Blue")
	}
	if len(submitBody.Answers) != 1 || submitBody.Answers[0].Selected != "Blue" {
		t.Fatalf("answers = %#v", submitBody.Answers)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for streaming response to finish")
	}

	if rr.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", rr.Code)
	}

	var sawPrompt bool
	var sawCompleted bool
	scanner := bufio.NewScanner(rr.Body)
	var currentEvent string
	var promptData serveAskUserPrompt
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		switch currentEvent {
		case "response.ask_user.prompt":
			sawPrompt = true
			if err := json.Unmarshal([]byte(data), &promptData); err != nil {
				t.Fatalf("decode ask_user prompt: %v", err)
			}
		case "response.completed":
			if data != "[DONE]" {
				sawCompleted = true
			}
		}
	}
	if !sawPrompt {
		t.Fatal("response.ask_user.prompt event not found")
	}
	if promptData.CallID != "call-ask" || len(promptData.Questions) != 1 || promptData.Questions[0].Header != "Color" {
		t.Fatalf("prompt data = %#v", promptData)
	}
	if !sawCompleted {
		t.Fatal("response.completed event not found")
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.Requests))
	}

	var toolResult *tools.AskUserResult
	for _, msg := range provider.Requests[1].Messages {
		for _, part := range msg.Parts {
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.Name != tools.AskUserToolName {
				continue
			}
			var parsed tools.AskUserResult
			if err := json.Unmarshal([]byte(part.ToolResult.Content), &parsed); err != nil {
				t.Fatalf("decode tool result content: %v", err)
			}
			toolResult = &parsed
		}
	}
	if toolResult == nil {
		t.Fatal("second provider request missing ask_user tool result")
	}
	if len(toolResult.Answers) != 1 || toolResult.Answers[0].Selected != "Blue" {
		t.Fatalf("tool result answers = %#v", toolResult.Answers)
	}
}

func TestResponsesStreamCanResumeAfterClientDisconnect(t *testing.T) {
	provider := newStagedProvider("hello ", "world")
	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManager(),
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()
	defer srv.responseRuns.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "resume-session")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var responseID string
	lastSeq := int64(0)
	for {
		eventName, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("stream ended before first text delta")
		}
		if data == "[DONE]" {
			t.Fatal("stream completed before disconnect")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal SSE payload: %v", err)
		}
		if seq, ok := payload["sequence_number"].(float64); ok {
			lastSeq = int64(seq)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			responseID, _ = response["id"].(string)
		case "response.output_text.delta":
			if got := payload["delta"]; got != "hello " {
				t.Fatalf("first delta = %v, want hello ", got)
			}
			cancelReq()
			_ = resp.Body.Close()
			goto disconnected
		}
	}

disconnected:
	if responseID == "" {
		t.Fatal("missing response id before disconnect")
	}

	<-provider.firstSent
	close(provider.releaseSecond)

	statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("get response status failed: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", statusResp.StatusCode)
	}
	var statusPayload map[string]any
	if err := json.NewDecoder(statusResp.Body).Decode(&statusPayload); err != nil {
		t.Fatalf("decode response status: %v", err)
	}
	if got := statusPayload["status"]; got != "in_progress" && got != "completed" {
		t.Fatalf("status = %v, want in_progress or completed", got)
	}

	resumeResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID + "/events?after=" + strconv.FormatInt(lastSeq, 10))
	if err != nil {
		t.Fatalf("resume request failed: %v", err)
	}
	defer resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d, want 200", resumeResp.StatusCode)
	}

	resumeScanner := bufio.NewScanner(resumeResp.Body)
	var resumed []string
	sawCompleted := false
	for {
		eventName, data, ok := readSSEEvent(t, resumeScanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal resumed SSE payload: %v", err)
		}
		switch eventName {
		case "response.output_text.delta":
			resumed = append(resumed, fmt.Sprint(payload["delta"]))
		case "response.completed":
			sawCompleted = true
		case "response.failed":
			t.Fatalf("resume stream failed: %s", data)
		}
	}

	if strings.Join(resumed, "") != "world" {
		t.Fatalf("resumed text = %q, want %q", strings.Join(resumed, ""), "world")
	}
	if !sawCompleted {
		t.Fatal("resume stream missing response.completed")
	}
}

func TestResponsesCompletedRunExpiresAfterRetention(t *testing.T) {
	provider := newStagedProvider("hello ", "world")
	close(provider.releaseSecond)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManagerWithRetention(100 * time.Millisecond),
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()
	defer srv.responseRuns.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "retention-session")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var responseID string
	sawCompleted := false
	for {
		eventName, data, ok := readSSEEvent(t, scanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal SSE payload: %v", err)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			responseID, _ = response["id"].(string)
		case "response.completed":
			sawCompleted = true
		}
	}

	if responseID == "" {
		t.Fatal("missing response id for completed run")
	}
	if !sawCompleted {
		t.Fatal("stream missing response.completed")
	}

	statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("get completed response failed: %v", err)
	}
	statusBody, _ := io.ReadAll(statusResp.Body)
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", statusResp.StatusCode, string(statusBody))
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		expireResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
		if err != nil {
			t.Fatalf("get expired response failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, expireResp.Body)
		expireResp.Body.Close()
		if expireResp.StatusCode == http.StatusNotFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("response %s still present after retention window; status=%d", responseID, expireResp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHandleResponseByID_CancelOnlySucceedsOnce(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	run := newResponseRun("resp_cancel_once", "cancel-session", "", "mock-model", time.Now().Unix(), cancel)
	mgr := newServeResponseRunManagerWithRetention(time.Minute)
	if err := mgr.create(run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	srv := &serveServer{
		responseRuns: mgr,
	}

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_cancel_once/cancel", nil)
	firstRR := httptest.NewRecorder()
	srv.handleResponseByID(firstRR, firstReq)
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first cancel status = %d, want 200", firstRR.Code)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_cancel_once/cancel", nil)
	secondRR := httptest.NewRecorder()
	srv.handleResponseByID(secondRR, secondReq)
	if secondRR.Code != http.StatusConflict {
		t.Fatalf("second cancel status = %d, want 409", secondRR.Code)
	}
}

func TestResponsesCompactedRunRequiresSnapshotRecovery(t *testing.T) {
	longText := strings.Repeat("abcdefghij", 300)
	provider := llm.NewMockProvider("mock").AddTextResponse(longText)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManager(),
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()
	defer srv.responseRuns.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "compaction-session")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}

	scanner := bufio.NewScanner(resp.Body)
	var responseID string
	for {
		eventName, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("stream ended before response.created")
		}
		if data == "[DONE]" {
			t.Fatal("stream ended before response.created")
		}
		if eventName != "response.created" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal response.created payload: %v", err)
		}
		response, _ := payload["response"].(map[string]any)
		responseID, _ = response["id"].(string)
		break
	}
	_ = resp.Body.Close()

	if responseID == "" {
		t.Fatal("missing response id")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
		if err != nil {
			t.Fatalf("get response status failed: %v", err)
		}
		var statusPayload map[string]any
		if err := json.NewDecoder(statusResp.Body).Decode(&statusPayload); err != nil {
			statusResp.Body.Close()
			t.Fatalf("decode response status: %v", err)
		}
		statusResp.Body.Close()
		if statusPayload["status"] == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("response %s did not complete in time", responseID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	replayResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID + "/events?after=0")
	if err != nil {
		t.Fatalf("replay request failed: %v", err)
	}
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(replayResp.Body)
		t.Fatalf("replay status = %d, want 409 (body=%s)", replayResp.StatusCode, string(body))
	}

	var replayErr map[string]any
	if err := json.NewDecoder(replayResp.Body).Decode(&replayErr); err != nil {
		t.Fatalf("decode replay error: %v", err)
	}
	if got := replayErr["snapshot_required"]; got != true {
		t.Fatalf("snapshot_required = %v, want true", got)
	}

	snapshotResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("snapshot request failed: %v", err)
	}
	defer snapshotResp.Body.Close()
	if snapshotResp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", snapshotResp.StatusCode)
	}

	var snapshotPayload map[string]any
	if err := json.NewDecoder(snapshotResp.Body).Decode(&snapshotPayload); err != nil {
		t.Fatalf("decode snapshot payload: %v", err)
	}
	recovery, _ := snapshotPayload["recovery"].(map[string]any)
	if recovery == nil {
		t.Fatal("missing recovery payload")
	}
	messages, _ := recovery["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("recovery message count = %d, want 1", len(messages))
	}
	message, _ := messages[0].(map[string]any)
	if got := message["role"]; got != "assistant" {
		t.Fatalf("recovery message role = %v, want assistant", got)
	}
	if got := message["content"]; got != longText {
		t.Fatalf("recovery message content length = %d, want %d", len(fmt.Sprint(got)), len(longText))
	}
}

func TestResponseToSessionMap_CleanedOnEviction(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(50*time.Millisecond, 100, factory)
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()

	// Create a session and get a response ID
	code, resp := doResponsesWithHeader(t, srv, `{"input":"hi"}`, "evict-test")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	respID, _ := resp["id"].(string)

	// Verify mapping exists
	if _, ok := srv.responseToSession.Load(respID); !ok {
		t.Fatalf("responseToSession should contain %q after request", respID)
	}

	// Wait for TTL expiry + janitor tick
	time.Sleep(200 * time.Millisecond)
	mgr.evictExpired()

	// Mapping should be cleaned up
	if _, ok := srv.responseToSession.Load(respID); ok {
		t.Fatalf("responseToSession should be cleaned up after eviction")
	}
}

func TestStreamResponses_EmitsInterjectionEvent(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_1", "slow_tool", map[string]any{})
	provider.AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&testServeDelayTool{delay: 20 * time.Millisecond})

	engine := llm.NewEngine(provider, registry)
	engine.Interject("keep sleeping")

	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	mgr := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "sess_interject")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: response.interjection") {
		t.Fatalf("expected response.interjection event in stream, got:\n%s", body)
	}
	if !strings.Contains(body, `"text":"keep sleeping"`) {
		t.Fatalf("expected interjection payload in stream, got:\n%s", body)
	}
}

func TestParsePreviousResponseID(t *testing.T) {
	var req responsesCreateRequest
	body := `{"input":"hello","previous_response_id":"resp_abc123"}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.PreviousResponseID != "resp_abc123" {
		t.Fatalf("previous_response_id = %q, want resp_abc123", req.PreviousResponseID)
	}
}

func TestServeRuntime_CumulativeUsageAccumulates(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddTurn(llm.MockTurn{Text: "a", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}).
		AddTurn(llm.MockTurn{Text: "b", Usage: llm.Usage{InputTokens: 20, OutputTokens: 8}})
	engine := llm.NewEngine(provider, nil)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{SessionID: "cumul-test", MaxTurns: 1}

	result1, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("first"),
	}, req)
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if result1.SessionUsage.InputTokens != 10 {
		t.Fatalf("session input tokens after 1st run = %d, want 10", result1.SessionUsage.InputTokens)
	}
	if result1.SessionUsage.OutputTokens != 5 {
		t.Fatalf("session output tokens after 1st run = %d, want 5", result1.SessionUsage.OutputTokens)
	}

	result2, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("second"),
	}, req)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if result2.SessionUsage.InputTokens != 30 {
		t.Fatalf("session input tokens after 2nd run = %d, want 30", result2.SessionUsage.InputTokens)
	}
	if result2.SessionUsage.OutputTokens != 13 {
		t.Fatalf("session output tokens after 2nd run = %d, want 13", result2.SessionUsage.OutputTokens)
	}
}

func TestServeRuntime_CumulativeUsageResetsOnFreshConversation(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddTurn(llm.MockTurn{Text: "a", Usage: llm.Usage{InputTokens: 100, OutputTokens: 50}}).
		AddTurn(llm.MockTurn{Text: "b", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}})
	engine := llm.NewEngine(provider, nil)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{SessionID: "reset-test", MaxTurns: 1}

	// First run accumulates usage
	result1, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("first"),
	}, req)
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if result1.SessionUsage.InputTokens != 100 {
		t.Fatalf("after run 1: session input = %d, want 100", result1.SessionUsage.InputTokens)
	}

	// Second run with replaceHistory=true should reset cumulative usage
	result2, err := rt.Run(context.Background(), true, true, []llm.Message{
		llm.UserText("fresh start"),
	}, req)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	// Should only reflect this run's usage, not accumulated
	if result2.SessionUsage.InputTokens != 10 {
		t.Fatalf("after fresh run: session input = %d, want 10 (reset)", result2.SessionUsage.InputTokens)
	}
	if result2.SessionUsage.OutputTokens != 5 {
		t.Fatalf("after fresh run: session output = %d, want 5 (reset)", result2.SessionUsage.OutputTokens)
	}
}

func TestRegisterResponseID_CapsAtMax(t *testing.T) {
	srv := &serveServer{}
	rt := &serveRuntime{}

	// Register more than maxResponseIDs
	for i := 0; i < maxResponseIDs+5; i++ {
		id := fmt.Sprintf("resp_%d", i)
		srv.registerResponseID(rt, id, "sess-1")
	}

	// Should be capped
	if got := len(rt.getResponseIDs()); got != maxResponseIDs {
		t.Fatalf("responseIDs len = %d, want %d", got, maxResponseIDs)
	}

	// First 5 IDs should be pruned from the map
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("resp_%d", i)
		if _, ok := srv.responseToSession.Load(id); ok {
			t.Fatalf("pruned ID %q still in responseToSession map", id)
		}
	}

	// Latest IDs should still be in the map
	for i := 5; i < maxResponseIDs+5; i++ {
		id := fmt.Sprintf("resp_%d", i)
		if _, ok := srv.responseToSession.Load(id); !ok {
			t.Fatalf("retained ID %q missing from responseToSession map", id)
		}
	}

	// lastResponseID should be the most recent
	expected := fmt.Sprintf("resp_%d", maxResponseIDs+4)
	if got := rt.getLastResponseID(); got != expected {
		t.Fatalf("lastResponseID = %q, want %q", got, expected)
	}
}

func TestServeSessionManager_Get_ExistingSession(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	// Create a session first.
	created, err := mgr.GetOrCreate(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Get should find it.
	got, ok := mgr.Get("sess-1")
	if !ok {
		t.Fatal("Get returned false for existing session")
	}
	if got != created {
		t.Fatal("Get returned different runtime than GetOrCreate")
	}
}

func TestServeSessionManager_Get_MissingSession(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		t.Fatal("factory should not be called")
		return nil, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	rt, ok := mgr.Get("nonexistent")
	if ok {
		t.Fatal("Get returned true for nonexistent session")
	}
	if rt != nil {
		t.Fatal("Get returned non-nil runtime for nonexistent session")
	}
}

func TestServeSessionManager_GetOrCreate_RespectsContextCancel(t *testing.T) {
	// Factory blocks until told to proceed.
	proceed := make(chan struct{})
	factory := func(ctx context.Context) (*serveRuntime, error) {
		<-proceed
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	// First call triggers factory (blocks).
	go func() {
		_, _ = mgr.GetOrCreate(context.Background(), "slow-sess")
	}()
	// Give time for in-flight to be registered.
	time.Sleep(20 * time.Millisecond)

	// Second call with a cancelled context should return immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mgr.GetOrCreate(ctx, "slow-sess")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Unblock factory so cleanup works.
	close(proceed)
}

func TestServeSessionManager_EvictionCallbackCleansResponseIDs(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(50*time.Millisecond, 100, factory)
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()

	rt, err := mgr.GetOrCreate(context.Background(), "evict-sess")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Simulate registering response IDs.
	srv.registerResponseID(rt, "resp_a", "evict-sess")
	srv.registerResponseID(rt, "resp_b", "evict-sess")

	// Verify mappings exist.
	if _, ok := srv.responseToSession.Load("resp_a"); !ok {
		t.Fatal("resp_a should exist before eviction")
	}
	if _, ok := srv.responseToSession.Load("resp_b"); !ok {
		t.Fatal("resp_b should exist before eviction")
	}

	// Wait for TTL and evict.
	time.Sleep(100 * time.Millisecond)
	mgr.evictExpired()

	// Mappings should be cleaned up.
	if _, ok := srv.responseToSession.Load("resp_a"); ok {
		t.Fatal("resp_a should be cleaned up after eviction")
	}
	if _, ok := srv.responseToSession.Load("resp_b"); ok {
		t.Fatal("resp_b should be cleaned up after eviction")
	}
}

func TestServeSessionManager_GetOrCreate_ConcurrentDedup(t *testing.T) {
	var factoryCalls atomic.Int32
	factory := func(ctx context.Context) (*serveRuntime, error) {
		factoryCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	const workers = 20
	runtimes := make([]*serveRuntime, workers)
	errs := make([]error, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			rt, err := mgr.GetOrCreate(context.Background(), "dedup-id")
			runtimes[idx] = rt
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d error: %v", i, err)
		}
	}

	// All workers should get the same runtime.
	first := runtimes[0]
	for i := 1; i < workers; i++ {
		if runtimes[i] != first {
			t.Fatalf("worker %d got different runtime pointer", i)
		}
	}

	// Factory should only have been called once.
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory called %d times, want 1", got)
	}
}

func TestHandlePushSubscribe(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	t.Run("POST saves subscription", func(t *testing.T) {
		srv := &serveServer{store: store}
		body := `{"endpoint":"https://push.example.com/sub1","keys":{"p256dh":"keydata","auth":"authdata"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
		}

		// Verify subscription was persisted
		subs, err := store.ListPushSubscriptions(context.Background())
		if err != nil {
			t.Fatalf("ListPushSubscriptions: %v", err)
		}
		if len(subs) != 1 {
			t.Fatalf("subscription count = %d, want 1", len(subs))
		}
		if subs[0].Endpoint != "https://push.example.com/sub1" {
			t.Fatalf("endpoint = %q, want %q", subs[0].Endpoint, "https://push.example.com/sub1")
		}
	})

	t.Run("DELETE removes subscription", func(t *testing.T) {
		srv := &serveServer{store: store}
		body := `{"endpoint":"https://push.example.com/sub1"}`
		req := httptest.NewRequest(http.MethodDelete, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
		}

		subs, err := store.ListPushSubscriptions(context.Background())
		if err != nil {
			t.Fatalf("ListPushSubscriptions: %v", err)
		}
		if len(subs) != 0 {
			t.Fatalf("subscription count = %d, want 0", len(subs))
		}
	})

	t.Run("POST missing fields returns 400", func(t *testing.T) {
		srv := &serveServer{store: store}
		body := `{"endpoint":"https://push.example.com/sub2"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("no store returns 503", func(t *testing.T) {
		srv := &serveServer{}
		body := `{"endpoint":"https://push.example.com/sub3","keys":{"p256dh":"k","auth":"a"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("auth required returns 401 without token", func(t *testing.T) {
		srv := &serveServer{
			store: store,
			cfg:   serveServerConfig{requireAuth: true, token: "secret"},
		}
		body := `{"endpoint":"https://push.example.com/sub4","keys":{"p256dh":"k","auth":"a"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.auth(srv.handlePushSubscribe)(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("GET not allowed", func(t *testing.T) {
		srv := &serveServer{store: store}
		req := httptest.NewRequest(http.MethodGet, "/v1/push/subscribe", nil)
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405; body: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestPushSubscribe_EndToEnd stores a subscription via the handler using
// base64url-encoded keys (matching real browser toJSON() output), then calls
// sendWebPush against a local httptest push server. This validates the full
// chain: handler -> DB -> webpush-go encrypt+send.
func TestPushSubscribe_EndToEnd(t *testing.T) {
	// Generate a real P-256 ECDH key pair (simulates the browser's key).
	browserKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate browser key: %v", err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(browserKey.PublicKey().Bytes())

	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	auth := base64.RawURLEncoding.EncodeToString(authSecret)

	// Mock push service that records whether it received a request.
	var pushReceived atomic.Bool
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushReceived.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushServer.Close()

	// Store via handler (same JSON shape as subscription.toJSON()).
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	srv := &serveServer{store: store}
	subBody := fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`,
		pushServer.URL, p256dh, auth)
	req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(subBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handlePushSubscribe(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("subscribe status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	// Read the stored subscription back
	subs, err := store.ListPushSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subscription count = %d, want 1", len(subs))
	}

	// Verify keys were stored in base64url (no +, /, or trailing =)
	for _, key := range []string{subs[0].KeyP256DH, subs[0].KeyAuth} {
		if strings.ContainsAny(key, "+/=") {
			t.Fatalf("stored key %q contains standard base64 characters; expected base64url", key)
		}
	}

	// Generate VAPID keys and send a push notification via sendWebPush.
	vapidPriv, vapidPub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate VAPID keys: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"title": "test", "body": "hello"})
	opts := &webpush.Options{
		VAPIDPublicKey:  vapidPub,
		VAPIDPrivateKey: vapidPriv,
		Subscriber:      "mailto:test@example.com",
		TTL:             30,
	}

	status, err := sendWebPush(context.Background(), &subs[0], payload, opts)
	if err != nil {
		t.Fatalf("sendWebPush error: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("push status = %d, want 201", status)
	}
	if !pushReceived.Load() {
		t.Fatal("mock push server never received a request")
	}
}
