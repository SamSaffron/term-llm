package serveui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	pathpkg "path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestGeneratedBundleAssets(t *testing.T) {
	for _, name := range []string{
		"dist/app.js", "dist/app.css", "dist/hub.js", "dist/hub.css", "dist/chunks/vendor.js",
		"dist/chunks/rich-highlight.js", "dist/chunks/rich-katex.js",
		"dist/chunks/highlight.js", "dist/chunks/katex.js", "dist/chunks/katex.css", "dist/chunks/webrtc.js", "dist/chunks/mcp.js",
		"dist/chunks/MarkdownFilePreview.js", "dist/chunks/markdown-preview-store.js",
		"dist/chunks/ShellOverlay.js", "dist/chunks/markdown-document.js", "dist/chunks/file-text.js",
		"dist/assets/MarkdownFilePreview.css", "dist/assets/ShellOverlay.css",
	} {
		body, err := testBuildAsset(name)
		if err != nil {
			t.Fatalf("testBuildAsset(%q): %v", name, err)
		}
		if len(body) == 0 {
			t.Fatalf("testBuildAsset(%q) is empty", name)
		}
	}
	if _, err := testBuildAsset("dist/app.js.map"); err == nil {
		t.Fatal("production source map must not be embedded")
	}
	if _, err := testBuildAsset("dist/hub.js.map"); err == nil {
		t.Fatal("Hub production source map must not be embedded")
	}
}

func TestGeneratedImportsPreloadsAndCSSURLsResolveToEmbeddedAssets(t *testing.T) {
	assetReference := regexp.MustCompile(`(?:["']|url\()((?:\.\.?/)[^"'()]+\.(?:js|css|woff2|png|svg))["') ]`)
	err := fs.WalkDir(staticFiles, "static/dist", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (!strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".css")) {
			return err
		}
		body, err := fs.ReadFile(staticFiles, name)
		if err != nil {
			return err
		}
		assetName := strings.TrimPrefix(name, "static/")
		for _, match := range assetReference.FindAllSubmatch(body, -1) {
			resolved := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(assetName), string(match[1])))
			if _, err := StaticAsset(resolved); err != nil {
				t.Errorf("%s references missing graph asset %q: %v", assetName, resolved, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProductionBundleSizeBudgets(t *testing.T) {
	budgets := map[string]struct{ raw, gzip int }{
		// Notification reconciliation/outbox messaging, the owned mobile voice
		// state machine, gallery/diff navigation, focused store modules, the
		// safe-point branch workflow, interactive worktree conflict recovery,
		// replayable SSE/long-poll server-event coordination, branch-tree
		// navigation, the paginated Recent/Projects sidebar, elastic streaming
		// presentation buffer, explicit response authority/transport state,
		// session approval-policy status/commands and their lazy settings modal,
		// authoritative mobile stream recovery, widget process lifecycle controls,
		// model capability-aware runtime controls, durable terminal/input-required
		// attention reconciliation, native commit review/editor controls, the
		// resumable server-authoritative shell collaboration transport/store,
		// inline tool media presentation, and the small eager Markdown review
		// toggle/loader are first-party shell code. The xterm terminal, preview body,
		// parser, source transport, and feature CSS remain in bounded lazy chunks.
		// Keep bounded headroom while still failing meaningful accidental regressions.
		// The bounded approval dialog, keyboard controls, and isolated streaming,
		// diff/voice boundaries, atomic rich-content preparation, session readiness,
		// and incremental transcript indexes are included in the post-cleanup
		// baseline of ~491.8 kB raw. Extension recovery, ordered loading and the
		// lazy Interface-settings entry add ~3.9 kB raw; the panel and its CSS
		// load only on demand. Allow modest headroom for the combined features
		// with bounded CSS headroom for responsive steering controls. Token-gate
		// wiring, verified credential replacement, and startup retry bring the
		// eager shell to ~499.4 kB; the gate UI and its CSS remain lazy-loaded.
		// Index-backed infinite history loading, revision/cancellation guards, and
		// visible-row anchoring and extension context snapshots bring the eager
		// shell to ~504.2/143.4 kB raw/gzip. Native picker touch containment
		// brings the shell to ~505.3/143.7 kB raw/gzip.
		// /stats command/API wiring and its lazy loader bring the eager shell
		// to ~506.6/144.0 kB. The modal and CSS stay separately bounded below.
		// The image viewer and its styles now load on demand. Its small loader
		// leaves the eager shell at ~503.2/142.9 kB raw/gzip, with separate budgets below.
		// Inline delegation progress is eager transcript code. Live semantic activity,
		// shared-clock quiet-state labels, and collapsed running previews bring the
		// eager shell to ~504.5/142.7 kB raw/gzip. Retain narrow raw headroom; the
		// compressed cap remains unchanged.
		// First-seen live transcript ordering adds ~0.74 kB to the eager store,
		// bringing it to ~505.52 kB. The history panel remains lazy.
		// Downloadable attachment chips add six inline MIME glyphs plus the
		// MIME/extension mapping tables to the eager transcript and composer,
		// bringing the shell to ~509.5/143.2 kB raw/gzip. The glyphs are inline
		// SVG in the shared registry, so no sprite asset or icon dependency
		// enters the graph.
		// The live voice session control plane adds the call-binding signals and
		// session_changed follow-along to the eager live store, the in-place
		// rebind/selection path to the app store, the panel's binding line, and
		// the superseded-echo/generation guards that keep a stale switch from
		// walking the UI backwards, bringing the shell to ~514.1/145.5 kB
		// raw/gzip.
		//
		// Headroom is set deliberately here rather than raised a little at a time.
		// The live-voice session work landed in three steps and each one left the
		// cap a few hundred bytes above the measurement, which stops being a budget
		// and becomes a tripwire: the next unrelated change fails on size without
		// having grown anything meaningfully. These caps sit ~1% above the measured
		// 514.1/144.2 kB, which still fails an accidental regression while leaving
		// room for an ordinary change.
		//
		// Subagent drill-down adds the child-session store (live tail polling,
		// attempt-discard handling and overflow recovery), the delegation context
		// surface, the subagent index and the delegated composer modes, bringing
		// the shell to ~535.1/151.3 kB. Same ~1% rule as above.
		//
		// Page-provided (WebMCP) tools add the eager tool discovery store, the
		// client-tool runner, and the run engine's tool-output continuation;
		// discovery must run at startup, so none of it can be lazy. That brings
		// the shell to ~527.0/153.9 kB raw/gzip. Same ~1% rule as above.
		//
		// Later conversation-management work left the shell at 534.5/155.9 kB,
		// already over that compressed cap. Server-ordered pinned conversations
		// then add the pinned section's pointer/touch drag, keyboard and menu
		// moves, and the store's optimistic save and rollback. The section is on
		// screen at startup, so the code is eager: ~542.1/158.4 kB. Same ~1%
		// rule as above.
		//
		// Server-ordered projects generalize that drag and optimistic save into
		// shared list and store helpers, and add the project header drag and the
		// keyboard and menu moves. The Projects view is on screen at startup too:
		// ~546.9/160.0 kB. Same ~1% rule as above.
		// The proxied node's Agents sidebar now follows the Hub's saved node order
		// and offers link drag, keyboard/menu moves, optimistic saves, and stale
		// read protection. Those controls are needed at startup: ~552.0/161.6 kB
		// raw/gzip, with roughly the same modest headroom as the other budgets.
		// Fast reopen restores the last-known workspace before any network
		// response, so the bounded IndexedDB cache, the workspace/discovery
		// records and their sanitizers must be in the entry: ~566.8/166.4 kB.
		// Modals opened by explicit user actions (settings, rename, goal, widgets,
		// branch paths, skills, project picker/assignment, worktrees) now load on
		// demand; agent approval/ask-user prompts and the side question stay
		// eager. That leaves the entry at ~527.3/155.2 kB. Same ~1% rule as above.
		// Detached-agent wait/continue cards keep validated child links through
		// live results and durable reloads; their small eager transcript parser
		// adds a little to the compressed shell.
		"dist/app.js":                {raw: 533_000, gzip: 157_000},
		"dist/chunks/Lightbox.js":    {raw: 8_000, gzip: 3_200},
		"dist/assets/Lightbox.css":   {raw: 4_000, gzip: 1_400},
		"dist/chunks/StatsModal.js":  {raw: 8_000, gzip: 3_000},
		"dist/assets/StatsModal.css": {raw: 5_000, gzip: 1_600},
		// The MCP servers dialog (flat list, row menu, and the add-server sheet
		// with catalogue/URL/command sources) measured 14.0/5.0 kB JS and
		// 11.1/2.7 kB CSS; it loads only when the dialog opens.
		"dist/chunks/MCPModal.js":  {raw: 15_500, gzip: 5_600},
		"dist/assets/MCPModal.css": {raw: 12_500, gzip: 3_100},
		// The live panel's binding line, the transcript cross-fade that runs when a
		// voice switch swaps the session in place, and the subagent breadcrumb /
		// index / live-tail styling bring the sheet to ~178.1 kB. Same reasoning as
		// above: a cap that close to the measurement fails the next CSS change on
		// arithmetic rather than on bloat.
		"dist/app.css": {raw: 180_000, gzip: 34_000},
		// Measured after the completed standalone port: 67.7/21.5 KiB JS and
		// 16.7/4.1 KiB CSS. These limits retain modest growth headroom without
		// allowing chat-only rendering dependencies into the Hub graph. The
		// native-app sign-in approval flow brought JS to 72.5/23.5 KiB, and
		// steering sign-in off non-passkey origins (127.0.0.1 vs localhost) to
		// 72.6/23.5 KiB.
		//
		// The saved dashboard node order shares the chat's reorderable-list
		// hook (pointer/touch drag, now with grid geometry, page scrolling,
		// and keyboard/menu moves) and its optimistic, serialized saves with
		// rollback, and adds the card controls and store wiring: ~9.4 KiB raw,
		// all of it on screen at startup, bringing JS to 82.1/27.3 KiB. These
		// limits keep the same modest headroom as before.
		// The dashboard now paints a persisted last-known snapshot before any
		// request and applies each section as it lands; the shared IndexedDB
		// cache plus field allowlists add ~9.8 KiB: 97.9/31.9 KiB.
		// The online/offline/all node filter menu, its URL sync, and the
		// filtered-grid order merge take JS from 97.5/30.9 to 100.7/31.7 KiB
		// and CSS from 17.9 to 19.3 KiB.
		"dist/hub.js":  {raw: 105_000, gzip: 34_000},
		"dist/hub.css": {raw: 20_500, gzip: 5_500},
	}
	for name, budget := range budgets {
		body, err := testBuildAsset(name)
		if err != nil {
			t.Fatal(err)
		}
		var compressed bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&compressed, 6)
		_, _ = writer.Write(body)
		_ = writer.Close()
		if len(body) > budget.raw || compressed.Len() > budget.gzip {
			t.Errorf("%s size raw/gzip = %d/%d, budget %d/%d", name, len(body), compressed.Len(), budget.raw, budget.gzip)
		}
	}
}

func TestBundlePolicy(t *testing.T) {
	js, err := testBuildAsset("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"window.TermLLMApp", "preact/compat", "legacy-bridge", "./assets/highlight.css", "./assets/katex.css", "@xterm/xterm", "xterm-rows"} {
		if bytes.Contains(js, []byte(forbidden)) {
			t.Errorf("production bundle contains forbidden compatibility path %q", forbidden)
		}
	}
	if bytes.Contains(js, []byte("__TERM_LLM_TEST__")) || bytes.Contains(js, []byte("__TERM_LLM_ENABLE_TEST_BRIDGE__")) {
		t.Fatal("production bundle contains browser test bridge")
	}
}

func TestLightboxAssetsRemainLazy(t *testing.T) {
	for _, asset := range []struct{ eager, lazy, marker string }{
		{"dist/app.js", "dist/chunks/Lightbox.js", "lightbox-view-controls"},
		{"dist/app.css", "dist/assets/Lightbox.css", ".lightbox-toolbar"},
	} {
		eager, err := testBuildAsset(asset.eager)
		if err != nil {
			t.Fatal(err)
		}
		lazy, err := testBuildAsset(asset.lazy)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(eager, []byte(asset.marker)) {
			t.Errorf("%s unexpectedly contains image viewer code/styles", asset.eager)
		}
		if !bytes.Contains(lazy, []byte(asset.marker)) {
			t.Errorf("%s is missing image viewer code/styles", asset.lazy)
		}
	}
}

func TestCommitDialogRemainsLazy(t *testing.T) {
	eager, err := testBuildAsset("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	lazy, err := testBuildAsset("dist/chunks/CommitModal.js")
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("Commit is disabled until repository status and staged files are reviewed again.")
	if bytes.Contains(eager, marker) {
		t.Error("dist/app.js unexpectedly contains commit dialog code")
	}
	if !bytes.Contains(lazy, marker) {
		t.Error("dist/chunks/CommitModal.js is missing commit dialog code")
	}
}

func TestMCPDialogAssetsRemainLazy(t *testing.T) {
	for _, asset := range []struct{ eager, lazy, marker string }{
		{"dist/app.js", "dist/chunks/MCPModal.js", "Local command"},
		{"dist/app.css", "dist/assets/MCPModal.css", ".mcp-row-menu"},
	} {
		eager, err := testBuildAsset(asset.eager)
		if err != nil {
			t.Fatal(err)
		}
		lazy, err := testBuildAsset(asset.lazy)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(eager, []byte(asset.marker)) {
			t.Errorf("%s unexpectedly contains MCP dialog code/styles", asset.eager)
		}
		if !bytes.Contains(lazy, []byte(asset.marker)) {
			t.Errorf("%s is missing MCP dialog code/styles", asset.lazy)
		}
	}
}

// Modals opened by explicit user actions load on demand; a static import would
// fold one back into the entry. Agent approval/ask-user prompts stay eager.
func TestUserOpenedModalsRemainLazy(t *testing.T) {
	eager, err := testBuildAsset("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, modal := range []struct{ chunk, marker string }{
		{"dist/chunks/SettingsModal.js", "Show archived sessions"},
		{"dist/chunks/RenameModal.js", "Improve title with AI"},
		{"dist/chunks/GoalModal.js", "Set a persistent objective"},
		{"dist/chunks/WidgetsModal.js", "Local widgets open in this tab."},
		{"dist/chunks/BranchModals.js", "Filesystem and tool side effects are not undone."},
		{"dist/chunks/SkillsModal.js", "skill-provenance"},
		{"dist/chunks/ProjectPicker.js", "Defaults to the folder name"},
		{"dist/chunks/ProjectAssignment.js", "Choose a sidebar group."},
		{"dist/chunks/Worktrees.js", "worktree-intro"},
	} {
		lazy, err := testBuildAsset(modal.chunk)
		if err != nil {
			t.Fatalf("testBuildAsset(%q): %v", modal.chunk, err)
		}
		if bytes.Contains(eager, []byte(modal.marker)) {
			t.Errorf("dist/app.js unexpectedly contains %s code (%q)", modal.chunk, modal.marker)
		}
		if !bytes.Contains(lazy, []byte(modal.marker)) {
			t.Errorf("%s is missing its dialog code (%q)", modal.chunk, modal.marker)
		}
	}
}

func TestLiveVoiceCallRemainsLazy(t *testing.T) {
	eager, err := testBuildAsset("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	lazy, err := testBuildAsset("dist/chunks/live.js")
	if err != nil {
		t.Fatal(err)
	}
	// The WebRTC peer for live voice is only needed once a call starts, so it
	// must stay out of the eager shell every browser downloads.
	const marker = "oai-events"
	if bytes.Contains(eager, []byte(marker)) {
		t.Error("dist/app.js unexpectedly contains the live voice WebRTC peer")
	}
	if !bytes.Contains(lazy, []byte(marker)) {
		t.Error("dist/chunks/live.js is missing the live voice WebRTC peer")
	}
	endpoints, err := testBuildAsset("dist/chunks/live-endpoints.js")
	if err != nil {
		t.Fatal(err)
	}
	const audioMarker = "X-Term-LLM-Live-Audio-Capability"
	if bytes.Contains(eager, []byte(audioMarker)) {
		t.Error("dist/app.js unexpectedly contains live audio endpoint configuration")
	}
	if !bytes.Contains(endpoints, []byte(audioMarker)) {
		t.Error("dist/chunks/live-endpoints.js is missing live audio endpoint configuration")
	}
}

func TestHubBundlePolicy(t *testing.T) {
	js, err := testBuildAsset("dist/hub.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"marked", "DOMPurify", "KaTeX", "highlight.js", "@xterm", "WebRTC",
		"TERM_LLM_TEST", "mcp-store", "app-store", "chunks/",
	} {
		if bytes.Contains(js, []byte(forbidden)) {
			t.Errorf("standalone Hub bundle contains forbidden chat dependency marker %q", forbidden)
		}
	}
	if bytes.Contains(js, []byte("sourceMappingURL")) {
		t.Fatal("standalone Hub bundle references a source map")
	}
}

func TestChatAndHubAssetVersionScopes(t *testing.T) {
	versionFor := func(include func(string) bool) string {
		entries := []string{}
		err := fs.WalkDir(staticFiles, "static", func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			if include(path) {
				entries = append(entries, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(entries)
		hash := sha256.New()
		for _, path := range entries {
			body, err := fs.ReadFile(staticFiles, path)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = hash.Write([]byte(path))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write(body)
			_, _ = hash.Write([]byte{0})
		}
		return hex.EncodeToString(hash.Sum(nil))[:12]
	}
	chat := versionFor(func(path string) bool {
		return path != "static/dist/hub.js" && path != "static/dist/hub.css"
	})
	hub := versionFor(func(path string) bool {
		return path == "static/dist/hub.js" || path == "static/dist/hub.css"
	})
	if AssetVersion() != chat {
		t.Fatalf("AssetVersion=%q, want chat-only scope %q", AssetVersion(), chat)
	}
	if HubAssetVersion() != hub {
		t.Fatalf("HubAssetVersion=%q, want exact Hub scope %q", HubAssetVersion(), hub)
	}
	if AssetVersion() == HubAssetVersion() {
		t.Fatal("chat and Hub asset scopes unexpectedly share an identity")
	}
}

func TestLazyChunksReuseCanonicalEntryModuleURL(t *testing.T) {
	importsEntry := false
	for _, name := range []string{
		"dist/chunks/markdown-preview-store.js",
		"dist/chunks/file-text.js",
	} {
		body, err := testBuildAsset(name)
		if err != nil {
			t.Fatal(err)
		}
		importsEntry = importsEntry || bytes.Contains(body, []byte(`../`+strings.TrimPrefix(ChatAssetManifest().EntryJS, "dist/")))
	}
	if !importsEntry {
		t.Fatal("expected a lazy Markdown chunk to exercise the shared entry import contract")
	}
	rendered := string(RenderIndexHTML("/ui", "", RenderOptions{}))
	if !strings.Contains(rendered, `type="module" src="`+ChatAssetManifest().EntryJS+`"`) || strings.Contains(rendered, `src="dist/app.js?v=`) {
		t.Fatal("lazy chunks and the HTML entry must resolve app.js to one canonical module URL")
	}
}

func TestRenderIndexHTMLPreservesBootstrapAndModuleContract(t *testing.T) {
	const bootstrap = `<script>window.TERM_LLM_UI_PREFIX="/chat/nodes/alpha";</script>`
	rendered := string(RenderIndexHTML("/chat/nodes/alpha", bootstrap, RenderOptions{WebRTC: true}))
	for _, want := range []string{
		`<base href="/chat/nodes/alpha/">`,
		bootstrap,
		`href="` + ChatAssetManifest().EntryCSS[0] + `"`,
		`type="module" src="` + ChatAssetManifest().EntryJS + `"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered index missing %q", want)
		}
	}
	if strings.Contains(rendered, `src="dist/app.js?v=`) {
		t.Fatal("entry module must keep the canonical URL imported by lazy chunks")
	}
	if strings.Index(rendered, bootstrap) > strings.Index(rendered, `src="`+ChatAssetManifest().EntryJS) {
		t.Fatal("injected Hub/bootstrap context must precede the deferred module")
	}
	if count := strings.Count(rendered, `<script type="module"`); count != 1 {
		t.Fatalf("first-party module entries = %d, want 1", count)
	}
}

func TestRenderServiceWorkerVersionsOnlyDirectShellAssets(t *testing.T) {
	without := string(RenderServiceWorker(RenderOptions{}))
	with := string(RenderServiceWorker(RenderOptions{WebRTC: true}))
	for _, want := range []string{
		"term-llm-shell-" + AssetVersion(),
		"'./manifest.webmanifest?v=" + AssetVersion() + "'",
		"'./icon-512.png?v=" + AssetVersion() + "'",
	} {
		if !strings.Contains(without, want) {
			t.Errorf("service worker missing %q", want)
		}
	}
	for _, chunk := range []string{"app.js?v=", "vendor.js?v=", "webrtc.js?v=", "highlight.js?v=", "katex.js?v=", "mcp.js?v="} {
		if strings.Contains(without, chunk) || strings.Contains(with, chunk) {
			t.Errorf("canonical stable URL %q must not appear in the hashed graph", chunk)
		}
	}
	if strings.Contains(without, "'./dist/app.js'") || strings.Contains(with, "'./dist/app.js'") {
		t.Fatal("stable entry URL must not appear in the hashed asset list")
	}
	if strings.Contains(without, "hub.js") || strings.Contains(without, "hub.css") || strings.Contains(with, "hub.js") || strings.Contains(with, "hub.css") {
		t.Fatal("standalone Hub assets must stay outside the chat service-worker cache")
	}
	if without != with {
		t.Fatal("WebRTC must not add a versioned URL that differs from the chunk URL imported by Vite")
	}
}

func TestStaticAssetReturnsCopy(t *testing.T) {
	first, err := testBuildAsset("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := testBuildAsset("dist/app.js")
	first[0] ^= 0xff
	if bytes.Equal(first, second) {
		t.Fatal("StaticAsset returned shared mutable storage")
	}
}

func TestRenderedAssetsAreGzipReadable(t *testing.T) {
	body, _ := testBuildAsset("dist/app.js")
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write(body)
	_ = writer.Close()
	reader, err := gzip.NewReader(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(decoded, body) {
		t.Fatalf("gzip round trip failed: %v", err)
	}
}

// Logical asset names keep lazy-content and budget assertions independent of
// build hashes. This helper is test-only: HTTP serving never aliases old URLs.
func testBuildAsset(name string) ([]byte, error) {
	if body, err := StaticAsset(name); err == nil {
		return body, nil
	}
	ext := pathpkg.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for _, file := range ChatAssetManifest().Files {
		if strings.HasPrefix(file, stem+"-") && strings.HasSuffix(file, ext) && len(strings.TrimSuffix(strings.TrimPrefix(file, stem+"-"), ext)) == 8 {
			return StaticAsset(file)
		}
	}
	return StaticAsset(name)
}

func TestChatManifestCompleteAndContentAddressed(t *testing.T) {
	manifest := ChatAssetManifest()
	if len(manifest.Files) < 30 {
		t.Fatal("incomplete asset graph")
	}
	for _, canonical := range []string{"dist/app.js", "dist/app.css", "dist/chunks/vendor.js", "dist/chunks/katex.js"} {
		if _, err := StaticAsset(canonical); err == nil {
			t.Errorf("stable build path still embedded: %s", canonical)
		}
	}
	err := fs.WalkDir(staticFiles, "static/dist", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		file := strings.TrimPrefix(name, "static/")
		if file == "dist/hub.js" || file == "dist/hub.css" || file == "dist/asset-manifest.json" {
			return nil
		}
		if !IsHashedAsset(file) {
			t.Errorf("build file absent from hashed manifest: %s", file)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(RenderServiceWorker(RenderOptions{}))
	for _, file := range manifest.Files {
		if !strings.Contains(rendered, `"./`+file+`"`) {
			t.Errorf("SW missing %s", file)
		}
	}
	for _, path := range PrewarmAssetPaths(true) {
		if !IsHashedAsset(path) {
			t.Errorf("prewarm not hashed: %s", path)
		}
	}
}

func TestSourceAssetPathsAndManifestCopies(t *testing.T) {
	manifest := ChatAssetManifest()
	originalEntryCSS := manifest.EntryCSS[0]
	manifest.EntryCSS[0] = "mutated"
	manifest.Files[0] = "mutated"
	copy := ChatAssetManifest()
	if copy.EntryCSS[0] != originalEntryCSS || copy.Files[0] == "mutated" {
		t.Fatal("manifest exposed mutable shared storage")
	}
	sourcePaths := SourceAssetPaths()
	for _, file := range sourcePaths {
		if _, err := StaticAsset(file); err != nil {
			t.Errorf("source path %s: %v", file, err)
		}
	}
	for _, file := range copy.Files {
		if strings.HasSuffix(file, ".js") || strings.HasSuffix(file, ".css") {
			index := sort.SearchStrings(sourcePaths, file)
			if index == len(sourcePaths) || sourcePaths[index] != file {
				t.Errorf("hashed source omitted: %s", file)
			}
		}
	}
}
