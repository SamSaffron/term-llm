//go:build !(js && wasm)

package session

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

func htmlExportExtensions() []goldmark.Extender {
	return []goldmark.Extender{extension.GFM}
}
