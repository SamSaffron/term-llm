package extensions

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// MaxIconSpriteBytes bounds one extension's icon sprite.
const MaxIconSpriteBytes = 1 << 20

const svgNamespace = "http://www.w3.org/2000/svg"

var validIconSymbol = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// iconSymbols lists the <symbol id> values of an icon sprite: an SVG document
// whose symbols are named after the web UI's icons. The browser draws them with
// <use href="sprite.svg#id">; ids the UI doesn't know are ignored there.
func iconSymbols(sprite []byte) ([]string, error) {
	if len(sprite) > MaxIconSpriteBytes {
		return nil, fmt.Errorf("icon sprite exceeds %d bytes", MaxIconSpriteBytes)
	}
	decoder := xml.NewDecoder(bytes.NewReader(sprite))
	var symbols []string
	seen := map[string]bool{}
	depth := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("icon sprite is not well-formed XML: %w", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			depth++
			// Without the SVG namespace a browser doesn't treat the symbols as SVG,
			// and every replaced icon would draw blank instead of falling back.
			if depth == 1 && (element.Name.Local != "svg" || element.Name.Space != svgNamespace) {
				return nil, fmt.Errorf(`icon sprite must be an <svg xmlns="%s"> document`, svgNamespace)
			}
			if element.Name.Local != "symbol" {
				continue
			}
			id := ""
			for _, attr := range element.Attr {
				if attr.Name.Local == "id" && attr.Name.Space == "" {
					id = attr.Value
				}
			}
			if !validIconSymbol.MatchString(id) {
				return nil, fmt.Errorf("icon symbol id %q must be lowercase letters, numbers and hyphens", id)
			}
			if seen[id] {
				return nil, fmt.Errorf("icon symbol %q is defined twice", id)
			}
			seen[id] = true
			symbols = append(symbols, id)
		case xml.EndElement:
			depth--
		}
	}
	if len(symbols) == 0 {
		return nil, fmt.Errorf("icon sprite has no <symbol id> elements")
	}
	return symbols, nil
}

// IconMeta renders a <meta> per enabled, valid extension with an icon sprite, in
// enabled order (a later extension's symbol wins). The page reads them before
// its first render (frontend/src/components/icon-overrides.ts). Like stylesheet
// hrefs, content is relative so it resolves against the page's <base>.
// Extension assets are not cacheable, so this is deliberately not a preload:
// <use> fetches the sprite with its own destination and would download it twice.
func (m *Manager) IconMeta() string {
	s := m.Status()
	if s.Disabled || s.Generation == "" {
		return ""
	}
	var tags strings.Builder
	for _, id := range s.Enabled {
		for _, entry := range s.Entries {
			if entry.ID != id || entry.Error != "" || entry.Icons == "" || len(entry.IconSymbols) == 0 {
				continue
			}
			href := "extensions/" + url.PathEscape(s.Generation) + "/" + url.PathEscape(id) + "/" + escapePath(entry.Icons)
			tags.WriteString(`<meta name="term-llm-extension-icons" data-extension="` + html.EscapeString(id) +
				`" data-symbols="` + html.EscapeString(strings.Join(entry.IconSymbols, " ")) +
				`" content="` + html.EscapeString(href) + `">`)
		}
	}
	return tags.String()
}
