//go:build js && wasm

package session

import "github.com/yuin/goldmark"

// The browser host does not export native HTML transcripts. Do not link
// Goldmark's GFM extension there: its package initializer compiles {1,256} URL
// patterns whose simplified syntax trees are 512 levels deep, and Go's
// recursive regexp compiler exhausts the native WebAssembly stack on iPhone
// before main can run. CommonMark has no such initializer; native builds keep
// GFM unchanged.
func htmlExportExtensions() []goldmark.Extender {
	return nil
}
