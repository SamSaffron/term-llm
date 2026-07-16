package serveui

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSideQuestionOverlayBehaviorJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping side-question JS tests")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "side_question_test.js")
	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("side_question_test.js failed: %v", err)
	}
}

func TestSideQuestionOverlayAssetsAreWiredAndIsolated(t *testing.T) {
	index, err := StaticAsset("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="sideQuestionOverlay"`, `src="side-question.js"`, `id="messages"`, `id="sideQuestionTranscript"`, `id="sideQuestionInput"`, `placeholder="Ask a follow-up…"`} {
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
		"renderAssistantMarkdown(answerBody", "sideQuestionComposer", "sideQuestionInput",
		"if (side.running)", "elements.sideQuestionInput.value = ''",
	} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("side-question.js missing %q", want)
		}
	}
	jsSource := string(js)
	if strings.Contains(jsSource, "innerHTML") {
		t.Fatal("side transcript bypasses the shared sanitized Markdown renderer")
	}
	if strings.Index(jsSource, "entries.forEach") > strings.Index(jsSource, "appendExchange(transcript, side.question") {
		t.Fatal("side transcript does not append history before the active exchange")
	}
	stream, err := StaticAsset("app-stream.js")
	if err != nil {
		t.Fatal(err)
	}
	streamSource := string(stream)
	if !strings.Contains(streamSource, `/^\/side(?:\s|$)/i.test(prompt)`) {
		t.Fatal("web composer does not intercept /side before main submission")
	}
	if !strings.Contains(streamSource, "elements.promptInput.value = ''") {
		t.Fatal("submitted /side command is not cleared from the main composer")
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
