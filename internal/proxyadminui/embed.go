// Package proxyadminui embeds the management UI served by `term-llm serve proxy`.
package proxyadminui

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed static/index.html static/proxy-admin.css static/proxy-admin.js
var assets embed.FS

// Asset returns an embedded UI asset by its basename.
func Asset(name string) ([]byte, error) {
	if name == "" || name == "." || name == ".." {
		return nil, fmt.Errorf("invalid proxy admin asset %q", name)
	}
	b, err := fs.ReadFile(assets, "static/"+name)
	if err != nil {
		return nil, fmt.Errorf("read proxy admin asset %q: %w", name, err)
	}
	return b, nil
}
