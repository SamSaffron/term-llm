package serveui

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestStaticAssetsUseStrictMathDelimiters(t *testing.T) {
	coreJS, err := StaticAsset("app-core.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-core.js): %v", err)
	}
	coreSrc := string(coreJS)
	for _, want := range []string{
		"const MATH_DELIMITERS = [",
		"{ left: '$$', right: '$$', display: true }",
		"{ left: '\\\\[', right: '\\\\]', display: true }",
		"{ left: '\\\\(', right: '\\\\)', display: false }",
		"delimiters: MATH_DELIMITERS,",
	} {
		if !strings.Contains(coreSrc, want) {
			t.Fatalf("app-core.js missing %q", want)
		}
	}
	if strings.Contains(coreSrc, "{ left: '$', right: '$', display: false }") {
		t.Fatal("app-core.js still enables single-dollar inline math")
	}
}

func TestMarkdownSetupJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS markdown tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "markdown_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("markdown_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("markdown_test.js failed: %v", err)
	}
}

func TestMarkdownStreamingJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS markdown streaming tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "markdown_streaming_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("markdown_streaming_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("markdown_streaming_test.js failed: %v", err)
	}
}

func TestDecorationJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS decoration tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "decoration_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("decoration_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("decoration_test.js failed: %v", err)
	}
}

func TestAppStreamJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS app-stream tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "app_stream_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("app_stream_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("app_stream_test.js failed: %v", err)
	}
}

func TestAppSessionsJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS app-sessions tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "app_sessions_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("app_sessions_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("app_sessions_test.js failed: %v", err)
	}
}

func TestStaticAssetsSupportCodeBlockUX(t *testing.T) {
	renderJS, err := StaticAsset("app-render.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-render.js): %v", err)
	}
	renderSrc := string(renderJS)
	for _, want := range []string{
		`/\blanguage-\w+/.test(code.className)`,
		`btn.className = 'code-copy-btn'`,
		`navigator.clipboard.writeText(text)`,
		`btn.classList.add('copied')`,
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
		".code-copy-btn",
		".code-copy-btn.copied",
		".markdown-body pre:hover .code-copy-btn",
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

func TestStaticAssetsSupportEffortDropdown(t *testing.T) {
	indexHTML, err := StaticAsset("index.html")
	if err != nil {
		t.Fatalf("StaticAsset(index.html): %v", err)
	}
	indexSrc := string(indexHTML)
	for _, want := range []string{
		`id="effortSelect"`,
		`<option value="minimal">minimal</option>`,
		`<option value="low">low</option>`,
		`<option value="medium">medium</option>`,
		`<option value="high">high</option>`,
		`<option value="xhigh">xhigh</option>`,
		`<option value="max">max</option>`,
	} {
		if !strings.Contains(indexSrc, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	// "default" was removed because it is redundant with the empty "Auto
	// (server default)" option and was rejected by every upstream provider.
	if strings.Contains(indexSrc, `<option value="default">`) {
		t.Fatalf(`index.html must not offer an effort "default" option (redundant with "" and rejected by providers)`)
	}

	coreJS, err := StaticAsset("app-core.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-core.js): %v", err)
	}
	coreSrc := string(coreJS)
	for _, want := range []string{
		"selectedEffort: 'term_llm_selected_effort'",
		"selectedEffort: localStorage.getItem(STORAGE_KEYS.selectedEffort) || ''",
		"effortSelect: document.getElementById('effortSelect')",
	} {
		if !strings.Contains(coreSrc, want) {
			t.Fatalf("app-core.js missing %q", want)
		}
	}

	streamJS, err := StaticAsset("app-stream.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-stream.js): %v", err)
	}
	streamSrc := string(streamJS)
	for _, want := range []string{
		"elements.effortSelect.value = state.selectedEffort",
		"localStorage.setItem(STORAGE_KEYS.selectedEffort, newEffort)",
		"localStorage.removeItem(STORAGE_KEYS.selectedEffort)",
		"const activeEffort = session.activeEffort || state.selectedEffort",
		"body.reasoning_effort = activeEffort",
	} {
		if !strings.Contains(streamSrc, want) {
			t.Fatalf("app-stream.js missing %q", want)
		}
	}
	// Effort must not commit on change — Cancel has to discard the pending
	// value. Commit happens only inside connectToken() on Save.
	if strings.Contains(streamSrc, "elements.effortSelect?.addEventListener('change'") {
		t.Fatalf("app-stream.js must not wire a change listener on effortSelect (would persist pending value on Cancel)")
	}
}

func TestStaticAssetsSupportIncrementalMarkdownStreaming(t *testing.T) {
	indexHTML, err := StaticAsset("index.html")
	if err != nil {
		t.Fatalf("StaticAsset(index.html): %v", err)
	}
	indexSrc := string(indexHTML)
	for _, want := range []string{
		`src="markdown-streaming.js"`,
		`id="sidebarToggleBtn"`,
		`id="sidebarRailNewChatBtn"`,
	} {
		if !strings.Contains(indexSrc, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}

	streamingJS, err := StaticAsset("markdown-streaming.js")
	if err != nil {
		t.Fatalf("StaticAsset(markdown-streaming.js): %v", err)
	}
	streamingSrc := string(streamingJS)
	for _, want := range []string{
		"function nextStreamingRenderDelay(",
		"function areMathDelimitersBalanced(",
		"function canStreamPlainTextTail(",
	} {
		if !strings.Contains(streamingSrc, want) {
			t.Fatalf("markdown-streaming.js missing %q", want)
		}
	}

	renderJS, err := StaticAsset("app-render.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-render.js): %v", err)
	}
	renderSrc := string(renderJS)
	for _, want := range []string{
		"const enqueueAssistantStreamUpdate = (message) => {",
		"const finalizeAssistantStreamRender = (message) => {",
		"const renderAssistantTailPlainText = (streamState, tail) => {",
	} {
		if !strings.Contains(renderSrc, want) {
			t.Fatalf("app-render.js missing %q", want)
		}
	}

	coreJS, err := StaticAsset("app-core.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-core.js): %v", err)
	}
	coreSrc := string(coreJS)
	for _, want := range []string{
		"sidebarCollapsed: localStorage.getItem(STORAGE_KEYS.sidebarCollapsed) === '1'",
		"sidebarCollapsed: 'term_llm_sidebar_collapsed'",
		"sidebarRailNewChatBtn: document.getElementById('sidebarRailNewChatBtn')",
	} {
		if !strings.Contains(coreSrc, want) {
			t.Fatalf("app-core.js missing %q", want)
		}
	}

	sessionsJS, err := StaticAsset("app-sessions.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-sessions.js): %v", err)
	}
	sessionsSrc := string(sessionsJS)
	for _, want := range []string{
		"elements.sidebarToggleBtn.addEventListener('click', toggleSidebarCollapsed);",
		"elements.sidebarRailNewChatBtn.addEventListener('click', async () => {",
		"document.addEventListener('visibilitychange', async () => {",
		"flushStreamPersistence();",
	} {
		if !strings.Contains(sessionsSrc, want) {
			t.Fatalf("app-sessions.js missing %q", want)
		}
	}

	swJS, err := StaticAsset("sw.js")
	if err != nil {
		t.Fatalf("StaticAsset(sw.js): %v", err)
	}
	if !strings.Contains(string(swJS), "'./markdown-streaming.js'") {
		t.Fatal("sw.js missing markdown-streaming.js shell asset")
	}

	css, err := StaticAsset("app.css")
	if err != nil {
		t.Fatalf("StaticAsset(app.css): %v", err)
	}
	cssSrc := string(css)
	for _, want := range []string{
		"--sidebar-width: 280px;",
		"--sidebar-rail-width: 56px;",
		".app.sidebar-collapsed {",
		".sidebar-rail {",
	} {
		if !strings.Contains(cssSrc, want) {
			t.Fatalf("app.css missing %q", want)
		}
	}
}
