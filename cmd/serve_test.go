package cmd

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/agents"
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

func TestServeServerStopIsIdempotent(t *testing.T) {
	s := &serveServer{
		server:     &http.Server{},
		shutdownCh: make(chan struct{}),
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop() error = %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
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

func TestParseSidebarSessionCategories(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		defaultAll bool
		want       []string
		wantErr    string
	}{
		{name: "default all", raw: "", defaultAll: true, want: []string{"all"}},
		{name: "empty allowed", raw: "", defaultAll: false, want: nil},
		{name: "dedup", raw: "chat, web, chat", defaultAll: true, want: []string{"chat", "web"}},
		{name: "all wins", raw: "chat,all,web", defaultAll: true, want: []string{"all"}},
		{name: "invalid", raw: "chat,nope", defaultAll: true, wantErr: "invalid --sidebar-sessions value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSidebarSessionCategories(tt.raw, tt.defaultAll)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("got %v, want %v", got, tt.want)
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
		wantFiles  string
	}{
		{"/ui", "/ui/", "/ui/images/", "/ui/files/"},
		{"/chat", "/chat/", "/chat/images/", "/chat/files/"},
		{"/app/v2", "/app/v2/", "/app/v2/images/", "/app/v2/files/"},
	}
	for _, tt := range tests {
		cfg := serveServerConfig{basePath: tt.basePath}
		if got := cfg.uiRoute(); got != tt.wantUI {
			t.Errorf("basePath=%q uiRoute()=%q, want %q", tt.basePath, got, tt.wantUI)
		}
		if got := cfg.imagesRoute(); got != tt.wantImages {
			t.Errorf("basePath=%q imagesRoute()=%q, want %q", tt.basePath, got, tt.wantImages)
		}
		if got := cfg.filesRoute(); got != tt.wantFiles {
			t.Errorf("basePath=%q filesRoute()=%q, want %q", tt.basePath, got, tt.wantFiles)
		}
	}
}

func TestCustomBasePath_EndToEnd(t *testing.T) {
	// Handlers are called with paths already stripped of basePath by
	// http.StripPrefix in the mux. So "/" is the SPA root, "/app.css" is
	// a static asset, and "/images/" is the images route.
	srv := &serveServer{
		cfg: serveServerConfig{
			ui:              true,
			basePath:        "/chat",
			sidebarSessions: []string{"chat", "web"},
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
	if !strings.Contains(body, `TERM_LLM_SIDEBAR_SESSIONS=["chat","web"]`) {
		t.Error("/ should inject TERM_LLM_SIDEBAR_SESSIONS")
	}
	if !strings.Contains(body, `TERM_LLM_UI_VERSION=`) {
		t.Error("/ should inject TERM_LLM_UI_VERSION")
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

func TestServeCORSExposesUIVersionHeader(t *testing.T) {
	srv := &serveServer{
		cfg: serveServerConfig{
			corsOrigins: []string{"https://example.com"},
		},
	}
	h := srv.cors(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()

	h(rr, req)

	if got := rr.Header().Get("X-Term-LLM-UI-Version"); got == "" {
		t.Fatal("X-Term-LLM-UI-Version header missing")
	}
	if got := rr.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(strings.ToLower(got), "x-term-llm-ui-version") {
		t.Fatalf("Access-Control-Expose-Headers = %q, want x-term-llm-ui-version", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "x-term-llm-ui-version") {
		t.Fatalf("Access-Control-Allow-Headers = %q, want x-term-llm-ui-version", got)
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

func TestHandleUI_StaticAssetCompressionAndConditionalCaching(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}
	version := serveui.AssetVersion()
	wantBody, err := serveui.StaticAsset("app.css")
	if err != nil {
		t.Fatalf("StaticAsset(app.css): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/app.css?v="+version, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("vary = %q, want Accept-Encoding", got)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("expected ETag on static asset")
	}
	zr, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	gotBody, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll gzip: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("Close gzip: %v", err)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("decompressed body mismatch")
	}

	req = httptest.NewRequest(http.MethodGet, "/app.css?v="+version, nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("plain status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("plain content-encoding = %q, want empty", got)
	}
	if !bytes.Equal(rr.Body.Bytes(), wantBody) {
		t.Fatalf("plain body mismatch")
	}

	req = httptest.NewRequest(http.MethodGet, "/app.css?v="+version, nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("conditional response body length = %d, want 0", rr.Body.Len())
	}

	req = httptest.NewRequest(http.MethodHead, "/app.css?v="+version, nil)
	rr = httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rr.Code)
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatalf("expected ETag on HEAD response")
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD body length = %d, want 0", rr.Body.Len())
	}
}

func TestHandleUI_ServiceWorkerCompressionKeepsNoCache(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/ui"}}

	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.handleUI(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache-control = %q, want no-cache", got)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", got)
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatalf("expected ETag on service worker")
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
		`.startup-splash {`,
		`@keyframes startup-spin {`,
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("expected %q in body", snippet)
		}
	}
	if strings.Index(body, `.startup-splash {`) > strings.Index(body, `href="app.css?v=`+version+`"`) {
		t.Fatalf("expected inline startup styles before app.css link")
	}
	for _, snippet := range []string{
		`src="vendor/katex/katex.min.js?v=0.16.38"`,
		`src="vendor/hljs/highlight.min.js?v=11.11.1"`,
		`href="vendor/katex/katex.min.css?v=0.16.38"`,
	} {
		if strings.Contains(body, snippet) {
			t.Fatalf("did not expect eager optional markdown asset %q in index", snippet)
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
	for _, snippet := range []string{
		`'./vendor/katex/katex.min.js?v=0.16.38'`,
		`'./vendor/hljs/highlight.min.js?v=11.11.1'`,
		`'./vendor/hljs/github-dark.min.css?v=11.11.1'`,
	} {
		if strings.Contains(body, snippet) {
			t.Fatalf("did not expect lazy optional asset %q in shell precache", snippet)
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

func TestParseResponsesInput_DeveloperDoesNotSuppressServerSystemPrompt(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"message","role":"developer","content":"Be concise"},
		{"type":"message","role":"user","content":"hello"}
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
	if msgs[0].Role != llm.RoleDeveloper {
		t.Fatalf("first role = %s, want developer", msgs[0].Role)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	rt := &serveRuntime{
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		systemPrompt: "server system prompt",
	}
	_, err = rt.Run(context.Background(), true, replaceHistory, msgs, llm.Request{})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(provider.Requests))
	}
	if len(provider.Requests[0].Messages) != 3 {
		t.Fatalf("request message count = %d, want 3", len(provider.Requests[0].Messages))
	}
	if provider.Requests[0].Messages[0].Role != llm.RoleSystem || provider.Requests[0].Messages[0].Parts[0].Text != "server system prompt" {
		t.Fatalf("first request message = %+v, want server system prompt", provider.Requests[0].Messages[0])
	}
	if provider.Requests[0].Messages[1].Role != llm.RoleDeveloper || provider.Requests[0].Messages[1].Parts[0].Text != "Be concise" {
		t.Fatalf("second request message = %+v, want developer prompt", provider.Requests[0].Messages[1])
	}
	if provider.Requests[0].Messages[2].Role != llm.RoleUser || provider.Requests[0].Messages[2].Parts[0].Text != "hello" {
		t.Fatalf("third request message = %+v, want user prompt", provider.Requests[0].Messages[2])
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
	if msg.Parts[0].ImagePath != "" {
		t.Fatalf("parts[0].ImagePath = %q, want empty for inline upload", msg.Parts[0].ImagePath)
	}
	uploadsDir := filepath.Join(dataHome, "term-llm", "uploads")
	if _, err := os.Stat(uploadsDir); !os.IsNotExist(err) {
		t.Fatalf("uploads dir stat err = %v, want not exist for inline image upload", err)
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

func TestParseResponsesInput_FunctionCallOutputDoesNotReplaceHistory(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"content"}
	]`)
	msgs, replaceHistory, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if replaceHistory {
		t.Fatalf("replaceHistory = true, want false")
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

func TestParseResponsesInput_FunctionCallHistoryWithUserReplacesHistory(t *testing.T) {
	payload := json.RawMessage(`[
		{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"content"},
		{"type":"message","role":"user","content":"what next?"}
	]`)
	msgs, replaceHistory, err := parseResponsesInput(payload)
	if err != nil {
		t.Fatalf("parseResponsesInput failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3", len(msgs))
	}
	if msgs[2].Role != llm.RoleUser {
		t.Fatalf("third role = %s, want user", msgs[2].Role)
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

func TestPopulateMissingToolResultNames_UsesHistory(t *testing.T) {
	messages, replaceHistory, err := parseChatMessages([]chatMessage{{
		Role:       "tool",
		ToolCallID: "call_1",
		Content:    json.RawMessage(`"done"`),
	}})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if replaceHistory {
		t.Fatalf("replaceHistory = true, want false for tool-only follow-up")
	}

	history := []llm.Message{{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type: llm.PartToolCall,
			ToolCall: &llm.ToolCall{
				ID:        "call_1",
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path":"a.txt"}`),
			},
		}},
	}}

	populateMissingToolResultNames(messages, history)

	if got := messages[0].Parts[0].ToolResult.Name; got != "read_file" {
		t.Fatalf("tool result name = %q, want %q", got, "read_file")
	}
}

func TestParseChatMessages_DeveloperDoesNotSuppressServerSystemPrompt(t *testing.T) {
	msgs, replaceHistory, err := parseChatMessages([]chatMessage{
		{Role: "developer", Content: json.RawMessage(`"Be concise"`)},
		{Role: "user", Content: json.RawMessage(`"hello"`)},
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
	if msgs[0].Role != llm.RoleDeveloper {
		t.Fatalf("first role = %s, want developer", msgs[0].Role)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	rt := &serveRuntime{
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		systemPrompt: "server system prompt",
	}
	_, err = rt.Run(context.Background(), true, replaceHistory, msgs, llm.Request{})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(provider.Requests))
	}
	if len(provider.Requests[0].Messages) != 3 {
		t.Fatalf("request message count = %d, want 3", len(provider.Requests[0].Messages))
	}
	if provider.Requests[0].Messages[0].Role != llm.RoleSystem || provider.Requests[0].Messages[0].Parts[0].Text != "server system prompt" {
		t.Fatalf("first request message = %+v, want server system prompt", provider.Requests[0].Messages[0])
	}
	if provider.Requests[0].Messages[1].Role != llm.RoleDeveloper || provider.Requests[0].Messages[1].Parts[0].Text != "Be concise" {
		t.Fatalf("second request message = %+v, want developer prompt", provider.Requests[0].Messages[1])
	}
	if provider.Requests[0].Messages[2].Role != llm.RoleUser || provider.Requests[0].Messages[2].Parts[0].Text != "hello" {
		t.Fatalf("third request message = %+v, want user prompt", provider.Requests[0].Messages[2])
	}
}

func TestParseChatMessages_UserImageContent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	msgs, replaceHistory, err := parseChatMessages([]chatMessage{{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}},
			{"type":"text","text":"describe this"}
		]`),
	}})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
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
	if len(msgs[0].Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msgs[0].Parts))
	}
	if msgs[0].Parts[0].Type != llm.PartImage {
		t.Fatalf("parts[0].type = %s, want image", msgs[0].Parts[0].Type)
	}
	if msgs[0].Parts[0].ImageData == nil || msgs[0].Parts[0].ImageData.MediaType != "image/png" || msgs[0].Parts[0].ImageData.Base64 != "aGVsbG8=" {
		t.Fatalf("parts[0].image = %#v, want png data URL", msgs[0].Parts[0].ImageData)
	}
	if msgs[0].Parts[1].Type != llm.PartText || msgs[0].Parts[1].Text != "describe this" {
		t.Fatalf("parts[1] = %+v, want trailing text", msgs[0].Parts[1])
	}
}

func TestParseChatMessages_AssistantImageContent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	msgs, replaceHistory, err := parseChatMessages([]chatMessage{{
		Role: "assistant",
		Content: json.RawMessage(`[
			{"type":"text","text":"looking"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
		]`),
	}})
	if err != nil {
		t.Fatalf("parseChatMessages failed: %v", err)
	}
	if !replaceHistory {
		t.Fatalf("replaceHistory = false, want true")
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleAssistant {
		t.Fatalf("role = %s, want assistant", msgs[0].Role)
	}
	if len(msgs[0].Parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(msgs[0].Parts))
	}
	if msgs[0].Parts[0].Type != llm.PartText || msgs[0].Parts[0].Text != "looking" {
		t.Fatalf("parts[0] = %+v, want leading text", msgs[0].Parts[0])
	}
	if msgs[0].Parts[1].Type != llm.PartImage {
		t.Fatalf("parts[1].type = %s, want image", msgs[0].Parts[1].Type)
	}
	if msgs[0].Parts[1].ImageData == nil || msgs[0].Parts[1].ImageData.MediaType != "image/png" || msgs[0].Parts[1].ImageData.Base64 != "aGVsbG8=" {
		t.Fatalf("parts[1].image = %#v, want png data URL", msgs[0].Parts[1].ImageData)
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

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "bearer secret")
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("lowercase bearer: status = %d, want 204", rr.Code)
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

func TestServeSessionManager_EvictExpiredSkipsActiveRun(t *testing.T) {
	manager := newServeSessionManager(10*time.Millisecond, 10, nil)
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt := &serveRuntime{}
	rt.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	state := &runtimeInterruptState{cancel: cancel, done: make(chan struct{})}
	rt.setActiveInterrupt(state)

	manager.mu.Lock()
	manager.sessions["busy"] = rt
	manager.mu.Unlock()

	manager.evictExpired()

	manager.mu.Lock()
	_, ok := manager.sessions["busy"]
	manager.mu.Unlock()
	if !ok {
		t.Fatal("expected active expired session to remain in manager")
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions after active session sweep = %d, want 0", got)
	}
	select {
	case <-ctx.Done():
		t.Fatal("expected active session not to be cancelled during eviction sweep")
	default:
	}

	rt.clearActiveInterrupt(state)
	manager.evictExpired()

	manager.mu.Lock()
	_, ok = manager.sessions["busy"]
	manager.mu.Unlock()
	if ok {
		t.Fatal("expected inactive expired session to be evicted")
	}
	if got := evictions.Load(); got != 1 {
		t.Fatalf("evictions after inactive session sweep = %d, want 1", got)
	}
}

func TestServeSessionManager_GetOrCreateSkipsEvictingActiveRunAtCapacity(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 2, func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	var evicted *serveRuntime
	manager.onEvict = func(rt *serveRuntime) {
		evicted = rt
	}

	busyCtx, busyCancel := context.WithCancel(context.Background())
	defer busyCancel()

	busy := &serveRuntime{}
	busy.lastUsedUnixNano.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	busyState := &runtimeInterruptState{cancel: busyCancel, done: make(chan struct{})}
	busy.setActiveInterrupt(busyState)

	idle := &serveRuntime{}
	idle.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())

	manager.mu.Lock()
	manager.sessions["busy"] = busy
	manager.sessions["idle"] = idle
	manager.mu.Unlock()

	created, err := manager.GetOrCreate(context.Background(), "new")
	if err != nil {
		t.Fatalf("GetOrCreate error: %v", err)
	}
	if created == nil {
		t.Fatal("expected created runtime")
	}

	manager.mu.Lock()
	_, busyOK := manager.sessions["busy"]
	_, idleOK := manager.sessions["idle"]
	_, newOK := manager.sessions["new"]
	sessionCount := len(manager.sessions)
	manager.mu.Unlock()

	if !busyOK {
		t.Fatal("expected active session to remain in manager")
	}
	if idleOK {
		t.Fatal("expected idle session to be evicted instead of active session")
	}
	if !newOK {
		t.Fatal("expected new session to be stored in manager")
	}
	if sessionCount != 2 {
		t.Fatalf("session count = %d, want 2", sessionCount)
	}
	if evicted != idle {
		t.Fatal("expected idle session to be evicted")
	}
	select {
	case <-busyCtx.Done():
		t.Fatal("expected active session not to be cancelled during capacity eviction")
	default:
	}
}

func TestServeSessionManager_GetOrCreateReturnsErrorWhenAllSessionsAreBusyAtCapacity(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 1, func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	busyCtx, busyCancel := context.WithCancel(context.Background())
	defer busyCancel()

	busy := &serveRuntime{}
	busy.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	busyState := &runtimeInterruptState{cancel: busyCancel, done: make(chan struct{})}
	busy.setActiveInterrupt(busyState)

	manager.mu.Lock()
	manager.sessions["busy"] = busy
	manager.mu.Unlock()

	created, err := manager.GetOrCreate(context.Background(), "new")
	if !errors.Is(err, errServeSessionLimitReached) {
		t.Fatalf("error = %v, want %v", err, errServeSessionLimitReached)
	}
	if created != nil {
		t.Fatal("expected no runtime to be created")
	}

	manager.mu.Lock()
	_, busyOK := manager.sessions["busy"]
	_, newOK := manager.sessions["new"]
	sessionCount := len(manager.sessions)
	manager.mu.Unlock()

	if !busyOK {
		t.Fatal("expected active session to remain in manager")
	}
	if newOK {
		t.Fatal("expected new session not to be stored when all sessions are busy")
	}
	if sessionCount != 1 {
		t.Fatalf("session count = %d, want 1", sessionCount)
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions = %d, want 0", got)
	}
	select {
	case <-busyCtx.Done():
		t.Fatal("expected active session not to be cancelled during capacity check")
	default:
	}
}

func TestServeSessionManager_GetOrCreateWithReturnsErrorWhenAllSessionsAreBusyAtCapacity(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 1, nil)
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	busyCtx, busyCancel := context.WithCancel(context.Background())
	defer busyCancel()

	busy := &serveRuntime{}
	busy.lastUsedUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	busyState := &runtimeInterruptState{cancel: busyCancel, done: make(chan struct{})}
	busy.setActiveInterrupt(busyState)

	manager.mu.Lock()
	manager.sessions["busy"] = busy
	manager.mu.Unlock()

	created, err := manager.GetOrCreateWith(context.Background(), "new", func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	if !errors.Is(err, errServeSessionLimitReached) {
		t.Fatalf("error = %v, want %v", err, errServeSessionLimitReached)
	}
	if created != nil {
		t.Fatal("expected no runtime to be created")
	}

	manager.mu.Lock()
	_, busyOK := manager.sessions["busy"]
	_, newOK := manager.sessions["new"]
	sessionCount := len(manager.sessions)
	manager.mu.Unlock()

	if !busyOK {
		t.Fatal("expected active session to remain in manager")
	}
	if newOK {
		t.Fatal("expected new session not to be stored when all sessions are busy")
	}
	if sessionCount != 1 {
		t.Fatalf("session count = %d, want 1", sessionCount)
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions = %d, want 0", got)
	}
	select {
	case <-busyCtx.Done():
		t.Fatal("expected active session not to be cancelled during capacity check")
	default:
	}
}

func TestServeSessionManager_ReplaceIdleWith_RestoresExistingSessionWhenCreateFails(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	var evictions atomic.Int32
	manager.onEvict = func(rt *serveRuntime) {
		evictions.Add(1)
	}

	existing := &serveRuntime{
		pendingApprovals: map[string]*servePendingApproval{
			"appr_1": {
				ApprovalID: "appr_1",
				Path:       "/tmp/file.txt",
				Options: []tools.ApprovalOption{{
					Label:  "Allow once",
					Choice: tools.ApprovalChoiceOnce,
				}},
				CreatedAt: time.Now(),
				responseC: make(chan serveApprovalSubmission, 1),
			},
		},
	}
	putTestSession(manager, "sess", existing)

	replaceErr := errors.New("replacement failed")
	got, err := manager.ReplaceIdleWith(
		context.Background(),
		"sess",
		func(existing *serveRuntime) bool { return true },
		func(ctx context.Context) (*serveRuntime, error) {
			return nil, replaceErr
		},
	)
	if !errors.Is(err, replaceErr) {
		t.Fatalf("ReplaceIdleWith error = %v, want %v", err, replaceErr)
	}
	if got != nil {
		t.Fatalf("ReplaceIdleWith runtime = %v, want nil", got)
	}

	restored, ok := manager.Get("sess")
	if !ok {
		t.Fatal("expected existing session to remain in manager after replacement failure")
	}
	if restored != existing {
		t.Fatal("expected existing runtime pointer to be preserved after replacement failure")
	}
	if prompts := existing.pendingApprovalPrompts(); len(prompts) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(prompts))
	}
	if got := evictions.Load(); got != 0 {
		t.Fatalf("evictions = %d, want 0", got)
	}
}

type serveSessionCloseTrackingProvider struct {
	closed atomic.Bool
}

func (p *serveSessionCloseTrackingProvider) Name() string       { return "close-tracking" }
func (p *serveSessionCloseTrackingProvider) Credential() string { return "test" }
func (p *serveSessionCloseTrackingProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}
func (p *serveSessionCloseTrackingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return &serveRuntimeTestStream{}, nil
}
func (p *serveSessionCloseTrackingProvider) CleanupMCP() { p.closed.Store(true) }

func newCloseTrackingServeRuntime() (*serveRuntime, *serveSessionCloseTrackingProvider) {
	provider := &serveSessionCloseTrackingProvider{}
	rt := &serveRuntime{provider: provider}
	rt.Touch()
	return rt, provider
}

func TestServeSessionManager_BeginSwapInstallsCandidateAndReturnsPrevious(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, _ := newCloseTrackingServeRuntime()
	candidate, _ := newCloseTrackingServeRuntime()
	putTestSession(manager, "swap", previous)

	gotCandidate, gotPrevious, commit, rollback, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		return candidate, nil
	})
	if err != nil {
		t.Fatalf("BeginSwap error: %v", err)
	}
	defer rollback()
	if gotCandidate != candidate {
		t.Fatalf("candidate = %p, want %p", gotCandidate, candidate)
	}
	if gotPrevious != previous {
		t.Fatalf("previous = %p, want %p", gotPrevious, previous)
	}
	current, ok := manager.Get("swap")
	if !ok || current != candidate {
		t.Fatalf("manager current = %p ok=%v, want candidate %p", current, ok, candidate)
	}
	if commit == nil || rollback == nil {
		t.Fatal("expected commit and rollback callbacks")
	}
}

func TestServeSessionManager_BeginSwapRollbackRestoresPreviousAndClosesCandidate(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, prevProvider := newCloseTrackingServeRuntime()
	candidate, candProvider := newCloseTrackingServeRuntime()
	putTestSession(manager, "swap", previous)

	_, _, _, rollback, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		return candidate, nil
	})
	if err != nil {
		t.Fatalf("BeginSwap error: %v", err)
	}
	rollback()
	current, ok := manager.Get("swap")
	if !ok || current != previous {
		t.Fatalf("manager current = %p ok=%v, want previous %p", current, ok, previous)
	}
	if !candProvider.closed.Load() {
		t.Fatal("expected rollback to close candidate")
	}
	if prevProvider.closed.Load() {
		t.Fatal("expected rollback to keep previous open")
	}
}

func TestServeSessionManager_BeginSwapCommitKeepsCandidateAndClosesPrevious(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, prevProvider := newCloseTrackingServeRuntime()
	candidate, candProvider := newCloseTrackingServeRuntime()
	putTestSession(manager, "swap", previous)

	_, _, commit, _, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		return candidate, nil
	})
	if err != nil {
		t.Fatalf("BeginSwap error: %v", err)
	}
	commit()
	current, ok := manager.Get("swap")
	if !ok || current != candidate {
		t.Fatalf("manager current = %p ok=%v, want candidate %p", current, ok, candidate)
	}
	if !prevProvider.closed.Load() {
		t.Fatal("expected commit to close previous")
	}
	if candProvider.closed.Load() {
		t.Fatal("expected commit to keep candidate open")
	}
}

func TestServeSessionManager_BeginSwapBusyPreviousReturnsBusy(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()

	previous, _ := newCloseTrackingServeRuntime()
	busyState := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	previous.setActiveInterrupt(busyState)
	defer previous.clearActiveInterrupt(busyState)
	putTestSession(manager, "swap", previous)

	created := false
	candidate, previousOut, commit, rollback, err := manager.BeginSwap(context.Background(), "swap", func(ctx context.Context) (*serveRuntime, error) {
		created = true
		rt, _ := newCloseTrackingServeRuntime()
		return rt, nil
	})
	if !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("BeginSwap error = %v, want %v", err, errServeSessionBusy)
	}
	if created {
		t.Fatal("factory should not be called for busy previous runtime")
	}
	if candidate != nil || previousOut != nil || commit != nil || rollback != nil {
		t.Fatalf("expected nil results on busy error")
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

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, "mock", "mock-model", true, wireSpawn, nil)
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

	engine, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, "mock", "mock-model", false, wireSpawn, nil)
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

func TestNewServeEngineWithTools_ConfiguresContextManagement(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "mock", Model: "serve-test-model", InputLimit: 1234}})
	defer llm.RegisterConfigLimits(nil)

	cfg := &config.Config{AutoCompact: true}
	provider := llm.NewMockProvider("mock")

	engine, _, err := newServeEngineWithTools(cfg, SessionSettings{}, provider, "mock", "serve-test-model", false, nil, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if got := engine.InputLimit(); got != 1234 {
		t.Fatalf("engine.InputLimit() = %d, want 1234", got)
	}

	compactionConfig := reflect.ValueOf(engine).Elem().FieldByName("compactionConfig")
	if !compactionConfig.IsValid() {
		t.Fatalf("compactionConfig field not found")
	}
	if compactionConfig.IsNil() {
		t.Fatalf("compactionConfig = nil, want enabled when auto_compact is true")
	}
}

func TestNewServeEngineWithTools_TracksContextWhenAutoCompactDisabled(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "mock", Model: "serve-test-model", InputLimit: 1234}})
	defer llm.RegisterConfigLimits(nil)

	cfg := &config.Config{AutoCompact: false}
	provider := llm.NewMockProvider("mock")

	engine, _, err := newServeEngineWithTools(cfg, SessionSettings{}, provider, "mock", "serve-test-model", false, nil, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if got := engine.InputLimit(); got != 1234 {
		t.Fatalf("engine.InputLimit() = %d, want 1234", got)
	}

	compactionConfig := reflect.ValueOf(engine).Elem().FieldByName("compactionConfig")
	if !compactionConfig.IsValid() {
		t.Fatalf("compactionConfig field not found")
	}
	if !compactionConfig.IsNil() {
		t.Fatalf("compactionConfig != nil, want tracking-only when auto_compact is false")
	}
}

func TestServeRuntimeRun_ReconfiguresContextManagementForRequestModel(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{
		{Provider: "mock", Model: "default-model", InputLimit: 1000},
		{Provider: "mock", Model: "override-model", InputLimit: 2000},
	})
	defer llm.RegisterConfigLimits(nil)

	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	engine := llm.NewEngine(provider, nil)
	engine.ConfigureContextManagement(provider, "mock", "default-model", false)

	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "default-model",
	}
	rt.Touch()

	_, err := rt.Run(context.Background(), false, false, []llm.Message{llm.UserText("hello")}, llm.Request{
		SessionID: "request-model-override",
		Model:     "override-model",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := engine.InputLimit(); got != 2000 {
		t.Fatalf("engine.InputLimit() = %d, want 2000 after request model override", got)
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

func TestServeRuntimeRun_PersistsPendingInterjectionAtEndOfSimpleStream(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	provider := newStagedProvider("hello ", "world")
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "staged",
		engine:       engine,
		store:        store,
		defaultModel: "staged-model",
	}
	rt.Touch()

	errCh := make(chan error, 1)
	go func() {
		_, runErr := rt.Run(context.Background(), true, false, []llm.Message{
			llm.UserText("original request"),
		}, llm.Request{SessionID: "serve-interject-persist", MaxTurns: 3})
		errCh <- runErr
	}()

	select {
	case <-provider.firstSent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first streamed chunk")
	}

	action, err := rt.Interrupt(context.Background(), "also remember this", nil)
	if err != nil {
		t.Fatalf("Interrupt failed: %v", err)
	}
	if action != llm.InterruptInterject {
		t.Fatalf("Interrupt action = %v, want %v", action, llm.InterruptInterject)
	}

	close(provider.releaseSecond)

	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("Run failed: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to finish")
	}

	if len(rt.history) != 3 {
		t.Fatalf("history len = %d, want 3", len(rt.history))
	}
	if rt.history[2].Role != llm.RoleUser {
		t.Fatalf("last history role = %s, want user", rt.history[2].Role)
	}
	if got := rt.history[2].Parts[0].Text; got != "also remember this" {
		t.Fatalf("last history text = %q, want %q", got, "also remember this")
	}

	msgs, err := store.GetMessages(context.Background(), "serve-interject-persist", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("persisted message count = %d, want 3", len(msgs))
	}
	if msgs[2].Role != llm.RoleUser {
		t.Fatalf("last persisted role = %s, want user", msgs[2].Role)
	}
	if msgs[2].TextContent != "also remember this" {
		t.Fatalf("last persisted text = %q, want %q", msgs[2].TextContent, "also remember this")
	}
}

func TestServeRuntimeRun_PersistsMessagesOnErrorExit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// Turn 1: tool call (succeeds, callbacks persist).
	// Turn 2: error (run returns error — deferred persist must save turn 1).
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddError(errors.New("provider exploded"))

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
		SessionID: "error-persist-test",
		MaxTurns:  5,
		Tools:     []llm.ToolSpec{(&echoTool{}).Spec()},
	}
	_, runErr := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("call the echo tool"),
	}, req)
	if runErr == nil {
		t.Fatal("expected Run to return an error")
	}

	// Despite the error, turn 1 messages must be persisted.
	msgs, err := store.GetMessages(context.Background(), "error-persist-test", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}

	var hasToolCall, hasToolResult bool
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
		t.Fatalf("persisted messages missing assistant tool_call after error exit; messages: %d", len(msgs))
	}
	if !hasToolResult {
		t.Fatalf("persisted messages missing tool_result after error exit; messages: %d", len(msgs))
	}
}

func TestServeRuntimeRun_ImmediateErrorDoesNotDesyncState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// The provider fails on the very first call — no callbacks fire.
	provider := llm.NewMockProvider("mock").
		AddError(errors.New("immediate boom")).
		AddTextResponse("recovered")

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

	sid := "immediate-error-test"
	req := llm.Request{
		SessionID: sid,
		MaxTurns:  5,
		Tools:     []llm.ToolSpec{(&echoTool{}).Spec()},
	}

	// First run: immediate error, no produced messages.
	_, runErr := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("hello"),
	}, req)
	if runErr == nil {
		t.Fatal("expected Run to return an error")
	}

	// rt.history must be empty — no callbacks ran, no state committed.
	if len(rt.history) != 0 {
		t.Fatalf("history len after immediate error = %d, want 0", len(rt.history))
	}

	// DB should have no messages for this session (no eager persist without callback).
	msgs, err := store.GetMessages(context.Background(), sid, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("persisted message count after immediate error = %d, want 0", len(msgs))
	}

	// Second run: succeeds — must not be confused by stale DB state.
	_, runErr = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("hello again"),
	}, req)
	if runErr != nil {
		t.Fatalf("second Run failed: %v", runErr)
	}

	msgs, err = store.GetMessages(context.Background(), sid, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after recovery failed: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected persisted messages after successful second run")
	}
}

func TestServeRuntimeRun_ReplaceHistoryClearsInMemoryHistoryOnEarlyFailure(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddTextResponse("old reply").
		AddError(errors.New("fresh-start boom")).
		AddTextResponse("recovered")
	engine := llm.NewEngine(provider, nil)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{SessionID: "replace-history-early-failure", MaxTurns: 1}

	_, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("old context"),
	}, req)
	if err != nil {
		t.Fatalf("first Run failed: %v", err)
	}
	if len(rt.history) == 0 {
		t.Fatal("expected first Run to populate history")
	}

	_, err = rt.Run(context.Background(), true, true, []llm.Message{
		llm.UserText("fresh start"),
	}, req)
	if err == nil || !strings.Contains(err.Error(), "fresh-start boom") {
		t.Fatalf("replaceHistory Run error = %v, want fresh-start boom", err)
	}
	if len(rt.history) != 0 {
		t.Fatalf("history len after failed replaceHistory run = %d, want 0", len(rt.history))
	}

	_, err = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("after failure"),
	}, req)
	if err != nil {
		t.Fatalf("third Run failed: %v", err)
	}
	if len(provider.Requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(provider.Requests))
	}
	if len(provider.Requests[2].Messages) != 1 {
		t.Fatalf("third request message count = %d, want 1", len(provider.Requests[2].Messages))
	}
	if provider.Requests[2].Messages[0].Role != llm.RoleUser {
		t.Fatalf("third request role = %s, want user", provider.Requests[2].Messages[0].Role)
	}
	if got := provider.Requests[2].Messages[0].Parts[0].Text; got != "after failure" {
		t.Fatalf("third request text = %q, want %q", got, "after failure")
	}
}

func TestServeRuntimeRun_ReinjectsPlatformDeveloperMessageAfterFailedFirstRun(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddError(errors.New("boom")).
		AddTextResponse("hello from serve")
	engine := llm.NewEngine(provider, nil)
	devText := "telegram developer instructions"
	input := []llm.Message{llm.UserText("hello")}

	rt := &serveRuntime{
		provider:         provider,
		engine:           engine,
		platform:         "telegram",
		platformMessages: agents.PlatformMessagesConfig{Telegram: devText},
	}
	rt.Touch()

	_, err := rt.Run(context.Background(), true, false, input, llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("first Run error = %v, want boom", err)
	}
	if rt.lastInjectedPlatform != "" {
		t.Fatalf("lastInjectedPlatform = %q, want empty after failed first run", rt.lastInjectedPlatform)
	}
	if len(rt.history) != 0 {
		t.Fatalf("history len = %d, want 0 after failed first run", len(rt.history))
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("request count after first run = %d, want 1", len(provider.Requests))
	}
	if len(provider.Requests[0].Messages) != 2 {
		t.Fatalf("first request message count = %d, want 2", len(provider.Requests[0].Messages))
	}
	if provider.Requests[0].Messages[0].Role != llm.RoleDeveloper || provider.Requests[0].Messages[0].Parts[0].Text != devText {
		t.Fatalf("first request did not include injected developer message: %+v", provider.Requests[0].Messages)
	}

	_, err = rt.Run(context.Background(), true, false, input, llm.Request{})
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}
	if rt.lastInjectedPlatform != "telegram" {
		t.Fatalf("lastInjectedPlatform = %q, want telegram after successful run", rt.lastInjectedPlatform)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("request count after second run = %d, want 2", len(provider.Requests))
	}
	if len(provider.Requests[1].Messages) != 2 {
		t.Fatalf("second request message count = %d, want 2", len(provider.Requests[1].Messages))
	}
	if provider.Requests[1].Messages[0].Role != llm.RoleDeveloper || provider.Requests[1].Messages[0].Parts[0].Text != devText {
		t.Fatalf("second request did not re-include injected developer message: %+v", provider.Requests[1].Messages)
	}
	if len(rt.history) < 3 {
		t.Fatalf("history len = %d, want at least 3 after successful run", len(rt.history))
	}
	if rt.history[0].Role != llm.RoleDeveloper || rt.history[0].Parts[0].Text != devText {
		t.Fatalf("history missing injected developer message: %+v", rt.history)
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

func TestStreamUIResponses_SetsSessionNumberHeader(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("hi")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{
		sessionMgr:   manager,
		store:        store,
		responseRuns: newServeResponseRunManager(),
	}

	body := `{"stream":true,"input":[{"type":"message","role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI", "1")
	req.Header.Set("session_id", "sess_test_number")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	numStr := strings.TrimSpace(rr.Header().Get("x-session-number"))
	if numStr == "" {
		t.Fatalf("x-session-number header missing from streaming UI response")
	}
	num, parseErr := strconv.ParseInt(numStr, 10, 64)
	if parseErr != nil || num <= 0 {
		t.Fatalf("x-session-number = %q, want positive integer", numStr)
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
	if err := os.MkdirAll(filepath.Join(dir, "benchmarks", "go"), 0755); err != nil {
		t.Fatalf("mkdir nested image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "benchmarks", "go", "board.png"), []byte("nested-png"), 0644); err != nil {
		t.Fatalf("write nested image: %v", err)
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

	// Nested file
	req = httptest.NewRequest(http.MethodGet, "/images/benchmarks/go/board.png", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nested file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "nested-png" {
		t.Fatalf("nested body = %q, want %q", got, "nested-png")
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

func TestEnsureImageServeable_RejectsExternalFile(t *testing.T) {
	outputDir := t.TempDir()
	externalDir := t.TempDir()

	// Create a file outside the image output directory.
	externalImg := filepath.Join(externalDir, "photo.png")
	if err := os.WriteFile(externalImg, []byte("external-image-data"), 0644); err != nil {
		t.Fatalf("write external image: %v", err)
	}

	// Create a file already inside the output directory.
	internalImg := filepath.Join(outputDir, "generated.png")
	if err := os.WriteFile(internalImg, []byte("internal-image-data"), 0644); err != nil {
		t.Fatalf("write internal image: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui"}, cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = outputDir

	// External files should be rejected instead of republished.
	if result, ok := srv.ensureImageServeable(externalImg); ok || result != "" {
		t.Fatalf("ensureImageServeable should reject external image, got result=%q ok=%v", result, ok)
	}

	// Internal file should be returned as an absolute, serveable path.
	result, ok := srv.ensureImageServeable(internalImg)
	if !ok {
		t.Fatal("ensureImageServeable should succeed for internal image")
	}
	absInternalImg, err := filepath.EvalSymlinks(internalImg)
	if err != nil {
		t.Fatalf("resolve internal image: %v", err)
	}
	if result != absInternalImg {
		t.Fatalf("internal image should be unchanged apart from canonicalization, got %q want %q", result, absInternalImg)
	}

	// Verify the internal file is actually serveable via handleImage.
	req := httptest.NewRequest(http.MethodGet, "/images/"+filepath.Base(result), nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("serve internal image: status = %d, want 200", rr.Code)
	}
}

func TestEnsureImageServeable_CopiesFromWriteDir(t *testing.T) {
	outputDir := t.TempDir()
	writeDir := t.TempDir()
	externalDir := t.TempDir()

	writeDirImg := filepath.Join(writeDir, "tool-output.png")
	if err := os.WriteFile(writeDirImg, []byte("tool-png"), 0644); err != nil {
		t.Fatalf("write writeDir image: %v", err)
	}
	externalImg := filepath.Join(externalDir, "secret.png")
	if err := os.WriteFile(externalImg, []byte("secret-png"), 0644); err != nil {
		t.Fatalf("write external image: %v", err)
	}

	srv := &serveServer{
		cfg: serveServerConfig{
			basePath:  "/ui",
			writeDirs: []string{writeDir},
		},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	result, ok := srv.ensureImageServeable(writeDirImg)
	if !ok {
		t.Fatal("ensureImageServeable should accept images from configured writeDirs")
	}
	if result == writeDirImg {
		t.Fatal("writeDir image should have been copied into imageOutputDir")
	}
	absResult, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("resolve copied image: %v", err)
	}
	absOutputDir, err := filepath.EvalSymlinks(outputDir)
	if err != nil {
		t.Fatalf("resolve output dir: %v", err)
	}
	if !strings.HasPrefix(absResult, absOutputDir+string(filepath.Separator)) {
		t.Fatalf("copied image %q should be under output dir %q", absResult, absOutputDir)
	}

	// The new copy should round-trip through handleImage.
	req := httptest.NewRequest(http.MethodGet, "/images/"+filepath.Base(result), nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("serve copied image: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "tool-png" {
		t.Fatalf("body = %q, want %q", got, "tool-png")
	}

	if _, ok := srv.ensureImageServeable(externalImg); ok {
		t.Fatal("images outside all approved dirs must still be rejected")
	}
}

func TestHandleFile_ServesFileAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "video.mp4"), []byte("fake-video"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "qwen36-go-benchmark"), 0755); err != nil {
		t.Fatalf("mkdir nested file dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen36-go-benchmark", "board.html"), []byte("<html>nested-board</html>"), 0644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	// index.html must be served verbatim — http.ServeFile would otherwise
	// 301-redirect any URL ending in /index.html to "./", which then hits
	// the SPA catch-all and returns the app shell.
	indexHTML := "<html>served-file-index</html>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen36-go-benchmark", "index.html"), []byte("<html>nested-index</html>"), 0644); err != nil {
		t.Fatalf("write nested index.html: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui", filesDir: dir}}

	// Valid file
	req := httptest.NewRequest(http.MethodGet, "/files/video.mp4", nil)
	rr := httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "fake-video" {
		t.Fatalf("body = %q, want %q", got, "fake-video")
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control = %q, want 'private'", cc)
	}

	// Nested file
	req = httptest.NewRequest(http.MethodGet, "/files/qwen36-go-benchmark/board.html", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nested file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "<html>nested-board</html>" {
		t.Fatalf("nested body = %q, want %q", got, "<html>nested-board</html>")
	}

	// index.html at the root of files-dir — must not redirect to "./".
	req = httptest.NewRequest(http.MethodGet, "/files/index.html", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("index.html: status = %d, want 200 (body=%q, location=%q)", rr.Code, rr.Body.String(), rr.Header().Get("Location"))
	}
	if got := rr.Body.String(); got != indexHTML {
		t.Fatalf("index.html body = %q, want %q", got, indexHTML)
	}

	// Nested index.html — same redirect trap.
	req = httptest.NewRequest(http.MethodGet, "/files/qwen36-go-benchmark/index.html", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nested index.html: status = %d, want 200 (location=%q)", rr.Code, rr.Header().Get("Location"))
	}
	if got := rr.Body.String(); got != "<html>nested-index</html>" {
		t.Fatalf("nested index.html body = %q, want %q", got, "<html>nested-index</html>")
	}

	// Path traversal
	req = httptest.NewRequest(http.MethodGet, "/files/..%2Fetc%2Fpasswd", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("traversal: status = %d, want 404", rr.Code)
	}

	// Empty filename
	req = httptest.NewRequest(http.MethodGet, "/files/", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("empty: status = %d, want 404", rr.Code)
	}

	// No files-dir configured → 404
	srv2 := &serveServer{cfg: serveServerConfig{basePath: "/ui"}}
	req = httptest.NewRequest(http.MethodGet, "/files/video.mp4", nil)
	rr = httptest.NewRecorder()
	srv2.handleFile(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("no files-dir: status = %d, want 404", rr.Code)
	}
}

func TestServeRoutePath_PreservesNestedRelativePaths(t *testing.T) {
	baseDir := t.TempDir()
	servedPath := filepath.Join(baseDir, "qwen36-go-benchmark", "board.html")
	if err := os.MkdirAll(filepath.Dir(servedPath), 0755); err != nil {
		t.Fatalf("mkdir served path dir: %v", err)
	}
	if err := os.WriteFile(servedPath, []byte("ok"), 0644); err != nil {
		t.Fatalf("write served path file: %v", err)
	}

	got := serveRoutePath("/chat/files/", baseDir, servedPath)
	want := "/chat/files/qwen36-go-benchmark/board.html"
	if got != want {
		t.Fatalf("serveRoutePath() = %q, want %q", got, want)
	}
}

func TestEnsureFileServeable_RejectsExternalFile(t *testing.T) {
	filesDir := t.TempDir()
	externalDir := t.TempDir()

	externalFile := filepath.Join(externalDir, "report.pdf")
	if err := os.WriteFile(externalFile, []byte("pdf-data"), 0644); err != nil {
		t.Fatalf("write external file: %v", err)
	}

	internalFile := filepath.Join(filesDir, "existing.mp4")
	if err := os.WriteFile(internalFile, []byte("video-data"), 0644); err != nil {
		t.Fatalf("write internal file: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui", filesDir: filesDir}}

	// External files should be rejected instead of republished.
	if result, ok := srv.ensureFileServeable(externalFile); ok || result != "" {
		t.Fatalf("ensureFileServeable should reject external file, got result=%q ok=%v", result, ok)
	}

	// Internal file should be returned as an absolute, serveable path.
	result, ok := srv.ensureFileServeable(internalFile)
	if !ok {
		t.Fatal("ensureFileServeable should succeed for internal file")
	}
	absInternalFile, err := filepath.EvalSymlinks(internalFile)
	if err != nil {
		t.Fatalf("resolve internal file: %v", err)
	}
	if result != absInternalFile {
		t.Fatalf("internal file should be unchanged apart from canonicalization, got %q want %q", result, absInternalFile)
	}

	// No files-dir → fails.
	srv2 := &serveServer{cfg: serveServerConfig{basePath: "/ui"}}
	_, ok = srv2.ensureFileServeable(externalFile)
	if ok {
		t.Fatal("ensureFileServeable should fail when filesDir is empty")
	}
}

func TestEnsureFileServeable_CopiesFromImageOutputDir(t *testing.T) {
	filesDir := t.TempDir()
	outputDir := t.TempDir()
	generatedImg := filepath.Join(outputDir, "generated.png")
	if err := os.WriteFile(generatedImg, []byte("image-data"), 0644); err != nil {
		t.Fatalf("write generated image: %v", err)
	}

	srv := &serveServer{
		cfg:    serveServerConfig{basePath: "/ui", filesDir: filesDir},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	result, ok := srv.ensureFileServeable(generatedImg)
	if !ok {
		t.Fatal("ensureFileServeable should allow files from image output dir")
	}
	if result == generatedImg {
		t.Fatal("image output file should have been copied into filesDir")
	}
	absResult, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("resolve copied file: %v", err)
	}
	absFilesDir, err := filepath.EvalSymlinks(filesDir)
	if err != nil {
		t.Fatalf("resolve files dir: %v", err)
	}
	if !strings.HasPrefix(absResult, absFilesDir+string(filepath.Separator)) {
		t.Fatalf("copied file %q should be under files dir %q", absResult, absFilesDir)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "image-data" {
		t.Fatalf("copied data = %q, want %q", string(data), "image-data")
	}
}

func TestEnsureFileServeable_CopiesFromWriteDir(t *testing.T) {
	filesDir := t.TempDir()
	writeDir := t.TempDir()
	externalDir := t.TempDir()

	toolOutput := filepath.Join(writeDir, "tool-output.pdf")
	if err := os.WriteFile(toolOutput, []byte("pdf-data"), 0644); err != nil {
		t.Fatalf("write tool output: %v", err)
	}
	external := filepath.Join(externalDir, "secret.txt")
	if err := os.WriteFile(external, []byte("secret"), 0644); err != nil {
		t.Fatalf("write external: %v", err)
	}

	srv := &serveServer{
		cfg: serveServerConfig{
			basePath:  "/ui",
			filesDir:  filesDir,
			writeDirs: []string{writeDir},
		},
		cfgRef: &config.Config{},
	}

	result, ok := srv.ensureFileServeable(toolOutput)
	if !ok {
		t.Fatal("ensureFileServeable should allow files from configured writeDirs")
	}
	if result == toolOutput {
		t.Fatal("writeDir source should have been copied into filesDir")
	}
	absResult, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("resolve copied file: %v", err)
	}
	absFilesDir, err := filepath.EvalSymlinks(filesDir)
	if err != nil {
		t.Fatalf("resolve files dir: %v", err)
	}
	if !strings.HasPrefix(absResult, absFilesDir+string(filepath.Separator)) {
		t.Fatalf("copied file %q should be under files dir %q", absResult, absFilesDir)
	}

	if _, ok := srv.ensureFileServeable(external); ok {
		t.Fatal("paths outside all approved dirs must still be rejected")
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
			ID            string `json:"id"`
			ShortTitle    string `json:"short_title"`
			LongTitle     string `json:"long_title"`
			CreatedAt     int64  `json:"created_at"`
			LastMessageAt int64  `json:"last_message_at"`
			MsgCount      int    `json:"message_count"`
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
	if body.Sessions[0].ShortTitle != "hello world" {
		t.Fatalf("short_title = %q, want %q", body.Sessions[0].ShortTitle, "hello world")
	}
	if body.Sessions[0].LastMessageAt == 0 {
		t.Fatalf("last_message_at = 0, want non-zero (falling back to created_at)")
	}
	if body.Sessions[0].LastMessageAt != body.Sessions[0].CreatedAt {
		t.Fatalf("last_message_at = %d, want %d (fallback to created_at when no messages)", body.Sessions[0].LastMessageAt, body.Sessions[0].CreatedAt)
	}
}

func TestHandleSessions_FiltersByCategoriesAndArchived(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	sessions := []*session.Session{
		{
			ID:        "sess-tui",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeChat,
			Origin:    session.OriginTUI,
			Pinned:    true,
			Summary:   "tui chat",
			CreatedAt: now,
			UpdatedAt: now,
			Status:    session.StatusActive,
		},
		{
			ID:        "sess-web",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeChat,
			Origin:    session.OriginWeb,
			Summary:   "web chat",
			CreatedAt: now.Add(time.Second),
			UpdatedAt: now.Add(time.Second),
			Status:    session.StatusActive,
		},
		{
			ID:        "sess-ask",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeAsk,
			Origin:    session.OriginTUI,
			Summary:   "ask run",
			CreatedAt: now.Add(2 * time.Second),
			UpdatedAt: now.Add(2 * time.Second),
			Status:    session.StatusActive,
		},
		{
			ID:        "sess-hidden",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeChat,
			Origin:    session.OriginWeb,
			Summary:   "hidden web chat",
			CreatedAt: now.Add(3 * time.Second),
			UpdatedAt: now.Add(3 * time.Second),
			Status:    session.StatusActive,
			Archived:  true,
		},
	}
	for _, sess := range sessions {
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s): %v", sess.ID, err)
		}
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions?categories=chat,web", nil)
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Sessions []struct {
			ID       string                `json:"id"`
			Mode     session.SessionMode   `json:"mode"`
			Origin   session.SessionOrigin `json:"origin"`
			Archived bool                  `json:"archived"`
			Pinned   bool                  `json:"pinned"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("session count = %d, want 2", len(body.Sessions))
	}
	if body.Sessions[0].ID != "sess-tui" || !body.Sessions[0].Pinned {
		t.Fatalf("first session = %+v, want pinned sess-tui first", body.Sessions[0])
	}
	gotIDs := []string{body.Sessions[0].ID, body.Sessions[1].ID}
	sort.Strings(gotIDs)
	if strings.Join(gotIDs, ",") != "sess-tui,sess-web" {
		t.Fatalf("ids = %v, want [sess-tui sess-web]", gotIDs)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sessions?categories=web&include_archived=1", nil)
	rr = httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("include_archived status = %d, want 200", rr.Code)
	}
	body = struct {
		Sessions []struct {
			ID       string                `json:"id"`
			Mode     session.SessionMode   `json:"mode"`
			Origin   session.SessionOrigin `json:"origin"`
			Archived bool                  `json:"archived"`
			Pinned   bool                  `json:"pinned"`
		} `json:"sessions"`
	}{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode include_archived: %v", err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("include_archived session count = %d, want 2", len(body.Sessions))
	}
}

func TestHandleSessionByID_PatchRenameAndArchive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-rename",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		Summary:   "hello world",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	srv := &serveServer{store: store}
	body := strings.NewReader(`{"name":"Renamed session","archived":true,"pinned":true}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/sess-rename", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}

	updated, err := store.Get(ctx, "sess-rename")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Name != "Renamed session" {
		t.Fatalf("Name = %q, want %q", updated.Name, "Renamed session")
	}
	if !updated.Archived {
		t.Fatal("Archived = false, want true")
	}
	if !updated.Pinned {
		t.Fatal("Pinned = false, want true")
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

func TestHandleSessionMessages_OmitsSystemAndDeveloperMessages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-sys", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Add a system message (as persisted by TUI chat sessions)
	sysMsg := session.NewMessage("sess-sys", llm.SystemText("You are a helpful assistant."), -1)
	if err := store.AddMessage(ctx, "sess-sys", sysMsg); err != nil {
		t.Fatalf("AddMessage(system): %v", err)
	}
	// Add a developer message (platform developer prompt, persisted with role=developer)
	devMsg := session.NewMessage("sess-sys", llm.Message{
		Role:  llm.RoleDeveloper,
		Parts: []llm.Part{{Type: llm.PartText, Text: "You are Jarvis, a personal AI assistant."}},
	}, -1)
	if err := store.AddMessage(ctx, "sess-sys", devMsg); err != nil {
		t.Fatalf("AddMessage(developer): %v", err)
	}
	// Add a user message
	userMsg := session.NewMessage("sess-sys", llm.UserText("hello"), -1)
	if err := store.AddMessage(ctx, "sess-sys", userMsg); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	// Add an assistant message
	assistantMsg := session.NewMessage("sess-sys", llm.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartText, Text: "hi there"}},
	}, -1)
	if err := store.AddMessage(ctx, "sess-sys", assistantMsg); err != nil {
		t.Fatalf("AddMessage(assistant): %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-sys/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (system+developer should be filtered)", len(body.Messages))
	}
	for _, m := range body.Messages {
		if m.Role == "system" {
			t.Fatal("system messages should be filtered from API response")
		}
		if m.Role == "developer" {
			t.Fatal("developer messages should be filtered from API response")
		}
	}
}

func TestRun_PersistsInjectedSystemPromptInHistoryAndStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock").AddTextResponse("hi there")
	rt := &serveRuntime{
		store:        store,
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
		systemPrompt: "server system prompt",
	}

	ctx := context.Background()
	_, err = rt.Run(ctx, true, false, []llm.Message{llm.UserText("hello")}, llm.Request{SessionID: "persist-system"})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(rt.history) != 3 {
		t.Fatalf("history len = %d, want 3", len(rt.history))
	}
	if rt.history[0].Role != llm.RoleSystem || rt.history[0].Parts[0].Text != "server system prompt" {
		t.Fatalf("history[0] = %+v, want injected system prompt", rt.history[0])
	}

	msgs, err := store.GetMessages(ctx, "persist-system", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("stored message count = %d, want 3", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].TextContent != "server system prompt" {
		t.Fatalf("stored first message = %+v, want injected system prompt", msgs[0])
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

func responseOutputText(t *testing.T, resp map[string]any) string {
	t.Helper()
	output, ok := resp["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("response output = %#v, want assistant message", resp["output"])
	}
	msg, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("first output item = %#v, want object", output[0])
	}
	content, ok := msg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("message content = %#v, want output_text", msg["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("first content part = %#v, want object", content[0])
	}
	text, _ := part["text"].(string)
	if text == "" {
		t.Fatalf("response text missing from %#v", part)
	}
	return text
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

func TestHandleSessionState_ReturnsModelAndEffortFromRuntime(t *testing.T) {
	rt := &serveRuntime{
		providerKey:  "anthropic",
		defaultModel: "claude-3-5-sonnet",
	}
	rt.mu.Lock()
	rt.sessionMeta = &session.Session{
		Model:           "claude-opus-4",
		ReasoningEffort: "high",
	}
	rt.mu.Unlock()

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions["sess-model"] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-model/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"model":"claude-opus-4"`) {
		t.Errorf("expected model from sessionMeta in state, got %s", body)
	}
	if !strings.Contains(body, `"reasoning_effort":"high"`) {
		t.Errorf("expected reasoning_effort from sessionMeta in state, got %s", body)
	}
	if !strings.Contains(body, `"provider":"anthropic"`) {
		t.Errorf("expected provider in state, got %s", body)
	}
}

func TestHandleSessionState_ExposesPendingInterjection(t *testing.T) {
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	engine.Interject("hi there")

	rt := &serveRuntime{engine: engine}
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions["sess-interject"] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-interject/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"pending_interjection"`) {
		t.Fatalf("expected pending_interjection in body, got %s", body)
	}
	if !strings.Contains(body, `"text":"hi there"`) {
		t.Fatalf("expected pending_interjection text, got %s", body)
	}

	// Peek must be non-destructive: a subsequent call should still see it.
	rr2 := httptest.NewRecorder()
	srv.handleSessionByID(rr2, req)
	if !strings.Contains(rr2.Body.String(), `"text":"hi there"`) {
		t.Fatalf("expected pending_interjection to survive repeated peeks, got %s", rr2.Body.String())
	}

	// DrainInterjection should still return the value.
	if got := engine.DrainInterjection(); got != "hi there" {
		t.Fatalf("drain after peeks = %q, want %q", got, "hi there")
	}
}

func TestHandleSessionState_FallsBackToDBWhenRuntimeNotLoaded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	sess := &session.Session{
		ID:              "sess-db-fallback",
		ProviderKey:     "openai",
		Model:           "gpt-5",
		ReasoningEffort: "medium",
		Mode:            session.ModeChat,
		Origin:          session.OriginWeb,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return &serveRuntime{}, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr, store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-db-fallback/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"model":"gpt-5"`) {
		t.Errorf("expected model from DB fallback in state, got %s", body)
	}
	if !strings.Contains(body, `"reasoning_effort":"medium"`) {
		t.Errorf("expected reasoning_effort from DB fallback in state, got %s", body)
	}
	if !strings.Contains(body, `"provider":"openai"`) {
		t.Errorf("expected provider from DB fallback in state, got %s", body)
	}
}

func TestHandleSessionState_DoesNotBlockWhileRunHoldsRuntimeMutex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	sess := &session.Session{
		ID:              "sess-busy",
		ProviderKey:     "openai",
		Model:           "gpt-5",
		ReasoningEffort: "medium",
		Mode:            session.ModeChat,
		Origin:          session.OriginWeb,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	rt := &serveRuntime{
		providerKey:  "openai",
		defaultModel: "gpt-5",
		store:        store,
		sessionMeta:  sess,
	}

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions[sess.ID] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr, store: store}

	// Simulate an active run by holding rt.mu while the handler runs. Release
	// before mgr.Close() so the runtime can finalize without deadlocking.
	rt.mu.Lock()
	lockReleased := false
	releaseLock := func() {
		if !lockReleased {
			lockReleased = true
			rt.mu.Unlock()
		}
	}
	defer releaseLock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/state", nil)
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		done <- rr
	}()

	select {
	case rr := <-done:
		releaseLock()
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, `"model":"gpt-5"`) {
			t.Errorf("expected model from DB fallback while rt.mu held, got %s", body)
		}
		if !strings.Contains(body, `"reasoning_effort":"medium"`) {
			t.Errorf("expected reasoning_effort from DB fallback while rt.mu held, got %s", body)
		}
		if !strings.Contains(body, `"provider":"openai"`) {
			t.Errorf("expected provider while rt.mu held, got %s", body)
		}
	case <-time.After(2 * time.Second):
		releaseLock()
		t.Fatal("handleSessionState blocked while rt.mu was held by a run")
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

func TestHandleResponses_NonStreamingResponseIDCanBeFetchedAndReplayed(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	code, resp := doResponses(t, srv, `{"input":"hi"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	responseID, _ := resp["id"].(string)
	if responseID == "" {
		t.Fatal("response missing id")
	}

	statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("get response by id failed: %v", err)
	}
	statusBody, _ := io.ReadAll(statusResp.Body)
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", statusResp.StatusCode, string(statusBody))
	}
	var snapshot map[string]any
	if err := json.Unmarshal(statusBody, &snapshot); err != nil {
		t.Fatalf("unmarshal response snapshot: %v", err)
	}
	if got, _ := snapshot["id"].(string); got != responseID {
		t.Fatalf("snapshot id = %q, want %q", got, responseID)
	}
	if got, _ := snapshot["status"].(string); got != "completed" {
		t.Fatalf("snapshot status = %q, want completed", got)
	}

	eventsResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID + "/events")
	if err != nil {
		t.Fatalf("get response events failed: %v", err)
	}
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != http.StatusOK {
		eventsBody, _ := io.ReadAll(eventsResp.Body)
		t.Fatalf("events status code = %d, want 200 (body=%s)", eventsResp.StatusCode, string(eventsBody))
	}

	scanner := bufio.NewScanner(eventsResp.Body)
	sawCreated := false
	sawDelta := false
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
			sawCreated = true
			response, _ := payload["response"].(map[string]any)
			if got, _ := response["id"].(string); got != responseID {
				t.Fatalf("created event id = %q, want %q", got, responseID)
			}
		case "response.output_text.delta":
			sawDelta = fmt.Sprint(payload["delta"]) == "hello"
		case "response.completed":
			sawCompleted = true
		}
	}
	if !sawCreated {
		t.Fatal("response.created event not found")
	}
	if !sawDelta {
		t.Fatal("response.output_text.delta event not found")
	}
	if !sawCompleted {
		t.Fatal("response.completed event not found")
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

func TestHandleResponses_StalePreviousResponseIDReturnsConflictAfterRuntimeRecreation(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2", "reply3")
	defer srv.sessionMgr.Close()

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "stale-recreate")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	code, resp2 := doResponses(t, srv, `{"input":"msg2","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("second response missing id")
	}

	// Simulate runtime recreation without cleaning the server-wide response map.
	srv.sessionMgr.mu.Lock()
	evicted := srv.sessionMgr.sessions["stale-recreate"]
	delete(srv.sessionMgr.sessions, "stale-recreate")
	srv.sessionMgr.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, _ = doResponses(t, srv, `{"input":"msg3","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("stale previous_response_id after recreation status = %d, want 409", code)
	}
}

func TestHandleResponses_FreshConversationResetRemovesStalePreviousResponseIDs(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "fresh-reset")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	// Simulate a recreated runtime while the old response ID mapping survives.
	srv.sessionMgr.mu.Lock()
	evicted := srv.sessionMgr.sessions["fresh-reset"]
	delete(srv.sessionMgr.sessions, "fresh-reset")
	srv.sessionMgr.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}
	if _, ok := srv.responseToSession.Load(respID1); !ok {
		t.Fatalf("responseToSession should still contain %q before reset", respID1)
	}

	code, resp2 := doResponsesWithHeader(t, srv, `{"input":"reset"}`, "fresh-reset")
	if code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("reset response missing id")
	}

	if _, ok := srv.responseToSession.Load(respID1); ok {
		t.Fatalf("stale previous_response_id %q should be removed after fresh reset", respID1)
	}
	latest, ok := srv.sessionToResponse.Load("fresh-reset")
	if !ok {
		t.Fatal("sessionToResponse missing fresh-reset after reset")
	}
	latestID, _ := latest.(string)
	if latestID != respID2 {
		t.Fatalf("sessionToResponse = %q, want %q", latestID, respID2)
	}
}

func TestHandleResponses_FreshConversationResetPreservesPreviousResponseIDsWhenRunFails(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "fresh-reset-busy")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	rt, ok := srv.sessionMgr.Get("fresh-reset-busy")
	if !ok || rt == nil {
		t.Fatal("expected runtime for fresh-reset-busy")
	}

	busyState := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	rt.mu.Lock()
	rt.setActiveInterrupt(busyState)
	defer func() {
		rt.clearActiveInterrupt(busyState)
		rt.mu.Unlock()
	}()

	code, _ = doResponsesWithHeader(t, srv, `{"input":"reset"}`, "fresh-reset-busy")
	if code != http.StatusConflict {
		t.Fatalf("reset status = %d, want 409", code)
	}

	mapped, ok := srv.responseToSession.Load(respID1)
	if !ok {
		t.Fatalf("responseToSession missing %q after failed fresh reset", respID1)
	}
	mappedSessionID, _ := mapped.(string)
	if mappedSessionID != "fresh-reset-busy" {
		t.Fatalf("responseToSession[%q] = %q, want %q", respID1, mappedSessionID, "fresh-reset-busy")
	}
	latest, ok := srv.sessionToResponse.Load("fresh-reset-busy")
	if !ok {
		t.Fatal("sessionToResponse missing fresh-reset-busy after failed reset")
	}
	latestID, _ := latest.(string)
	if latestID != respID1 {
		t.Fatalf("sessionToResponse = %q, want %q", latestID, respID1)
	}
}

func TestHandleResponses_FreshConversationBusySessionDoesNotOverwritePersistedRuntimeMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		provider.AddTextResponse("reply1")
		provider.AddTextResponse("reply2")
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

	code, resp := doResponsesWithHeader(t, srv, `{"input":"msg1","model":"original-model"}`, "fresh-reset-metadata")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID, _ := resp["id"].(string)
	if respID == "" {
		t.Fatal("first response missing id")
	}

	sess, err := store.Get(context.Background(), "fresh-reset-metadata")
	if err != nil {
		t.Fatalf("Get before busy reset: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session before busy reset")
	}
	if got := strings.TrimSpace(sess.Model); got != "original-model" {
		t.Fatalf("persisted model before busy reset = %q, want %q", got, "original-model")
	}

	rt, ok := srv.sessionMgr.Get("fresh-reset-metadata")
	if !ok || rt == nil {
		t.Fatal("expected runtime for fresh-reset-metadata")
	}
	busyState := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	rt.setActiveInterrupt(busyState)
	defer rt.clearActiveInterrupt(busyState)

	code, _ = doResponsesWithHeader(t, srv, `{"input":"reset","model":"replacement-model"}`, "fresh-reset-metadata")
	if code != http.StatusConflict {
		t.Fatalf("reset status = %d, want 409", code)
	}

	sess, err = store.Get(context.Background(), "fresh-reset-metadata")
	if err != nil {
		t.Fatalf("Get after busy reset: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session after busy reset")
	}
	if got := strings.TrimSpace(sess.Model); got != "original-model" {
		t.Fatalf("persisted model after busy reset = %q, want %q", got, "original-model")
	}
	mapped, ok := srv.responseToSession.Load(respID)
	if !ok {
		t.Fatalf("responseToSession missing %q after busy reset", respID)
	}
	mappedSessionID, _ := mapped.(string)
	if mappedSessionID != "fresh-reset-metadata" {
		t.Fatalf("responseToSession[%q] = %q, want %q", respID, mappedSessionID, "fresh-reset-metadata")
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

func TestHandleResponses_PreviousResponseIDFunctionCallOutputUsesPriorToolName(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddToolCall("call_1", "read_file", map[string]any{"path": "a.txt"}).
		AddTextResponse("done")

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
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

	srv := &serveServer{sessionMgr: manager}
	manager.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hi","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`, "tool-chain")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	body2 := `{"input":[{"type":"function_call_output","call_id":"call_1","output":"content"}],"previous_response_id":"` + respID1 + `"}`
	code, _ = doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.Requests))
	}

	var sawUserHi bool
	var sawToolCall bool
	var toolResultName string
	for _, msg := range provider.Requests[1].Messages {
		if msg.Role == llm.RoleUser && len(msg.Parts) > 0 && msg.Parts[0].Type == llm.PartText && msg.Parts[0].Text == "hi" {
			sawUserHi = true
		}
		for _, part := range msg.Parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.ID == "call_1" && part.ToolCall.Name == "read_file" {
				sawToolCall = true
			}
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.ID != "call_1" {
				continue
			}
			toolResultName = part.ToolResult.Name
		}
	}
	if !sawUserHi {
		t.Fatal("second provider request missing prior user message")
	}
	if !sawToolCall {
		t.Fatal("second provider request missing prior assistant tool call")
	}
	if toolResultName != "read_file" {
		t.Fatalf("tool result name = %q, want %q", toolResultName, "read_file")
	}
}

func TestHandleResponses_PreviousResponseIDFunctionCallOutputUsesPersistedToolNameAfterRuntimeRecreation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var created atomic.Int32
	providers := map[int32]*llm.MockProvider{}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := created.Add(1)
		provider := llm.NewMockProvider("mock")
		if createNum == 1 {
			provider.AddToolCall("call_1", "read_file", map[string]any{"path": "a.txt"})
		} else {
			provider.AddTextResponse("done")
		}
		providers[createNum] = provider
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-model",
			store:        store,
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{sessionMgr: manager, store: store}
	manager.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hi","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`, "tool-chain-store")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	manager.mu.Lock()
	evicted := manager.sessions["tool-chain-store"]
	delete(manager.sessions, "tool-chain-store")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	body2 := `{"input":[{"type":"function_call_output","call_id":"call_1","output":"content"}],"previous_response_id":"` + respID1 + `"}`
	code, _ = doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	provider := providers[2]
	if provider == nil {
		t.Fatal("expected recreated runtime provider")
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("recreated provider request count = %d, want 1", len(provider.Requests))
	}

	var toolResultName string
	for _, msg := range provider.Requests[0].Messages {
		for _, part := range msg.Parts {
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.ID != "call_1" {
				continue
			}
			toolResultName = part.ToolResult.Name
		}
	}
	if toolResultName != "read_file" {
		t.Fatalf("persisted tool result name = %q, want %q", toolResultName, "read_file")
	}
}

func TestHandleResponses_PreviousResponseIDRestoresPersistedProviderAfterRuntimeRecreation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var defaultCreates atomic.Int32
	var otherCreates atomic.Int32

	newRuntime := func(providerName, response string) *serveRuntime {
		provider := llm.NewMockProvider(providerName).AddTextResponse(response)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := defaultCreates.Add(1)
		return newRuntime("default", fmt.Sprintf("default response %d", createNum)), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, model string) (*serveRuntime, error) {
			createNum := otherCreates.Add(1)
			return newRuntime(providerName, fmt.Sprintf("%s response %d", providerName, createNum)), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"other"}`, "provider-chain")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	sess, err := store.Get(context.Background(), "provider-chain")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session")
	}
	if sess.ProviderKey != "other" {
		t.Fatalf("ProviderKey = %q, want other", sess.ProviderKey)
	}

	manager.mu.Lock()
	evicted := manager.sessions["provider-chain"]
	delete(manager.sessions, "provider-chain")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, resp2 := doResponses(t, srv, `{"input":"resume","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}

	output, ok := resp2["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("response output = %#v, want assistant message", resp2["output"])
	}
	msg, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("first output item = %#v, want object", output[0])
	}
	content, ok := msg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("message content = %#v, want output_text", msg["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("first content part = %#v, want object", content[0])
	}
	if got := part["text"]; got != "other response 2" {
		t.Fatalf("response text = %v, want %q", got, "other response 2")
	}
	if got := defaultCreates.Load(); got != 0 {
		t.Fatalf("default provider factory calls = %d, want 0", got)
	}
	if got := otherCreates.Load(); got != 2 {
		t.Fatalf("other provider factory calls = %d, want 2", got)
	}
}

func TestHandleResponses_ChainedRequestWithoutModelSwapRemainsPinnedToPersistedRuntime(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providers := map[string]*llm.MockProvider{}
	creates := map[string]int{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		creates[providerName]++
		provider := llm.NewMockProvider(providerName)
		provider.AddTextResponse(providerName + "-1")
		provider.AddTextResponse(providerName + "-2")
		providers[providerName] = provider
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: providerName + "-default", store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, modelName string) (*serveRuntime, error) {
			return newRuntime(providerName, modelName), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"old","model":"old-model"}`, "swap-pin")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID, _ := resp1["id"].(string)
	if respID == "" {
		t.Fatal("first response missing id")
	}
	code, resp2 := doResponses(t, srv, `{"input":"continue","previous_response_id":"`+respID+`","provider":"new","model":"new-model"}`)
	if code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp2); got != "old-2" {
		t.Fatalf("response text = %q, want old-2", got)
	}
	oldProvider := providers["old"]
	oldRequests := 0
	if oldProvider != nil {
		oldRequests = len(oldProvider.Requests)
	}
	if oldRequests != 2 {
		t.Fatalf("old provider requests = %d, want 2", oldRequests)
	}
	if got := oldProvider.Requests[1].Model; got != "old-model" {
		t.Fatalf("second request model = %q, want old-model", got)
	}
	if creates["new"] != 0 {
		t.Fatalf("new provider creates = %d, want 0 without model_swap", creates["new"])
	}
}

func TestHandleResponses_ModelSwapNaiveSuccessCommitsTargetRuntime(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providers := map[string]*llm.MockProvider{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		provider := llm.NewMockProvider(providerName)
		switch providerName {
		case "old":
			provider.AddTextResponse("old-1")
		case "new":
			provider.AddTextResponse("new-1")
			provider.AddTextResponse("new-2")
		default:
			provider.AddTextResponse(providerName + "-1")
		}
		providers[providerName] = provider
		engine := llm.NewEngine(provider, nil)
		if modelName == "" {
			modelName = providerName + "-default"
		}
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: modelName, store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, modelName string) (*serveRuntime, error) {
			return newRuntime(providerName, modelName), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"old","model":"old-model"}`, "swap-naive")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID := resp1["id"].(string)
	code, resp2 := doResponses(t, srv, `{"input":"continue","previous_response_id":"`+respID+`","provider":"new","model":"new-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusOK {
		t.Fatalf("swap status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp2); got != "new-1" {
		t.Fatalf("response text = %q, want new-1", got)
	}
	newProvider := providers["new"]
	newRequests := 0
	if newProvider != nil {
		newRequests = len(newProvider.Requests)
	}
	if newRequests != 1 {
		t.Fatalf("new provider requests = %d, want 1", newRequests)
	}
	if got := newProvider.Requests[0].Model; got != "new-model" {
		t.Fatalf("target request model = %q, want new-model", got)
	}
	if !requestContainsText(newProvider.Requests[0], "hello") || !requestContainsText(newProvider.Requests[0], "continue") {
		t.Fatalf("target request did not receive prior context plus pending user: %#v", newProvider.Requests[0].Messages)
	}
	sess, err := store.Get(context.Background(), "swap-naive")
	if err != nil || sess == nil {
		t.Fatalf("Get session after swap: %v", err)
	}
	if sess.ProviderKey != "new" || sess.Model != "new-model" {
		t.Fatalf("session runtime = %s/%s, want new/new-model", sess.ProviderKey, sess.Model)
	}
	storedMessages, err := store.GetMessages(context.Background(), "swap-naive", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after swap: %v", err)
	}
	foundMarker := false
	for _, msg := range storedMessages {
		if msg.Role == llm.RoleEvent {
			foundMarker = true
			if marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage()); !ok || marker.Status != "succeeded" || marker.Strategy != "naive" {
				t.Fatalf("unexpected model-swap marker: ok=%v marker=%#v", ok, marker)
			}
		}
	}
	if !foundMarker {
		t.Fatalf("expected persisted model-swap event marker, messages=%#v", storedMessages)
	}

	respID2 := resp2["id"].(string)
	code, resp3 := doResponses(t, srv, `{"input":"again","previous_response_id":"`+respID2+`"}`)
	if code != http.StatusOK {
		t.Fatalf("third status = %d, want 200 body=%#v", code, resp3)
	}
	if got := responseOutputText(t, resp3); got != "new-2" {
		t.Fatalf("third response text = %q, want new-2", got)
	}
	if len(newProvider.Requests) != 2 {
		t.Fatalf("new provider requests after third turn = %d, want 2", len(newProvider.Requests))
	}
	for _, msg := range newProvider.Requests[1].Messages {
		if msg.Role == llm.RoleEvent {
			t.Fatalf("provider request included event marker: %#v", newProvider.Requests[1].Messages)
		}
	}
}

func TestHandleResponses_ModelSwapNaiveFailureFallsBackToHandover(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providersByCreate := map[string][]*llm.MockProvider{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		provider := llm.NewMockProvider(providerName)
		providersByCreate[providerName] = append(providersByCreate[providerName], provider)
		switch providerName {
		case "old":
			if len(providersByCreate[providerName]) == 1 {
				provider.AddTextResponse("old-1")
			} else {
				provider.AddTextResponse("handover doc")
			}
		case "new":
			provider.AddError(errors.New("400 invalid_request: messages contain incompatible reasoning"))
			provider.AddTextResponse("retry-ok")
		default:
			provider.AddTextResponse(providerName + "-1")
		}
		engine := llm.NewEngine(provider, nil)
		if modelName == "" {
			modelName = providerName + "-default"
		}
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: modelName, store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, modelName string) (*serveRuntime, error) {
			return newRuntime(providerName, modelName), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"old","model":"old-model"}`, "swap-handover")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID := resp1["id"].(string)
	code, resp2 := doResponses(t, srv, `{"input":"continue","previous_response_id":"`+respID+`","provider":"new","model":"new-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusOK {
		t.Fatalf("swap status = %d, want 200 body=%#v", code, resp2)
	}
	if got := responseOutputText(t, resp2); got != "retry-ok" {
		t.Fatalf("response text = %q, want retry-ok", got)
	}
	newProvider := providersByCreate["new"][0]
	if len(newProvider.Requests) != 2 {
		t.Fatalf("new provider requests = %d, want naive + retry", len(newProvider.Requests))
	}
	if !requestContainsText(newProvider.Requests[1], "handover doc") || !requestContainsText(newProvider.Requests[1], "continue") {
		t.Fatalf("fallback request missing handover doc or pending user: %#v", newProvider.Requests[1].Messages)
	}
	if len(providersByCreate["old"]) < 2 || len(providersByCreate["old"][1].Requests) != 1 {
		t.Fatalf("expected helper old provider to generate handover")
	}
}

func TestHandleResponses_ModelSwapFallbackFailureRollsBackOriginalRuntimeAndHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providersByCreate := map[string][]*llm.MockProvider{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		provider := llm.NewMockProvider(providerName)
		providersByCreate[providerName] = append(providersByCreate[providerName], provider)
		switch providerName {
		case "old":
			if len(providersByCreate[providerName]) == 1 {
				provider.AddTextResponse("old-1")
				provider.AddTextResponse("old-2")
			} else {
				provider.AddTextResponse("handover doc")
			}
		case "new":
			provider.AddError(errors.New("400 invalid_request: messages contain incompatible reasoning"))
			provider.AddError(errors.New("400 invalid_request: retry still rejected"))
		default:
			provider.AddTextResponse(providerName + "-1")
		}
		engine := llm.NewEngine(provider, nil)
		if modelName == "" {
			modelName = providerName + "-default"
		}
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: modelName, store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, modelName string) (*serveRuntime, error) {
			return newRuntime(providerName, modelName), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"old","model":"old-model"}`, "swap-fail")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID := resp1["id"].(string)
	code, resp2 := doResponses(t, srv, `{"input":"continue","previous_response_id":"`+respID+`","provider":"new","model":"new-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("swap failure status = %d, want 400 body=%#v", code, resp2)
	}
	current, ok := manager.Get("swap-fail")
	if !ok || current.providerKey != "old" {
		t.Fatalf("session manager current provider = %v ok=%v, want old", func() string {
			if current == nil {
				return ""
			}
			return current.providerKey
		}(), ok)
	}
	sess, err := store.Get(context.Background(), "swap-fail")
	if err != nil || sess == nil {
		t.Fatalf("Get session after failed swap: %v", err)
	}
	if sess.ProviderKey != "old" || sess.Model != "old-model" {
		t.Fatalf("session runtime after rollback = %s/%s, want old/old-model", sess.ProviderKey, sess.Model)
	}
	storedMessages, err := store.GetMessages(context.Background(), "swap-fail", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after failed swap: %v", err)
	}
	if !sessionMessagesContainText(storedMessages, "hello") || !sessionMessagesContainText(storedMessages, "old-1") {
		t.Fatalf("rollback did not restore original conversation messages: %#v", storedMessages)
	}
	if sessionMessagesContainText(storedMessages, "handover doc") {
		t.Fatalf("rollback history should not persist handover retry context: %#v", storedMessages)
	}
	foundFailedMarker := false
	for _, msg := range storedMessages {
		if msg.Role == llm.RoleEvent {
			marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage())
			if ok && marker.Status == "failed" {
				foundFailedMarker = true
			}
		}
	}
	if !foundFailedMarker {
		t.Fatalf("expected failed model-swap marker after rollback: %#v", storedMessages)
	}

	code, resp3 := doResponses(t, srv, `{"input":"still old","previous_response_id":"`+respID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("post-rollback status = %d, want 200 body=%#v", code, resp3)
	}
	if got := responseOutputText(t, resp3); got != "old-2" {
		t.Fatalf("post-rollback response text = %q, want old-2", got)
	}
}

func sessionMessagesContainText(messages []session.Message, needle string) bool {
	for _, msg := range messages {
		if strings.Contains(msg.TextContent, needle) {
			return true
		}
		for _, part := range msg.Parts {
			if strings.Contains(part.Text, needle) {
				return true
			}
		}
	}
	return false
}

func requestContainsText(req llm.Request, needle string) bool {
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			if strings.Contains(part.Text, needle) {
				return true
			}
		}
	}
	return false
}

// A fresh conversation may reuse an existing session ID and switch to a new
// provider. The replacement runtime must also update the persisted session row
// so later chained requests resume on the new provider instead of jumping back
// to stale DB metadata.
func TestHandleResponses_FreshConversationReusedSessionIDUpdatesPersistedProvider(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var defaultCreates atomic.Int32
	var otherCreates atomic.Int32

	newRuntime := func(providerName, response string) *serveRuntime {
		provider := llm.NewMockProvider(providerName).AddTextResponse(response)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := defaultCreates.Add(1)
		return newRuntime("default", fmt.Sprintf("default response %d", createNum)), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, model string) (*serveRuntime, error) {
			createNum := otherCreates.Add(1)
			return newRuntime(providerName, fmt.Sprintf("%s response %d", providerName, createNum)), nil
		},
	}

	code, _ := doResponsesWithHeader(t, srv, `{"input":"hello"}`, "provider-reuse")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}

	code, resp2 := doResponsesWithHeader(t, srv, `{"input":"fresh","provider":"other"}`, "provider-reuse")
	if code != http.StatusOK {
		t.Fatalf("fresh provider request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp2); got != "other response 1" {
		t.Fatalf("response text = %q, want %q", got, "other response 1")
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("fresh provider response missing id")
	}

	sess, err := store.Get(context.Background(), "provider-reuse")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session")
	}
	if sess.ProviderKey != "other" {
		t.Fatalf("ProviderKey = %q, want other", sess.ProviderKey)
	}

	manager.mu.Lock()
	evicted := manager.sessions["provider-reuse"]
	delete(manager.sessions, "provider-reuse")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, resp3 := doResponses(t, srv, `{"input":"resume","previous_response_id":"`+respID2+`"}`)
	if code != http.StatusOK {
		t.Fatalf("resume request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp3); got != "other response 2" {
		t.Fatalf("resume response text = %q, want %q", got, "other response 2")
	}
	if got := defaultCreates.Load(); got != 1 {
		t.Fatalf("default provider factory calls = %d, want 1", got)
	}
	if got := otherCreates.Load(); got != 2 {
		t.Fatalf("other provider factory calls = %d, want 2", got)
	}
}

// When a fresh conversation reuses an old session ID but omits provider, it
// should fall back to the server default provider rather than inheriting the
// persisted provider from the prior conversation.
func TestHandleResponses_FreshConversationWithoutProviderUsesDefaultNotPersistedProvider(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var defaultCreates atomic.Int32
	var otherCreates atomic.Int32

	newRuntime := func(providerName string, createNum int32) *serveRuntime {
		provider := llm.NewMockProvider(providerName)
		provider.AddTextResponse(fmt.Sprintf("%s runtime %d response 1", providerName, createNum))
		provider.AddTextResponse(fmt.Sprintf("%s runtime %d response 2", providerName, createNum))
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := defaultCreates.Add(1)
		return newRuntime("default", createNum), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, model string) (*serveRuntime, error) {
			if providerName == "" || providerName == "default" {
				createNum := defaultCreates.Add(1)
				return newRuntime("default", createNum), nil
			}
			createNum := otherCreates.Add(1)
			return newRuntime(providerName, createNum), nil
		},
	}

	code, resp := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"other"}`, "provider-default-reuse")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp); got != "other runtime 1 response 1" {
		t.Fatalf("first response text = %q, want %q", got, "other runtime 1 response 1")
	}

	code, resp = doResponsesWithHeader(t, srv, `{"input":"fresh"}`, "provider-default-reuse")
	if code != http.StatusOK {
		t.Fatalf("fresh default request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp); got != "default runtime 1 response 1" {
		t.Fatalf("fresh default response text = %q, want %q", got, "default runtime 1 response 1")
	}
	respID, _ := resp["id"].(string)
	if respID == "" {
		t.Fatal("fresh default response missing id")
	}

	sess, err := store.Get(context.Background(), "provider-default-reuse")
	if err != nil {
		t.Fatalf("Get session after implicit default: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session after implicit default request")
	}
	if sess.ProviderKey != "default" {
		t.Fatalf("ProviderKey after implicit default = %q, want default", sess.ProviderKey)
	}

	manager.mu.Lock()
	evicted := manager.sessions["provider-default-reuse"]
	delete(manager.sessions, "provider-default-reuse")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, resp = doResponses(t, srv, `{"input":"resume","previous_response_id":"`+respID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("resume request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp); got != "default runtime 2 response 1" {
		t.Fatalf("resume response text = %q, want %q", got, "default runtime 2 response 1")
	}
	if got := defaultCreates.Load(); got != 2 {
		t.Fatalf("default provider factory calls = %d, want 2", got)
	}
	if got := otherCreates.Load(); got != 1 {
		t.Fatalf("other provider factory calls = %d, want 1", got)
	}
}

func TestFreshProviderRequest_ConcurrentReplace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	newRuntime := func(providerName string) *serveRuntime {
		provider := llm.NewMockProvider(providerName).AddTextResponse(providerName + " reply")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default"), nil
	})
	defer manager.Close()
	manager.onEvict = func(rt *serveRuntime) {}

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, providerName string, model string) (*serveRuntime, error) {
			return newRuntime(providerName), nil
		},
	}

	// Seed a session with the "default" provider.
	_, err = manager.GetOrCreate(context.Background(), "race-session")
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Launch concurrent replacement requests with different providers.
	const goroutines = 10
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	providers := make([]string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			provName := fmt.Sprintf("provider-%d", idx%2)
			rt, _, rerr := srv.runtimeForFreshProviderRequest(context.Background(), "race-session", provName)
			errs[idx] = rerr
			if rt != nil {
				providers[idx] = runtimeProviderKey(rt)
			}
		}(i)
	}
	wg.Wait()

	// Verify: all goroutines either succeeded, got errServeSessionBusy,
	// or got the belt-and-suspenders provider mismatch error (expected when
	// a concurrent goroutine wins the in-flight race with a different provider).
	anySuccess := false
	for i, err := range errs {
		if err != nil {
			if errors.Is(err, errServeSessionBusy) {
				continue
			}
			if strings.Contains(err.Error(), "already uses provider") {
				continue
			}
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
		anySuccess = true
	}
	if !anySuccess {
		t.Fatal("all goroutines failed; expected at least one success")
	}

	// The session should have a consistent provider after the race.
	// We can't predict which provider won (sequential replacements are valid),
	// but it must be one of the two requested providers.
	rt, ok := manager.Get("race-session")
	if !ok {
		t.Fatal("session missing after concurrent replace")
	}
	if got := runtimeProviderKey(rt); got != "provider-0" && got != "provider-1" {
		t.Fatalf("session provider = %q, want provider-0 or provider-1", got)
	}
}

func TestHandleResponses_NoPreviousResponseIDStartsFresh(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	// First request with session_id header, no previous_response_id
	code1, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "same-session")
	if code1 != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code1)
	}
	respID1, _ := resp1["id"].(string)
	if got := responseOutputText(t, resp1); got != "reply1" {
		t.Fatalf("first response text = %q, want %q", got, "reply1")
	}

	// Second request with same session_id header but no previous_response_id.
	// Should start a fresh conversation with a fresh runtime.
	code2, resp2 := doResponsesWithHeader(t, srv, `{"input":"msg2"}`, "same-session")
	if code2 != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200; without previous_response_id should start fresh", code2)
	}

	// Both should succeed and have different response IDs.
	respID2, _ := resp2["id"].(string)
	if respID1 == respID2 {
		t.Fatalf("response IDs should differ, both are %q", respID1)
	}
	if got := responseOutputText(t, resp2); got != "reply1" {
		t.Fatalf("second response text = %q, want %q from a fresh runtime", got, "reply1")
	}

	// Verify that the current runtime only saw the fresh request.
	rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "same-session")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	provider := rt.provider.(*llm.MockProvider)
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request on the fresh runtime, got %d", len(provider.Requests))
	}
	freshReq := provider.Requests[0]
	userMsgCount := 0
	for _, msg := range freshReq.Messages {
		if msg.Role == llm.RoleUser {
			userMsgCount++
		}
	}
	if userMsgCount != 1 {
		t.Fatalf("fresh request has %d user messages, want 1", userMsgCount)
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

func TestStreamResponses_FailedRunDoesNotBecomeLatestResponseID(t *testing.T) {
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

	firstReq, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"first","stream":true}`))
	if err != nil {
		t.Fatalf("new first request: %v", err)
	}
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("session_id", "busy-session")

	firstResp, err := ts.Client().Do(firstReq)
	if err != nil {
		t.Fatalf("first stream request failed: %v", err)
	}
	defer firstResp.Body.Close()

	firstScanner := bufio.NewScanner(firstResp.Body)
	var firstRespID string
	for {
		eventName, data, ok := readSSEEvent(t, firstScanner)
		if !ok {
			t.Fatal("first stream ended before first text delta")
		}
		if data == "[DONE]" {
			t.Fatal("first stream completed before first text delta")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal first SSE payload: %v", err)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			firstRespID, _ = response["id"].(string)
		case "response.output_text.delta":
			goto firstStreamBusy
		}
	}

firstStreamBusy:
	if firstRespID == "" {
		t.Fatal("missing first response id")
	}

	secondReq, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"second","stream":true}`))
	if err != nil {
		t.Fatalf("new second request: %v", err)
	}
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("session_id", "busy-session")

	secondResp, err := ts.Client().Do(secondReq)
	if err != nil {
		t.Fatalf("second stream request failed: %v", err)
	}
	defer secondResp.Body.Close()

	secondScanner := bufio.NewScanner(secondResp.Body)
	var secondRespID string
	sawFailed := false
	sawDone := false
	for {
		eventName, data, ok := readSSEEvent(t, secondScanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal second SSE payload: %v", err)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			secondRespID, _ = response["id"].(string)
		case "response.failed":
			sawFailed = true
			errPayload, _ := payload["error"].(map[string]any)
			if got := errPayload["type"]; got != "conflict_error" {
				t.Fatalf("response.failed error type = %v, want conflict_error", got)
			}
		}
	}
	if secondRespID == "" {
		t.Fatal("missing failed response id")
	}
	if !sawFailed {
		t.Fatal("second stream missing response.failed")
	}
	if !sawDone {
		t.Fatal("second stream missing [DONE]")
	}
	if _, ok := srv.responseToSession.Load(secondRespID); ok {
		t.Fatalf("failed response id %q should not be registered for chaining", secondRespID)
	}

	close(provider.releaseSecond)

	sawCompleted := false
	for {
		eventName, data, ok := readSSEEvent(t, firstScanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal completed first SSE payload: %v", err)
		}
		if eventName == "response.completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatal("first stream missing response.completed")
	}
	if _, ok := srv.responseToSession.Load(firstRespID); !ok {
		t.Fatalf("successful response id %q should be registered for chaining", firstRespID)
	}

	code, _ := doResponses(t, srv, `{"input":"follow-up","previous_response_id":"`+firstRespID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("chained request status = %d, want 200", code)
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

func TestHandleResponseByID_CancelIsIdempotentWhileRunInProgress(t *testing.T) {
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
	if secondRR.Code != http.StatusOK {
		t.Fatalf("second cancel status = %d, want 200", secondRR.Code)
	}
}

func TestHandleResponseByID_CancelStopsActiveToolRun(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_1", "slow_tool", map[string]any{})
	provider.AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&testServeDelayTool{delay: 5 * time.Second})

	engine := llm.NewEngine(provider, registry)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{
		responseRuns: newServeResponseRunManager(),
	}

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("sleep for a while"),
	}, llm.Request{
		SessionID:  "sess_cancel_tool",
		MaxTurns:   5,
		Tools:      []llm.ToolSpec{(&testServeDelayTool{}).Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
	}, "sess_cancel_tool", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun failed: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run.snapshot()
		recovery, ok := snapshot["recovery"].(map[string]any)
		if !ok {
			return false
		}
		messages, ok := recovery["messages"].([]map[string]any)
		if !ok {
			return false
		}
		for _, message := range messages {
			if message["role"] != "tool-group" || message["status"] != "running" {
				continue
			}
			toolsPayload, ok := message["tools"].([]map[string]any)
			if !ok {
				continue
			}
			for _, tool := range toolsPayload {
				if tool["name"] == "slow_tool" && tool["status"] == "running" {
					return true
				}
			}
		}
		return false
	}, "slow tool running in response recovery state")

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses/"+run.id+"/cancel", nil)
	firstRR := httptest.NewRecorder()
	srv.handleResponseByID(firstRR, firstReq)
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first cancel status = %d, want 200 body=%s", firstRR.Code, firstRR.Body.String())
	}

	waitForServeCondition(t, time.Second, func() bool {
		if rt.hasActiveRun() {
			return false
		}
		snapshot := run.snapshot()
		status, _ := snapshot["status"].(string)
		return status != "in_progress"
	}, "tool-backed response run to stop after cancel")

	if got := provider.CurrentTurn(); got != 1 {
		t.Fatalf("provider turn index = %d, want 1 (tool run should stop before follow-up turn)", got)
	}

	snapshot := run.snapshot()
	if status, _ := snapshot["status"].(string); status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled: %#v", status, snapshot)
	}
	if _, ok := snapshot["error"]; ok {
		t.Fatalf("cancelled run should not include error payload: %#v", snapshot)
	}

	subscription := run.subscribe(0)
	if subscription.ch != nil {
		t.Fatal("terminal cancelled run should replay without live subscription channel")
	}
	var sawCancelled bool
	for _, event := range subscription.replay {
		if event.Event == "response.failed" {
			t.Fatalf("cancelled run replay unexpectedly contained response.failed: %+v", event)
		}
		if event.Event == "response.cancelled" {
			sawCancelled = true
			var payload map[string]any
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatalf("unmarshal response.cancelled payload: %v", err)
			}
			response, _ := payload["response"].(map[string]any)
			if got := response["status"]; got != "cancelled" {
				t.Fatalf("response.cancelled status = %v, want cancelled", got)
			}
		}
	}
	if !sawCancelled {
		t.Fatal("cancelled run replay missing response.cancelled terminal event")
	}
}

func TestResolveServeResponseTimeout(t *testing.T) {
	tests := []struct {
		name      string
		flagSet   bool
		flagVal   time.Duration
		configVal string
		want      time.Duration
		wantErr   string
	}{
		{
			name: "default",
			want: defaultServeRequestTimeout,
		},
		{
			name:    "flag wins",
			flagSet: true,
			flagVal: 90 * time.Minute,
			want:    90 * time.Minute,
		},
		{
			name:      "config",
			configVal: "1h15m",
			want:      75 * time.Minute,
		},
		{
			name:      "invalid config",
			configVal: "eventually",
			wantErr:   "invalid serve.response_timeout",
		},
		{
			name:      "non-positive config",
			configVal: "0s",
			wantErr:   "must be > 0",
		},
		{
			name:    "non-positive flag",
			flagSet: true,
			flagVal: 0,
			wantErr: "invalid --response-timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveServeResponseTimeout(tt.flagSet, tt.flagVal, tt.configVal)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("timeout = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestStartResponseRunDeadlineExceededFailsWithHelpfulTimeoutMessage(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddError(context.DeadlineExceeded)
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{
		cfg: serveServerConfig{
			responseTimeout: 45 * time.Minute,
		},
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("take too long"),
	}, llm.Request{SessionID: "sess_timeout"}, "sess_timeout", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun failed: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run.snapshot()
		status, _ := snapshot["status"].(string)
		return status == "failed"
	}, "deadline-exceeded response run to fail")

	snapshot := run.snapshot()
	if status, _ := snapshot["status"].(string); status != "failed" {
		t.Fatalf("run status = %q, want failed: %#v", status, snapshot)
	}
	errPayload, _ := snapshot["error"].(map[string]any)
	if got := errPayload["type"]; got != "timeout_error" {
		t.Fatalf("error type = %v, want timeout_error: %#v", got, snapshot)
	}
	message, _ := errPayload["message"].(string)
	if !strings.Contains(message, "timed out after 45 minutes") {
		t.Fatalf("timeout message = %q, want configured 45 minute explanation", message)
	}
	if strings.Contains(message, "context deadline exceeded") {
		t.Fatalf("timeout message leaked raw context error: %q", message)
	}

	subscription := run.subscribe(0)
	if subscription.ch != nil {
		t.Fatal("terminal failed run should replay without live subscription channel")
	}
	var sawFailed bool
	for _, event := range subscription.replay {
		if event.Event != "response.failed" {
			continue
		}
		sawFailed = true
		var payload map[string]any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatalf("unmarshal response.failed payload: %v", err)
		}
		eventErr, _ := payload["error"].(map[string]any)
		if got := eventErr["message"]; got != message {
			t.Fatalf("response.failed message = %v, want %q", got, message)
		}
	}
	if !sawFailed {
		t.Fatal("deadline-exceeded run replay missing response.failed terminal event")
	}
}

func TestHandleResponseByID_CancelStopsShellToolRun(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_1", tools.ShellToolName, map[string]any{
		"command": "sleep 10",
	})
	provider.AddTextResponse("done")

	shellTool := tools.NewShellTool(nil, nil, tools.DefaultOutputLimits())
	registry := llm.NewToolRegistry()
	registry.Register(shellTool)

	engine := llm.NewEngine(provider, registry)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{
		responseRuns: newServeResponseRunManager(),
	}

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("sleep for a while"),
	}, llm.Request{
		SessionID:  "sess_cancel_shell_tool",
		MaxTurns:   5,
		Tools:      []llm.ToolSpec{shellTool.Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
	}, "sess_cancel_shell_tool", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun failed: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run.snapshot()
		recovery, ok := snapshot["recovery"].(map[string]any)
		if !ok {
			return false
		}
		messages, ok := recovery["messages"].([]map[string]any)
		if !ok {
			return false
		}
		for _, message := range messages {
			if message["role"] != "tool-group" || message["status"] != "running" {
				continue
			}
			toolsPayload, ok := message["tools"].([]map[string]any)
			if !ok {
				continue
			}
			for _, tool := range toolsPayload {
				if tool["name"] == tools.ShellToolName && tool["status"] == "running" {
					return true
				}
			}
		}
		return false
	}, "shell tool running in response recovery state")

	start := time.Now()
	cancelReq := httptest.NewRequest(http.MethodPost, "/v1/responses/"+run.id+"/cancel", nil)
	cancelRR := httptest.NewRecorder()
	srv.handleResponseByID(cancelRR, cancelReq)
	if cancelRR.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200 body=%s", cancelRR.Code, cancelRR.Body.String())
	}

	waitForServeCondition(t, 2*time.Second, func() bool {
		if rt.hasActiveRun() {
			return false
		}
		snapshot := run.snapshot()
		status, _ := snapshot["status"].(string)
		return status != "in_progress"
	}, "shell-backed response run to stop after cancel")

	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("shell tool cancel took too long: %s", elapsed)
	}
	if got := provider.CurrentTurn(); got != 1 {
		t.Fatalf("provider turn index = %d, want 1 (shell tool run should stop before follow-up turn)", got)
	}

	snapshot := run.snapshot()
	if status, _ := snapshot["status"].(string); status == "in_progress" {
		t.Fatalf("run status remained in_progress after shell-tool cancel: %#v", snapshot)
	}
}

func TestResponsesCompactedRunRequiresSnapshotRecovery(t *testing.T) {
	longText := strings.Repeat("abcdefghij", 3000)
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

func TestResponsesHandler_ReasoningEffortFlowsToProvider(t *testing.T) {
	var capturedProvider *llm.MockProvider
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		capturedProvider = provider
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

	body := `{"input":"hello","reasoning_effort":"high"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if capturedProvider == nil {
		t.Fatal("expected runtime factory to have been called")
	}
	if len(capturedProvider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(capturedProvider.Requests))
	}
	if got := capturedProvider.Requests[0].ReasoningEffort; got != "high" {
		t.Fatalf("ReasoningEffort = %q, want %q", got, "high")
	}
}

// After the first message of a web session pins reasoning_effort=high and
// model=first-model, chained requests on the same session must be silently
// overridden back to the persisted values. Mid-session switches of
// effort/model are disallowed — the first message of a conversation is the
// only place where client values are honored.
func TestHandleResponses_ModelAndEffortLockedAfterFirstMessage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	providers := make([]*llm.MockProvider, 0, 4)
	newRuntime := func() *serveRuntime {
		provider := llm.NewMockProvider("mock")
		provider.AddTextResponse("r1").AddTextResponse("r2").AddTextResponse("r3")
		providers = append(providers, provider)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-default-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime(), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
		store:      store,
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hi","model":"first-model","reasoning_effort":"high"}`, "lock-effort")
	if code != http.StatusOK {
		t.Fatalf("first status = %d", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	code, _ = doResponses(t, srv, `{"input":"again","model":"second-model","reasoning_effort":"low","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second status = %d", code)
	}

	if len(providers) == 0 {
		t.Fatal("expected runtime factory to have been called")
	}
	last := providers[len(providers)-1]
	if len(last.Requests) == 0 {
		t.Fatal("expected a provider request on second call")
	}
	lastReq := last.Requests[len(last.Requests)-1]
	if lastReq.Model != "first-model" {
		t.Fatalf("Model on second request = %q, want first-model (locked)", lastReq.Model)
	}
	if lastReq.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort on second request = %q, want high (locked)", lastReq.ReasoningEffort)
	}

	sess, err := store.Get(context.Background(), "lock-effort")
	if err != nil || sess == nil {
		t.Fatalf("Get session: err=%v sess=%v", err, sess)
	}
	if sess.Model != "first-model" {
		t.Fatalf("persisted Model = %q, want first-model", sess.Model)
	}
	if sess.ReasoningEffort != "high" {
		t.Fatalf("persisted ReasoningEffort = %q, want high", sess.ReasoningEffort)
	}
}

func TestHandleResponses_FreshConversationReusedSessionIDUpdatesPersistedModelAndEffort(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	providers := make([]*llm.MockProvider, 0, 4)
	var createNum atomic.Int32
	newRuntime := func() *serveRuntime {
		n := createNum.Add(1)
		provider := llm.NewMockProvider("mock").AddTextResponse(fmt.Sprintf("runtime %d", n))
		providers = append(providers, provider)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-default-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime(), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
		store:      store,
	}

	code, _ := doResponsesWithHeader(t, srv, `{"input":"hi","model":"first-model","reasoning_effort":"high"}`, "reuse-model-effort")
	if code != http.StatusOK {
		t.Fatalf("first status = %d", code)
	}

	code, resp2 := doResponsesWithHeader(t, srv, `{"input":"restart","model":"second-model","reasoning_effort":"low"}`, "reuse-model-effort")
	if code != http.StatusOK {
		t.Fatalf("fresh status = %d", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("fresh response missing id")
	}

	sess, err := store.Get(context.Background(), "reuse-model-effort")
	if err != nil || sess == nil {
		t.Fatalf("Get session: err=%v sess=%v", err, sess)
	}
	if sess.Model != "second-model" {
		t.Fatalf("persisted Model after fresh restart = %q, want second-model", sess.Model)
	}
	if sess.ReasoningEffort != "low" {
		t.Fatalf("persisted ReasoningEffort after fresh restart = %q, want low", sess.ReasoningEffort)
	}

	manager.mu.Lock()
	evicted := manager.sessions["reuse-model-effort"]
	delete(manager.sessions, "reuse-model-effort")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, _ = doResponses(t, srv, `{"input":"continue","model":"ignored-model","reasoning_effort":"high","previous_response_id":"`+respID2+`"}`)
	if code != http.StatusOK {
		t.Fatalf("resume status = %d", code)
	}

	if len(providers) < 3 {
		t.Fatalf("provider count = %d, want at least 3 runtimes", len(providers))
	}
	last := providers[len(providers)-1]
	if len(last.Requests) == 0 {
		t.Fatal("expected a provider request on resumed call")
	}
	lastReq := last.Requests[len(last.Requests)-1]
	if lastReq.Model != "second-model" {
		t.Fatalf("Model on resumed request = %q, want second-model", lastReq.Model)
	}
	if lastReq.ReasoningEffort != "low" {
		t.Fatalf("ReasoningEffort on resumed request = %q, want low", lastReq.ReasoningEffort)
	}
}

func TestResponsesHandler_ReasoningEffortDefaultNormalizedToEmpty(t *testing.T) {
	var capturedProvider *llm.MockProvider
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		capturedProvider = provider
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

	body := `{"input":"hello","reasoning_effort":"default"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(capturedProvider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(capturedProvider.Requests))
	}
	if got := capturedProvider.Requests[0].ReasoningEffort; got != "" {
		t.Fatalf("ReasoningEffort = %q, want empty (normalized from %q)", got, "default")
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

func TestStartResponseRun_StatelessCleanupRemovesResponseIDMapping(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddTextResponse("hello")

	rt := &serveRuntime{
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(rt, false, true, []llm.Message{
		llm.UserText("hi"),
	}, llm.Request{Model: "mock-model"}, "", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		run.mu.Lock()
		defer run.mu.Unlock()
		return run.status == "completed"
	}, "stateless response run completion")

	waitForServeCondition(t, time.Second, func() bool {
		_, ok := srv.responseToSession.Load(run.id)
		return !ok
	}, "stateless response ID cleanup")
}

func TestStartResponseRun_BusyConcurrentRunKeepsActiveSessionTracking(t *testing.T) {
	provider := newStagedProvider("hello ", "world")
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "staged",
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	const sessionID = "sess_busy_active_tracking"
	run1, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("first"),
	}, llm.Request{SessionID: sessionID}, sessionID, startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun first: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		return rt.hasActiveRun() && srv.responseRuns.activeRunID(sessionID) == run1.id
	}, "first response run to become active")

	run2, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("second"),
	}, llm.Request{SessionID: sessionID}, sessionID, startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun second: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run2.snapshot()
		status, _ := snapshot["status"].(string)
		return status == "failed"
	}, "second busy response run failure")

	if got := srv.responseRuns.activeRunID(sessionID); got != run1.id {
		t.Fatalf("activeRunID after concurrent busy run = %q, want %q", got, run1.id)
	}

	close(provider.releaseSecond)

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run1.snapshot()
		status, _ := snapshot["status"].(string)
		return status == "completed"
	}, "first response run completion")

	waitForServeCondition(t, time.Second, func() bool {
		return srv.responseRuns.activeRunID(sessionID) == ""
	}, "active response run cleanup")
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

func TestServeServer_RuntimeForRequest_StatefulRespectsRequestCancel(t *testing.T) {
	started := make(chan struct{})
	factory := func(ctx context.Context) (*serveRuntime, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := srv.runtimeForRequest(ctx, "slow-sess")
		errCh <- err
	}()

	<-started
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runtimeForRequest did not return after request cancellation")
	}

	if _, ok := mgr.Get("slow-sess"); ok {
		t.Fatal("cancelled runtime creation should not store a session runtime")
	}
}

func TestServeServer_RuntimeForProviderRequest_StatefulRespectsRequestCancel(t *testing.T) {
	started := make(chan struct{})
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return nil, fmt.Errorf("default session manager factory should not be used")
	})
	defer mgr.Close()

	srv := &serveServer{
		sessionMgr: mgr,
		runtimeFactory: func(ctx context.Context, providerName string, model string) (*serveRuntime, error) {
			if providerName != "test-provider" {
				return nil, fmt.Errorf("expected provider test-provider, got %q", providerName)
			}
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := srv.runtimeForProviderRequest(ctx, "slow-sess", "test-provider")
		errCh <- err
	}()

	<-started
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runtimeForProviderRequest did not return after request cancellation")
	}

	if _, ok := mgr.Get("slow-sess"); ok {
		t.Fatal("cancelled runtime creation should not store a session runtime")
	}
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

func TestHandleProviders_ReturnsList(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "anthropic",
		Providers: map[string]config.ProviderConfig{
			"anthropic": {Model: "claude-sonnet-4-6"},
			"openai":    {Model: "gpt-5"},
		},
	}
	srv := &serveServer{cfgRef: cfg}
	req := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	rr := httptest.NewRecorder()
	srv.handleProviders(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var result struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if result.Object != "list" {
		t.Fatalf("object = %q, want list", result.Object)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected at least one provider")
	}
	// Check that the default provider is marked
	found := false
	for _, p := range result.Data {
		if p["name"] == "anthropic" {
			if p["is_default"] != true {
				t.Errorf("anthropic should be marked as default")
			}
			found = true
		}
	}
	if !found {
		t.Error("expected anthropic in provider list")
	}
}

func TestHandleProviders_MethodNotAllowed(t *testing.T) {
	srv := &serveServer{cfgRef: &config.Config{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/providers", nil)
	rr := httptest.NewRecorder()
	srv.handleProviders(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandleModels_WithProviderParam(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "anthropic",
		Providers: map[string]config.ProviderConfig{
			"anthropic": {Model: "claude-sonnet-4-6"},
		},
	}
	// Pre-seed the cache so getModelsProvider doesn't try to construct a real
	// Anthropic provider (which requires auth not present in CI).
	mock := llm.NewMockProvider("anthropic")
	srv := &serveServer{
		cfgRef:          cfg,
		modelsProviders: map[string]llm.Provider{"anthropic": mock},
	}

	// Without provider param — uses default
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected at least one model for default provider")
	}

	// With unknown provider param — returns error
	req = httptest.NewRequest(http.MethodGet, "/v1/models?provider=nonexistent", nil)
	rr = httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown provider", rr.Code)
	}
}

func TestHandleModels_DropsEffortSuffixDuplicates(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "claude-bin",
		Providers:       map[string]config.ProviderConfig{"claude-bin": {}},
	}
	// Use the MockProvider so ListModels returns nothing and we fall back
	// to the curated claude-bin list (which contains opus-low/medium/high/...).
	mock := llm.NewMockProvider("claude-bin")
	srv := &serveServer{
		cfgRef:          cfg,
		modelsProviders: map[string]llm.Provider{"claude-bin": mock},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models?provider=claude-bin", nil)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	got := make(map[string]bool, len(result.Data))
	for _, m := range result.Data {
		id, _ := m["id"].(string)
		got[id] = true
	}
	// Base models must remain.
	for _, want := range []string{"opus", "sonnet", "haiku"} {
		if !got[want] {
			t.Errorf("expected %q in models response, got %v", want, got)
		}
	}
	// Effort-suffixed aliases must be filtered out — the web UI has a
	// dedicated reasoning-effort selector for these.
	for _, banned := range []string{"opus-low", "opus-medium", "opus-high", "opus-xhigh", "opus-max", "sonnet-low", "sonnet-medium", "sonnet-high"} {
		if got[banned] {
			t.Errorf("unexpected %q in models response (should be deduped by effort selector)", banned)
		}
	}
}

func TestHandleResponses_WithProviderField(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	manager := newServeSessionManager(time.Minute, 10, factory)
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
		runtimeFactory: func(ctx context.Context, providerName string, model string) (*serveRuntime, error) {
			engine := llm.NewEngine(provider, nil)
			rt := &serveRuntime{
				provider:     provider,
				providerKey:  providerName,
				engine:       engine,
				defaultModel: "mock-model",
			}
			rt.Touch()
			return rt, nil
		},
	}

	// Request with non-default provider creates session with that provider
	body := `{"input":"hello","provider":"other"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleResponses_PreservesClientDefinedPassthroughTools(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_passthrough_1", "client_tool", map[string]any{"value": "ok"})

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	manager := newServeSessionManager(time.Minute, 10, factory)
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
	}

	code, _ := doResponses(t, srv, `{
		"input":"hello",
		"tools":[{
			"type":"function",
			"name":"client_tool",
			"description":"Client-defined passthrough tool",
			"parameters":{
				"type":"object",
				"properties":{"value":{"type":"string"}},
				"required":["value"]
			}
		}],
		"tool_choice":{"type":"function","name":"client_tool"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.Requests))
	}

	req := provider.Requests[0]
	if len(req.Tools) != 1 {
		t.Fatalf("tool count = %d, want 1", len(req.Tools))
	}
	if req.Tools[0].Name != "client_tool" {
		t.Fatalf("tool name = %q, want client_tool", req.Tools[0].Name)
	}
	if req.Tools[0].Description != "Client-defined passthrough tool" {
		t.Fatalf("tool description = %q", req.Tools[0].Description)
	}
	if req.ToolChoice.Mode != llm.ToolChoiceName || req.ToolChoice.Name != "client_tool" {
		t.Fatalf("tool choice = %#v, want name client_tool", req.ToolChoice)
	}
	props, ok := req.Tools[0].Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool schema properties missing: %#v", req.Tools[0].Schema)
	}
	valueProp, ok := props["value"].(map[string]interface{})
	if !ok || valueProp["type"] != "string" {
		t.Fatalf("tool schema value property = %#v, want string type", props["value"])
	}
}

func TestSessionManager_GetOrCreateWithDeduplication(t *testing.T) {
	var calls int32
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return &serveRuntime{providerKey: "default"}, nil
	})
	defer manager.Close()

	const workers = 12
	results := make(chan *serveRuntime, workers)
	errs := make(chan error, workers)

	customFactory := func(ctx context.Context) (*serveRuntime, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(25 * time.Millisecond)
		rt := &serveRuntime{providerKey: "custom"}
		rt.Touch()
		return rt, nil
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			rt, err := manager.GetOrCreateWith(context.Background(), "same-id", customFactory)
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
		t.Fatalf("GetOrCreateWith error: %v", err)
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
	if first.providerKey != "custom" {
		t.Fatalf("providerKey = %q, want custom", first.providerKey)
	}
}

// ---------------------------------------------------------------------------
// Anthropic Messages API tests
// ---------------------------------------------------------------------------

func TestParseAnthropicMessages_SimpleText(t *testing.T) {
	msgs, err := parseAnthropicMessages([]anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"Hello"`)},
	})
	if err != nil {
		t.Fatalf("parseAnthropicMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("role = %s, want user", msgs[0].Role)
	}
	if msgs[0].Parts[0].Text != "Hello" {
		t.Fatalf("text = %q, want Hello", msgs[0].Parts[0].Text)
	}
}

func TestParseAnthropicMessages_ContentBlocks(t *testing.T) {
	msgs, err := parseAnthropicMessages([]anthropicMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Hi"},{"type":"text","text":" there"}]`)},
	})
	if err != nil {
		t.Fatalf("parseAnthropicMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	if len(msgs[0].Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(msgs[0].Parts))
	}
	if msgs[0].Parts[0].Text != "Hi" || msgs[0].Parts[1].Text != " there" {
		t.Fatalf("unexpected text parts")
	}
}

func TestParseAnthropicMessages_ToolUseRoundTrip(t *testing.T) {
	msgs, err := parseAnthropicMessages([]anthropicMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"a.txt"}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_1","content":"file contents"}]`)},
	})
	if err != nil {
		t.Fatalf("parseAnthropicMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleAssistant {
		t.Fatalf("first role = %s, want assistant", msgs[0].Role)
	}
	if msgs[0].Parts[0].ToolCall == nil || msgs[0].Parts[0].ToolCall.Name != "read_file" {
		t.Fatalf("missing tool call")
	}
	if msgs[1].Parts[0].ToolResult == nil || msgs[1].Parts[0].ToolResult.ID != "call_1" {
		t.Fatalf("missing tool result")
	}
	if msgs[1].Parts[0].ToolResult.Content != "file contents" {
		t.Fatalf("tool result content = %q", msgs[1].Parts[0].ToolResult.Content)
	}
}

func TestParseAnthropicSystem(t *testing.T) {
	// String form
	if got := parseAnthropicSystem(json.RawMessage(`"Be helpful"`)); got != "Be helpful" {
		t.Fatalf("string system = %q", got)
	}
	// Array form
	if got := parseAnthropicSystem(json.RawMessage(`[{"type":"text","text":"System prompt"}]`)); got != "System prompt" {
		t.Fatalf("array system = %q", got)
	}
	// Empty
	if got := parseAnthropicSystem(nil); got != "" {
		t.Fatalf("nil system = %q", got)
	}
}

func TestParseAnthropicToolChoice(t *testing.T) {
	if got := parseAnthropicToolChoice(json.RawMessage(`{"type":"auto"}`)); got.Mode != llm.ToolChoiceAuto {
		t.Fatalf("auto mode = %s", got.Mode)
	}
	if got := parseAnthropicToolChoice(json.RawMessage(`{"type":"any"}`)); got.Mode != llm.ToolChoiceRequired {
		t.Fatalf("any mode = %s", got.Mode)
	}
	if got := parseAnthropicToolChoice(json.RawMessage(`{"type":"tool","name":"shell"}`)); got.Mode != llm.ToolChoiceName || got.Name != "shell" {
		t.Fatalf("tool mode = %#v", got)
	}
	if got := parseAnthropicToolChoice(nil); got.Mode != llm.ToolChoiceAuto {
		t.Fatalf("nil mode = %s", got.Mode)
	}
}

func TestHandleAnthropicMessages_NonStreaming(t *testing.T) {
	srv := newTestServeServer("Hello from Anthropic!")
	body := `{"model":"test","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["type"] != "message" {
		t.Fatalf("type = %v, want message", result["type"])
	}
	if result["role"] != "assistant" {
		t.Fatalf("role = %v, want assistant", result["role"])
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content empty or wrong type")
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello from Anthropic!" {
		t.Fatalf("content block = %v", block)
	}
	if result["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", result["stop_reason"])
	}
}

func TestHandleAnthropicMessages_DoesNotExposeUnrequestedServerTools(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(_ context.Context) (*serveRuntime, error) {
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

	body := `{
		"model": "test",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{
			"name": "client_tool",
			"description": "Client-defined passthrough tool",
			"input_schema": {
				"type": "object",
				"properties": {
					"query": {"type": "string"}
				},
				"required": ["query"]
			}
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request, got %d", len(provider.Requests))
	}
	tools := provider.Requests[0].Tools
	if len(tools) != 1 {
		t.Fatalf("expected 1 passthrough tool, got %d", len(tools))
	}
	if tools[0].Name != "client_tool" {
		t.Fatalf("expected tool name client_tool, got %q", tools[0].Name)
	}
	if tools[0].Description != "Client-defined passthrough tool" {
		t.Fatalf("unexpected tool description: %q", tools[0].Description)
	}
}

func TestHandleAnthropicMessages_NoToolsDoesNotExposeServerTools(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(_ context.Context) (*serveRuntime, error) {
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

	body := `{
		"model": "test",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hi"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request, got %d", len(provider.Requests))
	}
	if got := len(provider.Requests[0].Tools); got != 0 {
		t.Fatalf("expected 0 tools, got %d", got)
	}
}

func TestHandleAnthropicMessages_StreamText(t *testing.T) {
	srv := newTestServeServer("streamed text")
	body := `{"model":"test","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	output := rr.Body.String()

	// Verify required SSE events are present
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in SSE output", want)
		}
	}

	// Verify text_delta contains our text (mock chunks into ~10 char pieces)
	if !strings.Contains(output, "streamed") {
		t.Errorf("missing 'streamed' in output")
	}
	if !strings.Contains(output, "text") {
		t.Errorf("missing 'text' in output")
	}

	// Verify stop_reason is end_turn (no tool calls)
	if !strings.Contains(output, `"stop_reason":"end_turn"`) {
		t.Errorf("missing end_turn stop_reason")
	}
}

func TestHandleAnthropicMessages_Auth_XApiKey(t *testing.T) {
	srv := newTestServeServer("ok")
	srv.cfg.requireAuth = true
	srv.cfg.token = "secret-token"

	body := `{"model":"test","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`

	// No auth → 401
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.auth(srv.handleAnthropicMessages)(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want 401", rr.Code)
	}

	// x-api-key → 200
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "secret-token")
	rr = httptest.NewRecorder()
	srv.auth(srv.handleAnthropicMessages)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("x-api-key: status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// Bearer token also still works
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")
	rr = httptest.NewRecorder()
	srv.auth(srv.handleAnthropicMessages)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer: status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAnthropicMessages_MethodNotAllowed(t *testing.T) {
	srv := newTestServeServer("ok")
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

// newTestServeServerWithToolMap creates a test serve server with a registered
// "echo" tool and the given toolMap for testing --tool-map behavior.
func newTestServeServerWithToolMap(toolMap map[string]string, responses ...string) *serveServer {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		for _, r := range responses {
			provider.AddTextResponse(r)
		}
		registry := llm.NewToolRegistry()
		registry.Register(&echoTool{})
		engine := llm.NewEngine(provider, registry)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
			toolMap:      toolMap,
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

func TestSelectTools_ResolvesToolMapNames(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{}) // registers as "echo"
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider: provider,
		engine:   engine,
		toolMap:  map[string]string{"MyEcho": "echo"},
	}

	// Requesting the client name "MyEcho" should resolve to server tool "echo"
	tools := rt.selectTools(map[string]bool{"MyEcho": true})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Fatalf("expected tool name 'echo', got %q", tools[0].Name)
	}

	// Requesting by server name directly should also work
	tools = rt.selectTools(map[string]bool{"echo": true})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool when using server name, got %d", len(tools))
	}

	// Requesting an unknown name should return nothing
	tools = rt.selectTools(map[string]bool{"nonexistent": true})
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools for unknown name, got %d", len(tools))
	}

	// No filter returns all
	tools = rt.selectTools(nil)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool for nil filter, got %d", len(tools))
	}
}

func TestToolMap_ChatCompletions(t *testing.T) {
	srv := newTestServeServerWithToolMap(
		map[string]string{"MyEcho": "echo"},
		"mapped tool works",
	)

	body := `{
		"model": "test",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{"type": "function", "function": {"name": "MyEcho"}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("expected choices in response")
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "mapped tool works" {
		t.Fatalf("unexpected content: %v", msg["content"])
	}
}

func TestChatCompletions_PreservesClientPassthroughTools(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	engine := llm.NewEngine(provider, nil)

	factory := func(ctx context.Context) (*serveRuntime, error) {
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

	body := `{
		"model": "test",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "client_tool",
				"description": "Client-defined passthrough tool",
				"parameters": {
					"type": "object",
					"properties": {
						"query": {"type": "string"}
					},
					"required": ["query"]
				}
			}
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request, got %d", len(provider.Requests))
	}
	tools := provider.Requests[0].Tools
	if len(tools) != 1 {
		t.Fatalf("expected 1 passthrough tool, got %d", len(tools))
	}
	if tools[0].Name != "client_tool" {
		t.Fatalf("expected tool name client_tool, got %q", tools[0].Name)
	}
	if tools[0].Description != "Client-defined passthrough tool" {
		t.Fatalf("unexpected tool description: %q", tools[0].Description)
	}
	if got := tools[0].Schema["type"]; got != "object" {
		t.Fatalf("expected tool schema type object, got %#v", got)
	}
	props, ok := tools[0].Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected schema properties map, got %#v", tools[0].Schema["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Fatalf("expected query property in schema, got %#v", props)
	}
}

func TestChatCompletions_ToolResultWithoutReplayedToolCallKeepsNameFromSessionHistory(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "client_tool", map[string]any{"query": "hi"}).
		AddTextResponse("all set")
	engine := llm.NewEngine(provider, nil)

	factory := func(ctx context.Context) (*serveRuntime, error) {
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

	firstBody := `{
		"model": "test",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{"type": "function", "function": {"name": "client_tool"}}]
	}`
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(firstBody))
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("session_id", "tool-history-session")
	firstResp := httptest.NewRecorder()
	srv.handleChatCompletions(firstResp, firstReq)
	if firstResp.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body: %s", firstResp.Code, firstResp.Body.String())
	}

	secondBody := `{
		"model": "test",
		"messages": [{"role": "tool", "tool_call_id": "call-1", "content": "done"}]
	}`
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(secondBody))
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("session_id", "tool-history-session")
	secondResp := httptest.NewRecorder()
	srv.handleChatCompletions(secondResp, secondReq)
	if secondResp.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body: %s", secondResp.Code, secondResp.Body.String())
	}

	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.Requests))
	}
	if len(provider.Requests[1].Messages) != 3 {
		t.Fatalf("second provider request message count = %d, want 3", len(provider.Requests[1].Messages))
	}
	if provider.Requests[1].Messages[0].Role != llm.RoleUser || len(provider.Requests[1].Messages[0].Parts) != 1 || provider.Requests[1].Messages[0].Parts[0].Type != llm.PartText || provider.Requests[1].Messages[0].Parts[0].Text != "Hi" {
		t.Fatalf("second request message[0] = %+v, want original user message", provider.Requests[1].Messages[0])
	}
	if provider.Requests[1].Messages[1].Role != llm.RoleAssistant || len(provider.Requests[1].Messages[1].Parts) != 1 || provider.Requests[1].Messages[1].Parts[0].Type != llm.PartToolCall || provider.Requests[1].Messages[1].Parts[0].ToolCall == nil || provider.Requests[1].Messages[1].Parts[0].ToolCall.ID != "call-1" {
		t.Fatalf("second request message[1] = %+v, want assistant tool call", provider.Requests[1].Messages[1])
	}
	if provider.Requests[1].Messages[2].Role != llm.RoleTool || len(provider.Requests[1].Messages[2].Parts) != 1 || provider.Requests[1].Messages[2].Parts[0].Type != llm.PartToolResult || provider.Requests[1].Messages[2].Parts[0].ToolResult == nil || provider.Requests[1].Messages[2].Parts[0].ToolResult.ID != "call-1" {
		t.Fatalf("second request message[2] = %+v, want tool result", provider.Requests[1].Messages[2])
	}

	var toolResultName string
	for _, msg := range provider.Requests[1].Messages {
		for _, part := range msg.Parts {
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.ID != "call-1" {
				continue
			}
			toolResultName = part.ToolResult.Name
		}
	}
	if toolResultName != "client_tool" {
		t.Fatalf("tool result name = %q, want %q", toolResultName, "client_tool")
	}
}

func TestToolMap_Responses(t *testing.T) {
	srv := newTestServeServerWithToolMap(
		map[string]string{"MyEcho": "echo"},
		"mapped response works",
	)

	body := `{
		"model": "test",
		"input": "Hi",
		"tools": [{"type": "function", "name": "MyEcho"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output, ok := result["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("expected output in response")
	}
	msg := output[0].(map[string]any)
	content := msg["content"].([]any)[0].(map[string]any)
	if content["text"] != "mapped response works" {
		t.Fatalf("unexpected text: %v", content["text"])
	}
}

func TestResolvePlatforms_API(t *testing.T) {
	got, err := resolvePlatforms([]string{"api"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "api" {
		t.Fatalf("got %v, want [api]", got)
	}
}

func TestSingleServeTemplatePlatform_API(t *testing.T) {
	if got := singleServeTemplatePlatform([]string{"api"}); got != "api" {
		t.Fatalf("got %q, want %q", got, "api")
	}
}

func TestServeHTTPHandler_MountsAPIOnlyUnderBasePath(t *testing.T) {
	srv := &serveServer{
		cfg:        serveServerConfig{basePath: "/ui", api: true},
		sessionMgr: newServeSessionManager(time.Minute, 10, nil),
	}
	handler := srv.httpHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// API routes should be reachable under basePath
	resp, err := http.Get(ts.URL + "/ui/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}

	// Root should not redirect (no UI)
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("root request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusTemporaryRedirect {
		t.Fatalf("api-only should not redirect root to basePath")
	}
}

func TestNonStreamingChat_ShowsServerExecutedToolCalls(t *testing.T) {
	// Script: model calls the echo tool (server-executed), then returns "done".
	// The chat completions endpoint is used by the web UI, which needs to see
	// tool calls so it can display them. They must NOT be filtered here.
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(ctx context.Context) (*serveRuntime, error) {
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

	body := `{"model":"test","messages":[{"role":"user","content":"call echo"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := result["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	// "echo" is server-executed but the chat completions handler must pass tool
	// calls through so the web UI can display them.
	if msg["tool_calls"] == nil {
		t.Fatalf("expected tool_calls to be visible in chat completions response, got nil")
	}
	if msg["content"] != "done" {
		t.Fatalf("expected final text 'done', got %v", msg["content"])
	}
}

func TestChatCompletionFinalResponse_UsageIncludesCachedPromptTokens(t *testing.T) {
	var result serveRunResult
	result.Text.WriteString("done")
	result.Usage = llm.Usage{
		InputTokens:       100,
		CachedInputTokens: 20,
		CacheWriteTokens:  5,
		OutputTokens:      7,
	}

	response := chatCompletionFinalResponse(result, "test-model")
	usage := response["usage"].(map[string]any)

	if got := usage["prompt_tokens"]; got != 120 {
		t.Fatalf("prompt_tokens = %v, want 120", got)
	}
	if got := usage["completion_tokens"]; got != 7 {
		t.Fatalf("completion_tokens = %v, want 7", got)
	}
	if got := usage["total_tokens"]; got != 127 {
		t.Fatalf("total_tokens = %v, want 127", got)
	}

	details := usage["prompt_tokens_details"].(map[string]any)
	if got := details["cached_tokens"]; got != 20 {
		t.Fatalf("cached_tokens = %v, want 20", got)
	}
	if got := details["cache_write_tokens"]; got != 5 {
		t.Fatalf("cache_write_tokens = %v, want 5", got)
	}
}

func TestStreamingChatIncludeUsage_UsageIncludesCachedPromptTokens(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{
		Text: "done",
		Usage: llm.Usage{
			InputTokens:       100,
			CachedInputTokens: 20,
			CacheWriteTokens:  5,
			OutputTokens:      7,
		},
	})
	engine := llm.NewEngine(provider, nil)

	factory := func(ctx context.Context) (*serveRuntime, error) {
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

	body := `{
		"model": "test",
		"stream": true,
		"stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "Hi"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	scanner := bufio.NewScanner(bytes.NewReader(rr.Body.Bytes()))
	for {
		_, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("stream ended before usage chunk")
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal stream chunk: %v", err)
		}

		usageVal, ok := payload["usage"]
		if !ok {
			continue
		}
		usage := usageVal.(map[string]any)
		if got := usage["prompt_tokens"].(float64); got != 120 {
			t.Fatalf("prompt_tokens = %v, want 120", got)
		}
		if got := usage["completion_tokens"].(float64); got != 7 {
			t.Fatalf("completion_tokens = %v, want 7", got)
		}
		if got := usage["total_tokens"].(float64); got != 127 {
			t.Fatalf("total_tokens = %v, want 127", got)
		}
		details := usage["prompt_tokens_details"].(map[string]any)
		if got := details["cached_tokens"].(float64); got != 20 {
			t.Fatalf("cached_tokens = %v, want 20", got)
		}
		if got := details["cache_write_tokens"].(float64); got != 5 {
			t.Fatalf("cache_write_tokens = %v, want 5", got)
		}
		return
	}

	t.Fatal("did not find usage chunk in stream")
}

func TestNonStreamingResponses_FiltersServerExecutedToolCalls(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr, cfg: serveServerConfig{suppressServerTools: true}}

	body := `{"model":"test","input":"call echo"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output := result["output"].([]any)
	// Should have only the text message, no function_call items
	for _, item := range output {
		m := item.(map[string]any)
		if m["type"] == "function_call" {
			t.Fatalf("server-executed tool calls should be filtered; got function_call in output")
		}
	}
	if len(output) != 1 {
		t.Fatalf("expected 1 output item (text), got %d", len(output))
	}
}
