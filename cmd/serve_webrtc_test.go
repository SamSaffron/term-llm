package cmd

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/serveui"
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
		webrtcHeadSnippet: snippet,
	}
	html := string(s.renderIndexHTML())
	if !strings.Contains(html, "app-webrtc.js") {
		t.Error("renderIndexHTML should include app-webrtc.js script tag when WebRTC is enabled")
	}
}

func TestDropScriptTagContaining(t *testing.T) {
	html := []byte(`<body>
  <script src="app-core.js?v=abc"></script>
  <script src="app-webrtc.js?v=abc"></script>
  <script src="app-sessions.js?v=abc"></script>
</body>`)
	got := dropScriptTagContaining(html, "app-webrtc.js")
	if strings.Contains(string(got), "app-webrtc.js") {
		t.Errorf("app-webrtc.js should have been removed, got:\n%s", got)
	}
	if !strings.Contains(string(got), "app-core.js") {
		t.Error("app-core.js should remain")
	}
	if !strings.Contains(string(got), "app-sessions.js") {
		t.Error("app-sessions.js should remain")
	}
}

func TestDropScriptTagContaining_NeedleAbsent(t *testing.T) {
	html := []byte(`<body><script src="app-core.js"></script></body>`)
	got := dropScriptTagContaining(html, "app-webrtc.js")
	if string(got) != string(html) {
		t.Errorf("html should be unchanged when needle absent")
	}
}

func TestDropSWShellAssetContaining(t *testing.T) {
	sw := serveui.RenderServiceWorker()
	if !strings.Contains(string(sw), "app-webrtc.js") {
		t.Fatal("RenderServiceWorker should contain app-webrtc.js entry")
	}
	stripped := dropSWShellAssetContaining(sw, "app-webrtc.js")
	if strings.Contains(string(stripped), "app-webrtc.js") {
		t.Error("app-webrtc.js should have been removed from SHELL_ASSETS")
	}
	// Other assets should remain intact.
	if !strings.Contains(string(stripped), "app-core.js") {
		t.Error("app-core.js should remain in SHELL_ASSETS")
	}
	if !strings.Contains(string(stripped), "app-sessions.js") {
		t.Error("app-sessions.js should remain in SHELL_ASSETS")
	}
}
