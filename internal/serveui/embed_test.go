package serveui

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

func TestStaticAssetsEmbedProductionFilesOnly(t *testing.T) {
	embedded := make(map[string]bool)
	var testFixtures []string
	err := fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		embedded[path] = true
		if strings.HasSuffix(path, "_test.js") {
			testFixtures = append(testFixtures, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded static assets: %v", err)
	}
	if len(testFixtures) > 0 {
		sort.Strings(testFixtures)
		t.Fatalf("embedded JS test fixtures: %s", strings.Join(testFixtures, ", "))
	}
	if _, err := StaticAsset("app_stream_test.js"); err == nil {
		t.Fatal("StaticAsset(app_stream_test.js) succeeded; JS test fixtures should not be embedded")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	testDir := filepath.Dir(thisFile)
	var missing []string
	err = filepath.WalkDir(filepath.Join(testDir, "static"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(testDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.js") {
			return nil
		}
		if !embedded[rel] {
			missing = append(missing, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source static assets: %v", err)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("production static assets not embedded: %s", strings.Join(missing, ", "))
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

func TestAppRenderJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS app-render tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "app_render_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("app_render_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("app_render_test.js failed: %v", err)
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

func TestAppSidebarJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS app-sidebar tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "app_sidebar_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("app_sidebar_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("app_sidebar_test.js failed: %v", err)
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

func TestChipPickerJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH, skipping JS chip-picker tests")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "static", "chip_picker_test.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("chip_picker_test.js not found at %s: %v", script, err)
	}

	out, err := exec.Command(node, script).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("chip_picker_test.js failed: %v", err)
	}
}

func TestAppWebRTCDoesNotReferenceLexicalApp(t *testing.T) {
	webrtcJS, err := StaticAsset("app-webrtc.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-webrtc.js): %v", err)
	}
	src := string(webrtcJS)
	for _, bad := range []string{
		"app?.",
		"typeof setConnectionState",
	} {
		if strings.Contains(src, bad) {
			t.Fatalf("app-webrtc.js contains unsafe global reference %q", bad)
		}
	}
	if !strings.Contains(src, "window.TermLLMApp") {
		t.Fatalf("app-webrtc.js should access app exports through window.TermLLMApp")
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

func TestStaticAssetsSupportTurnCopyActionPanel(t *testing.T) {
	renderJS, err := StaticAsset("app-render.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-render.js): %v", err)
	}
	renderSrc := string(renderJS)
	for _, want := range []string{
		"const getAssistantTurns = (session) => {",
		"const buildTurnClipboardText = (turn) => {",
		"button.className = 'turn-action-btn turn-copy-btn'",
		"navigator.clipboard",
		"clipboard.writeText(text)",
		"syncTurnActionPanels",
	} {
		if !strings.Contains(renderSrc, want) {
			t.Fatalf("app-render.js missing %q", want)
		}
	}

	streamJS, err := StaticAsset("app-stream.js")
	if err != nil {
		t.Fatalf("StaticAsset(app-stream.js): %v", err)
	}
	if !strings.Contains(string(streamJS), "syncTurnActionPanels") {
		t.Fatal("app-stream.js missing syncTurnActionPanels integration")
	}

	css, err := StaticAsset("app.css")
	if err != nil {
		t.Fatalf("StaticAsset(app.css): %v", err)
	}
	cssSrc := string(css)
	for _, want := range []string{
		".turn-action-panel",
		".turn-action-btn",
		".turn-action-btn.copied",
		"@keyframes copy-success-pop",
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
		"const currentEffort = session.activeEffort || ''",
		"body.reasoning_effort = activeEffort",
		"body.model_swap = { mode: 'auto', fallback: 'handover' }",
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
	for _, forbidden := range []string{
		`src="vendor/katex/katex.min.js?v=0.16.38"`,
		`src="vendor/hljs/highlight.min.js?v=11.11.1"`,
		`href="vendor/katex/katex.min.css?v=0.16.38"`,
	} {
		if strings.Contains(indexSrc, forbidden) {
			t.Fatalf("index.html should lazy-load optional markdown asset %q", forbidden)
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
		"function findStableMarkdownBoundary(",
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
		"markdown-stream-stable",
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

func TestRenderIndexHTMLWebRTCOption(t *testing.T) {
	disabled := string(RenderIndexHTML("/ui", "", RenderOptions{}))
	if strings.Contains(disabled, "app-webrtc.js") {
		t.Fatal("disabled WebRTC option should omit app-webrtc.js")
	}
	if strings.Contains(disabled, "term-llm:webrtc-script") {
		t.Fatal("disabled WebRTC option should remove the WebRTC script placeholder")
	}

	enabled := string(RenderIndexHTML("/ui", "", RenderOptions{WebRTC: true}))
	if !strings.Contains(enabled, "app-webrtc.js?v=") {
		t.Fatal("enabled WebRTC option should include versioned app-webrtc.js")
	}
	if strings.Contains(enabled, "term-llm:webrtc-script") {
		t.Fatal("enabled WebRTC option should remove the WebRTC script placeholder")
	}
}

func TestRenderServiceWorkerWebRTCOption(t *testing.T) {
	disabled := string(RenderServiceWorker(RenderOptions{}))
	if strings.Contains(disabled, "app-webrtc.js") {
		t.Fatal("disabled WebRTC option should omit app-webrtc.js from service worker")
	}
	if strings.Contains(disabled, "term-llm:webrtc-shell-asset") {
		t.Fatal("disabled WebRTC option should remove the WebRTC shell asset placeholder")
	}

	enabled := string(RenderServiceWorker(RenderOptions{WebRTC: true}))
	if !strings.Contains(enabled, "app-webrtc.js?v=") {
		t.Fatal("enabled WebRTC option should include versioned app-webrtc.js")
	}
	if strings.Contains(enabled, "term-llm:webrtc-shell-asset") {
		t.Fatal("enabled WebRTC option should remove the WebRTC shell asset placeholder")
	}
}

func TestRenderedStaticAssetsAreCached(t *testing.T) {
	manifest1 := RenderManifest()
	manifest2 := RenderManifest()
	if len(manifest1) == 0 || len(manifest2) == 0 {
		t.Fatal("RenderManifest returned empty output")
	}
	if &manifest1[0] != &manifest2[0] {
		t.Fatal("RenderManifest should return cached output")
	}

	sw1 := RenderServiceWorker(RenderOptions{})
	sw2 := RenderServiceWorker(RenderOptions{})
	if len(sw1) == 0 || len(sw2) == 0 {
		t.Fatal("RenderServiceWorker returned empty output")
	}
	if &sw1[0] != &sw2[0] {
		t.Fatal("RenderServiceWorker should return cached output for the same options")
	}

	webrtc1 := RenderServiceWorker(RenderOptions{WebRTC: true})
	webrtc2 := RenderServiceWorker(RenderOptions{WebRTC: true})
	if len(webrtc1) == 0 || len(webrtc2) == 0 {
		t.Fatal("RenderServiceWorker with WebRTC returned empty output")
	}
	if &webrtc1[0] != &webrtc2[0] {
		t.Fatal("RenderServiceWorker should cache WebRTC output separately")
	}
	if &sw1[0] == &webrtc1[0] {
		t.Fatal("RenderServiceWorker should keep WebRTC and non-WebRTC outputs separate")
	}
}

func TestRenderIndexHTML_PreloadHintsMatchScriptSrc(t *testing.T) {
	html := string(RenderIndexHTML("/", "", RenderOptions{}))
	if html == "" {
		t.Fatal("RenderIndexHTML returned empty")
	}

	scripts := []string{"app-core.js", "app-render.js", "app-stream.js", "app-sessions.js"}
	for _, name := range scripts {
		// Find the versioned URL used in the <script src="..."> tag.
		srcMarker := `src="`
		idx := strings.Index(html, srcMarker+name)
		if idx < 0 {
			t.Fatalf("no <script src=%q> found in rendered HTML", name)
		}
		start := idx + len(srcMarker)
		end := strings.Index(html[start:], `"`)
		if end < 0 {
			t.Fatalf("unterminated src attribute for %q", name)
		}
		scriptURL := html[start : start+end]

		// Find the preload href for the same file.
		hrefMarker := `href="`
		pidx := strings.Index(html, hrefMarker+name)
		if pidx < 0 {
			t.Fatalf("no preload href=%q found in rendered HTML", name)
		}
		pstart := pidx + len(hrefMarker)
		pend := strings.Index(html[pstart:], `"`)
		if pend < 0 {
			t.Fatalf("unterminated href attribute for preload %q", name)
		}
		preloadURL := html[pstart : pstart+pend]

		if scriptURL != preloadURL {
			t.Fatalf("%s: script src=%q but preload href=%q (URLs must match for browser to reuse fetch)", name, scriptURL, preloadURL)
		}
	}
}
