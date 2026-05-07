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

//go:embed static/*
var staticFiles embed.FS

var (
	assetVersionOnce sync.Once
	assetVersion     string

	manifestOnce  sync.Once
	manifestBytes []byte

	serviceWorkerOnce  sync.Once
	serviceWorkerBytes []byte
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

// RenderIndexHTML returns the index page with versioned first-party assets and caller-supplied head markup.
func RenderIndexHTML(basePath, headSnippet string) []byte {
	html := IndexHTML()
	if len(html) == 0 {
		return nil
	}

	replacements := []struct{ old, new string }{
		{`href="icon-512.png"`, `href="` + versioned("icon-512.png") + `"`},
		{`href="manifest.webmanifest"`, `href="` + versioned("manifest.webmanifest") + `"`},
		{`href="app.css"`, `href="` + versioned("app.css") + `"`},
		{`src="markdown-setup.js"`, `src="` + versioned("markdown-setup.js") + `"`},
		{`src="markdown-streaming.js"`, `src="` + versioned("markdown-streaming.js") + `"`},
		{`src="decoration.js"`, `src="` + versioned("decoration.js") + `"`},
		{`src="app-core.js"`, `src="` + versioned("app-core.js") + `"`},
		{`src="app-render.js"`, `src="` + versioned("app-render.js") + `"`},
		{`src="app-stream.js"`, `src="` + versioned("app-stream.js") + `"`},
		{`src="app-sessions.js"`, `src="` + versioned("app-sessions.js") + `"`},
		{`src="app-webrtc.js"`, `src="` + versioned("app-webrtc.js") + `"`},
	}
	for _, replacement := range replacements {
		html = bytes.ReplaceAll(html, []byte(replacement.old), []byte(replacement.new))
	}

	baseTag := `<base href="` + htmlpkg.EscapeString(basePath) + `/">`
	html = bytes.Replace(html, []byte(`<meta charset="utf-8">`), []byte(`<meta charset="utf-8">`+"\n  "+baseTag), 1)
	if headSnippet != "" {
		html = bytes.Replace(html, []byte("</head>"), []byte(headSnippet+"</head>"), 1)
	}
	return html
}

// RenderManifest returns the manifest with versioned icon URLs.
func RenderManifest() []byte {
	manifestOnce.Do(func() {
		data, err := StaticAsset("manifest.webmanifest")
		if err != nil {
			return
		}
		manifestBytes = bytes.ReplaceAll(data, []byte(`"./icon-512.png"`), []byte(`"./`+versioned("icon-512.png")+`"`))
	})
	return manifestBytes
}

// RenderServiceWorker returns the service worker with a versioned cache key and shell asset URLs.
func RenderServiceWorker() []byte {
	serviceWorkerOnce.Do(func() {
		data, err := StaticAsset("sw.js")
		if err != nil {
			return
		}
		replacements := []struct{ old, new string }{
			{"term-llm-shell-v2", "term-llm-shell-" + AssetVersion()},
			{"'./manifest.webmanifest'", "'./" + versioned("manifest.webmanifest") + "'"},
			{"'./icon-512.png'", "'./" + versioned("icon-512.png") + "'"},
			{"'./app.css'", "'./" + versioned("app.css") + "'"},
			{"'./markdown-setup.js'", "'./" + versioned("markdown-setup.js") + "'"},
			{"'./markdown-streaming.js'", "'./" + versioned("markdown-streaming.js") + "'"},
			{"'./decoration.js'", "'./" + versioned("decoration.js") + "'"},
			{"'./app-core.js'", "'./" + versioned("app-core.js") + "'"},
			{"'./app-render.js'", "'./" + versioned("app-render.js") + "'"},
			{"'./app-stream.js'", "'./" + versioned("app-stream.js") + "'"},
			{"'./app-sessions.js'", "'./" + versioned("app-sessions.js") + "'"},
			{"'./app-webrtc.js'", "'./" + versioned("app-webrtc.js") + "'"},
		}
		for _, replacement := range replacements {
			data = bytes.ReplaceAll(data, []byte(replacement.old), []byte(replacement.new))
		}
		serviceWorkerBytes = data
	})
	return serviceWorkerBytes
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
