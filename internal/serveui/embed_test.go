package serveui

import (
	"strings"
	"testing"
)

func TestStaticAssetsSupportEmbeddedVideoPlayback(t *testing.T) {
	renderJS, err := StaticAsset("app-render.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-render.js): %v", err)
	}
	renderSrc := string(renderJS)
	for _, want := range []string{
		"ADD_TAGS: ['video', 'source']",
		"ADD_ATTR: ['controls', 'playsinline', 'muted', 'loop', 'autoplay', 'poster', 'preload']",
		"const deferEmbeddedVideos = (target) => {",
		"button.textContent = 'Load video'",
		"replacement.setAttribute('preload', 'metadata')",
	} {
		if !strings.Contains(renderSrc, want) {
			t.Fatalf("app-render.js missing %q", want)
		}
	}

	css, err := StaticAsset("app.css")
	if err != nil {
		t.Fatalf("StaticAsset(app.css): %v", err)
	}
	cssSrc := string(css)
	for _, want := range []string{
		".markdown-body video",
		".deferred-video",
		".deferred-video-btn",
	} {
		if !strings.Contains(cssSrc, want) {
			t.Fatalf("app.css missing %q", want)
		}
	}
}

func TestStaticAssetsSupportSessionStreamDetachOnSwitch(t *testing.T) {
	sessionsJS, err := StaticAsset("app-sessions.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-sessions.js): %v", err)
	}
	sessionsSrc := string(sessionsJS)
	for _, want := range []string{
		"const switchToSession = async (sessionId, options = {}) => {",
		"if (state.currentStreamSessionId && state.currentStreamSessionId !== nextId) {",
		"detachResponseStream();",
	} {
		if !strings.Contains(sessionsSrc, want) {
			t.Fatalf("app-sessions.js missing %q", want)
		}
	}

	streamJS, err := StaticAsset("app-stream.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-stream.js): %v", err)
	}
	streamSrc := string(streamJS)
	for _, want := range []string{
		"const attachResponseStream = (session, responseId = '', controller = null) => {",
		"const detachResponseStream = () => {",
		"state.currentStreamSessionId = String(session?.id || '').trim();",
	} {
		if !strings.Contains(streamSrc, want) {
			t.Fatalf("app-stream.js missing %q", want)
		}
	}
}
