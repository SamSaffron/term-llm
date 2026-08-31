package serveui

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	pathpkg "path"
	"regexp"
	"strings"
	"testing"
)

func TestGeneratedBundleAssets(t *testing.T) {
	for _, name := range []string{
		"dist/app.js", "dist/app.css", "dist/chunks/vendor.js",
		"dist/chunks/rich-highlight.js", "dist/chunks/rich-katex.js",
		"dist/chunks/highlight.js", "dist/chunks/katex.js", "dist/chunks/katex.css", "dist/chunks/webrtc.js",
		"dist/chunks/MarkdownFilePreview.js", "dist/chunks/markdown-preview-store.js",
		"dist/chunks/markdown-document.js", "dist/chunks/file-text.js",
		"dist/assets/MarkdownFilePreview.css",
	} {
		body, err := StaticAsset(name)
		if err != nil {
			t.Fatalf("StaticAsset(%q): %v", name, err)
		}
		if len(body) == 0 {
			t.Fatalf("StaticAsset(%q) is empty", name)
		}
	}
	if _, err := StaticAsset("dist/app.js.map"); err == nil {
		t.Fatal("production source map must not be embedded")
	}
}

func TestGeneratedCSSPreloadsResolveToEmbeddedAssets(t *testing.T) {
	cssReference := regexp.MustCompile(`["']((?:\.\.?/)[^"']+\.css)["']`)
	err := fs.WalkDir(staticFiles, "static/dist", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".js") {
			return err
		}
		body, err := fs.ReadFile(staticFiles, name)
		if err != nil {
			return err
		}
		assetName := strings.TrimPrefix(name, "static/")
		for _, match := range cssReference.FindAllSubmatch(body, -1) {
			resolved := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(assetName), string(match[1])))
			if _, err := StaticAsset(resolved); err != nil {
				t.Errorf("%s preloads missing CSS asset %q: %v", assetName, resolved, err)
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
		// authoritative mobile stream recovery, widget process lifecycle controls,
		// model capability-aware runtime controls, and the small eager Markdown
		// review toggle/loader are first-party shell code. The preview body, parser,
		// source transport, and preview CSS remain in bounded lazy chunks. Keep
		// bounded headroom while still failing meaningful accidental regressions.
		"dist/app.js":  {raw: 435_500, gzip: 127_000},
		"dist/app.css": {raw: 166_500, gzip: 32_000},
	}
	for name, budget := range budgets {
		body, err := StaticAsset(name)
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
	js, err := StaticAsset("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"window.TermLLMApp", "preact/compat", "legacy-bridge", "./assets/highlight.css", "./assets/katex.css"} {
		if bytes.Contains(js, []byte(forbidden)) {
			t.Errorf("production bundle contains forbidden compatibility path %q", forbidden)
		}
	}
	if bytes.Contains(js, []byte("__TERM_LLM_TEST__")) || bytes.Contains(js, []byte("__TERM_LLM_ENABLE_TEST_BRIDGE__")) {
		t.Fatal("production bundle contains browser test bridge")
	}
}

func TestLazyChunksReuseCanonicalEntryModuleURL(t *testing.T) {
	importsEntry := false
	for _, name := range []string{
		"dist/chunks/markdown-preview-store.js",
		"dist/chunks/file-text.js",
	} {
		body, err := StaticAsset(name)
		if err != nil {
			t.Fatal(err)
		}
		importsEntry = importsEntry || bytes.Contains(body, []byte(`../app.js`))
	}
	if !importsEntry {
		t.Fatal("expected a lazy Markdown chunk to exercise the shared entry import contract")
	}
	rendered := string(RenderIndexHTML("/ui", "", RenderOptions{}))
	if !strings.Contains(rendered, `type="module" src="dist/app.js"`) || strings.Contains(rendered, `src="dist/app.js?v=`) {
		t.Fatal("lazy chunks and the HTML entry must resolve app.js to one canonical module URL")
	}
}

func TestRenderIndexHTMLPreservesBootstrapAndModuleContract(t *testing.T) {
	const bootstrap = `<script>window.TERM_LLM_UI_PREFIX="/chat/nodes/alpha";</script>`
	rendered := string(RenderIndexHTML("/chat/nodes/alpha", bootstrap, RenderOptions{WebRTC: true}))
	version := AssetVersion()
	for _, want := range []string{
		`<base href="/chat/nodes/alpha/">`,
		bootstrap,
		`href="dist/app.css?v=` + version + `"`,
		`type="module" src="dist/app.js"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered index missing %q", want)
		}
	}
	if strings.Contains(rendered, `src="dist/app.js?v=`) {
		t.Fatal("entry module must keep the canonical URL imported by lazy chunks")
	}
	if strings.Index(rendered, bootstrap) > strings.Index(rendered, `src="dist/app.js`) {
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
		"'./dist/app.css?v=" + AssetVersion() + "'",
	} {
		if !strings.Contains(without, want) {
			t.Errorf("service worker missing %q", want)
		}
	}
	for _, chunk := range []string{"app.js?v=", "vendor.js?v=", "webrtc.js?v=", "highlight.js?v=", "katex.js?v="} {
		if strings.Contains(without, chunk) || strings.Contains(with, chunk) {
			t.Errorf("stable-named chunk %q must remain unversioned and network-first", chunk)
		}
	}
	if strings.Contains(without, "'./dist/app.js'") || strings.Contains(with, "'./dist/app.js'") {
		t.Fatal("canonical app entry must remain network-first instead of joining the shell cache")
	}
	if without != with {
		t.Fatal("WebRTC must not add a versioned URL that differs from the chunk URL imported by Vite")
	}
}

func TestStaticAssetReturnsCopy(t *testing.T) {
	first, err := StaticAsset("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := StaticAsset("dist/app.js")
	first[0] ^= 0xff
	if bytes.Equal(first, second) {
		t.Fatal("StaticAsset returned shared mutable storage")
	}
}

func TestRenderedAssetsAreGzipReadable(t *testing.T) {
	body, _ := StaticAsset("dist/app.js")
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
