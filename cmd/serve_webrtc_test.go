package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServeWebRTC_HeadSnippetAbsent verifies that renderIndexHTML does not
// inject WebRTC globals when webrtcHeadSnippet is empty (default).
func TestServeWebRTC_HeadSnippetAbsent(t *testing.T) {
	s := &serveServer{
		cfg:               serveServerConfig{basePath: "/ui"},
		webrtcHeadSnippet: "",
	}
	html := string(s.renderIndexHTML())
	if strings.Contains(html, "__WEBRTC_ENABLED__") {
		t.Error("renderIndexHTML should not contain __WEBRTC_ENABLED__ when snippet is empty")
	}
	if strings.Contains(html, "__WEBRTC_SIGNALING_URL__") {
		t.Error("renderIndexHTML should not contain __WEBRTC_SIGNALING_URL__ when snippet is empty")
	}
}

// TestServeWebRTC_InjectsHeadSnippet verifies that a non-empty webrtcHeadSnippet
// is embedded in the rendered HTML.
func TestServeWebRTC_InjectsHeadSnippet(t *testing.T) {
	snippet := `<script>window.__WEBRTC_ENABLED__=true;window.__WEBRTC_SIGNALING_URL__="https://relay.example.com/webrtc";</script>`
	s := &serveServer{
		cfg:               serveServerConfig{basePath: "/ui"},
		webrtcEnabled:     true,
		webrtcHeadSnippet: snippet,
	}
	html := string(s.renderIndexHTML())
	if !strings.Contains(html, "__WEBRTC_ENABLED__") {
		t.Error("renderIndexHTML should contain __WEBRTC_ENABLED__ when snippet is set")
	}
	if !strings.Contains(html, "relay.example.com") {
		t.Error("renderIndexHTML should contain the signaling URL when snippet is set")
	}
}

func TestRenderIndexHTMLUsesSingleModuleForWebRTC(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s := &serveServer{
			cfg:               serveServerConfig{basePath: "/ui"},
			webrtcEnabled:     enabled,
			webrtcHeadSnippet: map[bool]string{true: `<script>window.__WEBRTC_ENABLED__=true;</script>`}[enabled],
		}
		html := string(s.renderIndexHTML())
		if strings.Count(html, `type="module" src="dist/app.js`) != 1 {
			t.Fatalf("enabled=%v: expected one application module", enabled)
		}
		if strings.Contains(html, "app-webrtc.js") {
			t.Fatalf("enabled=%v: legacy WebRTC script remains", enabled)
		}
	}
}

func TestHandleUIServiceWorkerKeepsWebRTCChunkNetworkFirst(t *testing.T) {
	var bodies []string
	for _, enabled := range []bool{false, true} {
		s := &serveServer{
			cfg:           serveServerConfig{ui: true, basePath: "/ui"},
			webrtcEnabled: enabled,
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)

		s.handleUI(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%v: status = %d, want %d", enabled, rec.Code, http.StatusOK)
		}
		body := rec.Body.String()
		if strings.Contains(body, "dist/chunks/webrtc.js") {
			t.Fatalf("enabled=%v: stable-named WebRTC chunk must not be treated as a versioned shell URL", enabled)
		}
		bodies = append(bodies, body)
	}
	if bodies[0] != bodies[1] {
		t.Fatal("WebRTC feature flag changed service-worker chunk cache semantics")
	}
}
