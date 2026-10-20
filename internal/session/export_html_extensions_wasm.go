//go:build js && wasm

package session

import "github.com/yuin/goldmark"

// The browser renders Markdown in the frontend and does not expose native HTML
// transcript export. Do not link Goldmark's server-side GFM extension into the
// browser: its package initializer compiles {1,256} URL patterns whose simplified
// syntax trees are 512 levels deep. Go's recursive regexp compiler exhausts the
// native WebAssembly stack on iPhone before main can run. CommonMark itself has
// no such initializer; native exports retain GFM unchanged.
func htmlExportExtensions() []goldmark.Extender {
	return nil
}
