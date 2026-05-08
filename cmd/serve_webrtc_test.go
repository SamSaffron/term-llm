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

func TestRenderIndexHTML_DropsWebRTCScriptWhenDisabled(t *testing.T) {
	s := &serveServer{
		cfg:               serveServerConfig{basePath: "/ui"},
		webrtcHeadSnippet: "",
	}
	html := string(s.renderIndexHTML())
	if strings.Contains(html, "app-webrtc.js") {
		t.Error("renderIndexHTML should omit app-webrtc.js script tag when WebRTC is disabled")
	}
}

func TestRenderIndexHTML_KeepsWebRTCScriptWhenEnabled(t *testing.T) {
	snippet := `<script>window.__WEBRTC_ENABLED__=true;</script>`
	s := &serveServer{
		cfg:               serveServerConfig{basePath: "/ui"},
		webrtcEnabled:     true,
		webrtcHeadSnippet: snippet,
	}
	html := string(s.renderIndexHTML())
	if !strings.Contains(html, "app-webrtc.js") {
		t.Error("renderIndexHTML should include app-webrtc.js script tag when WebRTC is enabled")
	}
}

func TestHandleUIServiceWorkerWebRTCAssetOption(t *testing.T) {
	tests := []struct {
		name          string
		webrtcEnabled bool
		wantWebRTC    bool
	}{
		{name: "disabled", webrtcEnabled: false, wantWebRTC: false},
		{name: "enabled", webrtcEnabled: true, wantWebRTC: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &serveServer{
				cfg:           serveServerConfig{ui: true, basePath: "/ui"},
				webrtcEnabled: tt.webrtcEnabled,
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)

			s.handleUI(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			body := rec.Body.String()
			hasWebRTC := strings.Contains(body, "app-webrtc.js")
			if hasWebRTC != tt.wantWebRTC {
				t.Fatalf("app-webrtc.js presence = %v, want %v", hasWebRTC, tt.wantWebRTC)
			}
			if strings.Contains(body, "term-llm:webrtc-shell-asset") {
				t.Fatal("service worker response should not contain WebRTC placeholder")
			}
		})
	}
}
