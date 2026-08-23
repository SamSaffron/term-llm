package serveui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	htmlpkg "html"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

//go:embed static/index.html static/manifest.webmanifest static/icon-512.png static/sw.js
//go:embed static/app.css
//go:embed static/app-core.js static/toast.js static/app-network.js static/app-plan.js static/slash-commands.js static/app-render.js static/app-sessions.js static/app-path-notes.js static/app-branching.js static/app-branch-commands.js static/app-session-events.js static/app-mcp.js static/app-goals-location.js static/app-message-convert.js static/intent-storage.js static/app-session-admin.js static/app-project-picker.js static/app-sidebar.js
//go:embed static/app-attachments.js static/app-stream.js static/app-response-effects.js static/app-send.js static/app-runtime.js static/app-interject.js static/app-modals.js static/app-composer.js static/app-skills.js static/side-question.js static/app-webrtc.js static/app-diff-comments.js static/app-diff-queue.js static/app-diff-scopes.js static/app-diffs.js static/app-worktrees.js
//go:embed static/decoration.js static/guardian-render.js static/markdown-setup.js static/markdown-streaming.js static/transcript-window.js static/active-response.js static/conversation.js
//go:embed static/vendor
var staticFiles embed.FS

var (
	assetVersionOnce sync.Once
	assetVersion     string

	renderManifestOnce sync.Once
	renderManifest     []byte

	renderServiceWorkerOnce [2]sync.Once
	renderServiceWorker     [2][]byte
)

// AssetVersion returns a stable hash of the embedded UI assets.
func AssetVersion() string {
	assetVersionOnce.Do(func() {
		entries := make([]string, 0, 32)
		_ = fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			entries = append(entries, path)
			return nil
		})
		sort.Strings(entries)
		h := sha256.New()
		for _, path := range entries {
			data, err := fs.ReadFile(staticFiles, path)
			if err != nil {
				continue
			}
			_, _ = h.Write([]byte(path))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write(data)
			_, _ = h.Write([]byte{0})
		}
		assetVersion = hex.EncodeToString(h.Sum(nil))[:12]
	})
	return assetVersion
}

func versioned(path string) string {
	return path + "?v=" + AssetVersion()
}

// IndexHTML returns the embedded UI page.
func IndexHTML() []byte {
	data, err := StaticAsset("index.html")
	if err != nil {
		return nil
	}
	return data
}

// RenderOptions controls optional UI features included in rendered UI assets.
type RenderOptions struct {
	WebRTC bool
}

// RenderIndexHTML returns the index page with versioned first-party assets,
// caller-supplied head markup, and optional feature scripts.
func RenderIndexHTML(basePath, headSnippet string, opts RenderOptions) []byte {
	html := IndexHTML()
	if len(html) == 0 {
		return nil
	}

	replacements := []struct{ old, new string }{
		{`href="icon-512.png"`, `href="` + versioned("icon-512.png") + `"`},
		{`href="manifest.webmanifest"`, `href="` + versioned("manifest.webmanifest") + `"`},
		{`href="app.css"`, `href="` + versioned("app.css") + `"`},
		{`href="app-core.js"`, `href="` + versioned("app-core.js") + `"`},
		{`href="toast.js"`, `href="` + versioned("toast.js") + `"`},
		{`href="app-network.js"`, `href="` + versioned("app-network.js") + `"`},
		{`href="transcript-window.js"`, `href="` + versioned("transcript-window.js") + `"`},
		{`href="active-response.js"`, `href="` + versioned("active-response.js") + `"`},
		{`href="conversation.js"`, `href="` + versioned("conversation.js") + `"`},
		{`href="app-plan.js"`, `href="` + versioned("app-plan.js") + `"`},
		{`href="slash-commands.js"`, `href="` + versioned("slash-commands.js") + `"`},
		{`href="guardian-render.js"`, `href="` + versioned("guardian-render.js") + `"`},
		{`href="app-render.js"`, `href="` + versioned("app-render.js") + `"`},
		{`href="app-attachments.js"`, `href="` + versioned("app-attachments.js") + `"`},
		{`href="app-stream.js"`, `href="` + versioned("app-stream.js") + `"`},
		{`href="app-response-effects.js"`, `href="` + versioned("app-response-effects.js") + `"`},
		{`href="app-send.js"`, `href="` + versioned("app-send.js") + `"`},
		{`href="app-runtime.js"`, `href="` + versioned("app-runtime.js") + `"`},
		{`href="app-interject.js"`, `href="` + versioned("app-interject.js") + `"`},
		{`href="app-modals.js"`, `href="` + versioned("app-modals.js") + `"`},
		{`href="app-composer.js"`, `href="` + versioned("app-composer.js") + `"`},
		{`href="app-skills.js"`, `href="` + versioned("app-skills.js") + `"`},
		{`href="side-question.js"`, `href="` + versioned("side-question.js") + `"`},
		{`href="app-project-picker.js"`, `href="` + versioned("app-project-picker.js") + `"`},
		{`href="app-sidebar.js"`, `href="` + versioned("app-sidebar.js") + `"`},
		{`href="app-sessions.js"`, `href="` + versioned("app-sessions.js") + `"`},
		{`href="app-path-notes.js"`, `href="` + versioned("app-path-notes.js") + `"`},
		{`href="app-branching.js"`, `href="` + versioned("app-branching.js") + `"`},
		{`href="app-branch-commands.js"`, `href="` + versioned("app-branch-commands.js") + `"`},
		{`href="app-session-events.js"`, `href="` + versioned("app-session-events.js") + `"`},
		{`href="app-mcp.js"`, `href="` + versioned("app-mcp.js") + `"`},
		{`href="app-goals-location.js"`, `href="` + versioned("app-goals-location.js") + `"`},
		{`href="app-message-convert.js"`, `href="` + versioned("app-message-convert.js") + `"`},
		{`href="intent-storage.js"`, `href="` + versioned("intent-storage.js") + `"`},
		{`href="app-session-admin.js"`, `href="` + versioned("app-session-admin.js") + `"`},
		{`href="app-diff-comments.js"`, `href="` + versioned("app-diff-comments.js") + `"`},
		{`href="app-diff-queue.js"`, `href="` + versioned("app-diff-queue.js") + `"`},
		{`href="app-diff-scopes.js"`, `href="` + versioned("app-diff-scopes.js") + `"`},
		{`href="app-diffs.js"`, `href="` + versioned("app-diffs.js") + `"`},
		{`href="app-worktrees.js"`, `href="` + versioned("app-worktrees.js") + `"`},
		{`src="markdown-setup.js"`, `src="` + versioned("markdown-setup.js") + `"`},
		{`src="markdown-streaming.js"`, `src="` + versioned("markdown-streaming.js") + `"`},
		{`src="decoration.js"`, `src="` + versioned("decoration.js") + `"`},
		{`src="transcript-window.js"`, `src="` + versioned("transcript-window.js") + `"`},
		{`src="active-response.js"`, `src="` + versioned("active-response.js") + `"`},
		{`src="conversation.js"`, `src="` + versioned("conversation.js") + `"`},
		{`src="app-core.js"`, `src="` + versioned("app-core.js") + `"`},
		{`src="toast.js"`, `src="` + versioned("toast.js") + `"`},
		{`src="app-network.js"`, `src="` + versioned("app-network.js") + `"`},
		{`src="app-plan.js"`, `src="` + versioned("app-plan.js") + `"`},
		{`src="slash-commands.js"`, `src="` + versioned("slash-commands.js") + `"`},
		{`src="guardian-render.js"`, `src="` + versioned("guardian-render.js") + `"`},
		{`src="app-render.js"`, `src="` + versioned("app-render.js") + `"`},
		{`src="app-attachments.js"`, `src="` + versioned("app-attachments.js") + `"`},
		{`src="app-stream.js"`, `src="` + versioned("app-stream.js") + `"`},
		{`src="app-response-effects.js"`, `src="` + versioned("app-response-effects.js") + `"`},
		{`src="app-send.js"`, `src="` + versioned("app-send.js") + `"`},
		{`src="app-runtime.js"`, `src="` + versioned("app-runtime.js") + `"`},
		{`src="app-interject.js"`, `src="` + versioned("app-interject.js") + `"`},
		{`src="app-modals.js"`, `src="` + versioned("app-modals.js") + `"`},
		{`src="app-composer.js"`, `src="` + versioned("app-composer.js") + `"`},
		{`src="app-skills.js"`, `src="` + versioned("app-skills.js") + `"`},
		{`src="side-question.js"`, `src="` + versioned("side-question.js") + `"`},
		{`src="app-project-picker.js"`, `src="` + versioned("app-project-picker.js") + `"`},
		{`src="app-sidebar.js"`, `src="` + versioned("app-sidebar.js") + `"`},
		{`src="app-sessions.js"`, `src="` + versioned("app-sessions.js") + `"`},
		{`src="app-path-notes.js"`, `src="` + versioned("app-path-notes.js") + `"`},
		{`src="app-branching.js"`, `src="` + versioned("app-branching.js") + `"`},
		{`src="app-branch-commands.js"`, `src="` + versioned("app-branch-commands.js") + `"`},
		{`src="app-session-events.js"`, `src="` + versioned("app-session-events.js") + `"`},
		{`src="app-mcp.js"`, `src="` + versioned("app-mcp.js") + `"`},
		{`src="app-goals-location.js"`, `src="` + versioned("app-goals-location.js") + `"`},
		{`src="app-message-convert.js"`, `src="` + versioned("app-message-convert.js") + `"`},
		{`src="intent-storage.js"`, `src="` + versioned("intent-storage.js") + `"`},
		{`src="app-session-admin.js"`, `src="` + versioned("app-session-admin.js") + `"`},
		{`src="app-diff-comments.js"`, `src="` + versioned("app-diff-comments.js") + `"`},
		{`src="app-diff-queue.js"`, `src="` + versioned("app-diff-queue.js") + `"`},
		{`src="app-diff-scopes.js"`, `src="` + versioned("app-diff-scopes.js") + `"`},
		{`src="app-diffs.js"`, `src="` + versioned("app-diffs.js") + `"`},
		{`src="app-worktrees.js"`, `src="` + versioned("app-worktrees.js") + `"`},
	}
	for _, replacement := range replacements {
		html = bytes.ReplaceAll(html, []byte(replacement.old), []byte(replacement.new))
	}

	webrtcScript := ""
	if opts.WebRTC {
		webrtcScript = `<script src="` + versioned("app-webrtc.js") + `"></script>`
	}
	html = bytes.Replace(html, []byte(`<!-- term-llm:webrtc-script -->`), []byte(webrtcScript), 1)

	baseTag := `<base href="` + htmlpkg.EscapeString(basePath) + `/">`
	html = bytes.Replace(html, []byte(`<meta charset="utf-8">`), []byte(`<meta charset="utf-8">`+"\n  "+baseTag), 1)
	if headSnippet != "" {
		html = bytes.Replace(html, []byte("</head>"), []byte(headSnippet+"</head>"), 1)
	}
	return html
}

// RenderManifest returns the manifest with versioned icon URLs. The returned
// slice is cached and must be treated as read-only.
func RenderManifest() []byte {
	renderManifestOnce.Do(func() {
		data, err := StaticAsset("manifest.webmanifest")
		if err != nil {
			return
		}
		renderManifest = bytes.ReplaceAll(data, []byte(`"./icon-512.png"`), []byte(`"./`+versioned("icon-512.png")+`"`))
	})
	return renderManifest
}

// RenderServiceWorker returns the service worker with a versioned cache key,
// shell asset URLs, and optional feature assets. The returned slice is cached
// per option set and must be treated as read-only.
func RenderServiceWorker(opts RenderOptions) []byte {
	cacheIndex := 0
	if opts.WebRTC {
		cacheIndex = 1
	}
	renderServiceWorkerOnce[cacheIndex].Do(func() {
		renderServiceWorker[cacheIndex] = renderServiceWorkerBytes(opts)
	})
	return renderServiceWorker[cacheIndex]
}

func renderServiceWorkerBytes(opts RenderOptions) []byte {
	data, err := StaticAsset("sw.js")
	if err != nil {
		return nil
	}
	replacements := []struct{ old, new string }{
		{"term-llm-shell-v5", "term-llm-shell-" + AssetVersion()},
		{"'./manifest.webmanifest'", "'./" + versioned("manifest.webmanifest") + "'"},
		{"'./icon-512.png'", "'./" + versioned("icon-512.png") + "'"},
		{"'./app.css'", "'./" + versioned("app.css") + "'"},
		{"'./markdown-setup.js'", "'./" + versioned("markdown-setup.js") + "'"},
		{"'./markdown-streaming.js'", "'./" + versioned("markdown-streaming.js") + "'"},
		{"'./decoration.js'", "'./" + versioned("decoration.js") + "'"},
		{"'./transcript-window.js'", "'./" + versioned("transcript-window.js") + "'"},
		{"'./active-response.js'", "'./" + versioned("active-response.js") + "'"},
		{"'./conversation.js'", "'./" + versioned("conversation.js") + "'"},
		{"'./app-core.js'", "'./" + versioned("app-core.js") + "'"},
		{"'./toast.js'", "'./" + versioned("toast.js") + "'"},
		{"'./app-network.js'", "'./" + versioned("app-network.js") + "'"},
		{"'./app-plan.js'", "'./" + versioned("app-plan.js") + "'"},
		{"'./slash-commands.js'", "'./" + versioned("slash-commands.js") + "'"},
		{"'./guardian-render.js'", "'./" + versioned("guardian-render.js") + "'"},
		{"'./app-render.js'", "'./" + versioned("app-render.js") + "'"},
		{"'./app-attachments.js'", "'./" + versioned("app-attachments.js") + "'"},
		{"'./app-stream.js'", "'./" + versioned("app-stream.js") + "'"},
		{"'./app-response-effects.js'", "'./" + versioned("app-response-effects.js") + "'"},
		{"'./app-send.js'", "'./" + versioned("app-send.js") + "'"},
		{"'./app-runtime.js'", "'./" + versioned("app-runtime.js") + "'"},
		{"'./app-interject.js'", "'./" + versioned("app-interject.js") + "'"},
		{"'./app-modals.js'", "'./" + versioned("app-modals.js") + "'"},
		{"'./app-composer.js'", "'./" + versioned("app-composer.js") + "'"},
		{"'./app-skills.js'", "'./" + versioned("app-skills.js") + "'"},
		{"'./side-question.js'", "'./" + versioned("side-question.js") + "'"},
		{"'./app-sidebar.js'", "'./" + versioned("app-sidebar.js") + "'"},
		{"'./app-sessions.js'", "'./" + versioned("app-sessions.js") + "'"},
		{"'./app-branch-commands.js'", "'./" + versioned("app-branch-commands.js") + "'"},
		{"'./app-session-events.js'", "'./" + versioned("app-session-events.js") + "'"},
		{"'./app-mcp.js'", "'./" + versioned("app-mcp.js") + "'"},
		{"'./app-goals-location.js'", "'./" + versioned("app-goals-location.js") + "'"},
		{"'./app-message-convert.js'", "'./" + versioned("app-message-convert.js") + "'"},
		{"'./intent-storage.js'", "'./" + versioned("intent-storage.js") + "'"},
		{"'./app-session-admin.js'", "'./" + versioned("app-session-admin.js") + "'"},
		{"'./app-diff-comments.js'", "'./" + versioned("app-diff-comments.js") + "'"},
		{"'./app-diff-queue.js'", "'./" + versioned("app-diff-queue.js") + "'"},
		{"'./app-diff-scopes.js'", "'./" + versioned("app-diff-scopes.js") + "'"},
		{"'./app-project-picker.js'", "'./" + versioned("app-project-picker.js") + "'"},
		{"'./app-diffs.js'", "'./" + versioned("app-diffs.js") + "'"},
		{"'./app-worktrees.js'", "'./" + versioned("app-worktrees.js") + "'"},
	}
	for _, replacement := range replacements {
		data = bytes.ReplaceAll(data, []byte(replacement.old), []byte(replacement.new))
	}

	webrtcAsset := ""
	if opts.WebRTC {
		webrtcAsset = "'./" + versioned("app-webrtc.js") + "',"
	}
	data = bytes.Replace(data, []byte(`// term-llm:webrtc-shell-asset`), []byte(webrtcAsset), 1)
	return data
}

// StaticAsset returns a copy of an embedded serve-ui asset.
func StaticAsset(name string) ([]byte, error) {
	cleanName := strings.TrimSpace(strings.TrimPrefix(name, "/"))
	if cleanName == "" || strings.Contains(cleanName, "..") {
		return nil, fmt.Errorf("invalid asset name %q", name)
	}
	data, err := fs.ReadFile(staticFiles, "static/"+cleanName)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}
