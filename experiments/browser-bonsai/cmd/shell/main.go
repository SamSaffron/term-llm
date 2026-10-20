// Command shell renders the genuine term-llm web shell for the browser demo and
// copies the embedded frontend assets into the static output directory.
//
// The generated page loads the parent-owned boot.mjs, which installs ephemeral
// storage, the worker-backed HTTP transport, and then imports the real frontend
// entry named by window.TERM_LLM_DEMO_ENTRY.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/serveui"
)

func main() {
	out := flag.String("out", "experiments/browser-bonsai/dist", "static output directory")
	static := flag.String("static", "internal/serveui/static", "source of the embedded frontend assets")
	flag.Parse()

	if err := run(*out, *static); err != nil {
		fmt.Fprintln(os.Stderr, "shell:", err)
		os.Exit(1)
	}
}

func run(out, static string) error {
	manifest := serveui.ChatAssetManifest()
	entry := strings.TrimPrefix(manifest.EntryJS, "dist/")
	if entry == "" {
		return fmt.Errorf("frontend manifest has no entry")
	}
	if err := os.MkdirAll(filepath.Join(out, "assets"), 0o755); err != nil {
		return err
	}

	// Copy the embedded chat assets wholesale so relative chunk, font, and CSS
	// URLs keep working after moving the dist tree under /assets.
	if err := copyAssets(out, static); err != nil {
		return err
	}

	head := "<script>window.TERM_LLM_DEMO_ENTRY = \"assets/" + entry + "\";</script>\n  "
	html := string(serveui.RenderIndexHTML(".", head))
	html = strings.ReplaceAll(html, `href="dist/`, `href="assets/`)
	html = strings.ReplaceAll(html, `src="dist/`, `src="assets/`)
	// The real app entry is loaded by boot.mjs, never directly by the shell.
	html = strings.ReplaceAll(html,
		`<script type="module" src="assets/`+entry+`"></script>`,
		`<script type="module" src="boot.mjs"></script>`)
	if !strings.Contains(html, `src="boot.mjs"`) {
		return fmt.Errorf("rendered shell is missing the boot module")
	}
	if !strings.Contains(html, "TERM_LLM_DEMO_ENTRY") {
		return fmt.Errorf("rendered shell is missing the frontend entry hint")
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), []byte(html), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "manifest.webmanifest"), serveui.RenderManifest(), 0o644); err != nil {
		return err
	}
	icon, err := serveui.StaticAsset("icon-512.png")
	if err != nil {
		return fmt.Errorf("read icon: %w", err)
	}
	if err := os.WriteFile(filepath.Join(out, "icon-512.png"), icon, 0o644); err != nil {
		return err
	}
	return nil
}

func copyAssets(out, static string) error {
	distRoot := filepath.Join(static, "dist")
	logical := map[string]bool{}
	err := filepath.WalkDir(distRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(static, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filepath.Ext(rel) == ".gz" {
			rel = strings.TrimSuffix(rel, ".gz")
		}
		if logical[rel] {
			return nil
		}
		logical[rel] = true
		data, err := serveui.StaticAsset(rel)
		if err != nil {
			return fmt.Errorf("read asset %s: %w", rel, err)
		}
		target := filepath.Join(out, "assets", filepath.FromSlash(strings.TrimPrefix(rel, "dist/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("copy frontend assets: %w", err)
	}
	if len(logical) == 0 {
		return fmt.Errorf("no frontend assets found under %s (run `make frontend`)", distRoot)
	}
	return nil
}
