package serveui

import (
	"strings"
	"testing"
)

func TestSideQuestionOverlayAssetsAreWiredAndIsolated(t *testing.T) {
	index, err := StaticAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="sideQuestionOverlay"`, `src="side-question.js"`, `id="messages"`, `side-question-response markdown-body`} {
		if !strings.Contains(string(index), want) {
			t.Fatalf("index missing %q", want)
		}
	}
	js, err := StaticAsset("side-question.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/api/sessions/", "side-question", "'/active'", "'/history'",
		"side.visible = false", "getActiveSession()", "method: 'DELETE'",
		"renderAssistantMarkdown(elements.sideQuestionResponse",
	} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("side-question.js missing %q", want)
		}
	}
	stream, err := StaticAsset("app-stream.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stream), `/^\/side(?:\s|$)/i.test(prompt)`) {
		t.Fatal("web composer does not intercept /side before main submission")
	}
	css, err := StaticAsset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"width: min(1200px, calc(100vw - 48px))",
		"height: min(86vh, 900px)",
		"flex: 1 1 auto",
		"min-height: 0",
	} {
		if !strings.Contains(string(css), want) {
			t.Fatalf("side-question CSS missing responsive reading-surface rule %q", want)
		}
	}
	sw, err := StaticAsset("sw.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sw), "'./side-question.js'") {
		t.Fatal("service worker shell omits side-question.js")
	}
}
