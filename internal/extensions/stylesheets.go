package extensions

import (
	"html"
	"net/url"
	"strings"
)

// StylesheetLinks renders a <link> for each enabled, valid extension with a
// stylesheet, in enabled order to match the cascade the client loader builds.
// The hrefs are relative ("extensions/<generation>/<id>/<css>") so they resolve
// against the page's <base>, which a Hub proxy rebases to the node mount.
func (m *Manager) StylesheetLinks() string {
	s := m.Status()
	if s.Disabled || s.Generation == "" {
		return ""
	}
	var links strings.Builder
	for _, id := range s.Enabled {
		for _, entry := range s.Entries {
			if entry.ID != id || entry.Error != "" || entry.CSS == "" {
				continue
			}
			href := "extensions/" + url.PathEscape(s.Generation) + "/" + url.PathEscape(id) + "/" + escapePath(entry.CSS)
			links.WriteString(`<link rel="stylesheet" data-extension="` + html.EscapeString(id) + `" href="` + html.EscapeString(href) + `">`)
		}
	}
	return links.String()
}

func escapePath(p string) string {
	segments := strings.Split(p, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
