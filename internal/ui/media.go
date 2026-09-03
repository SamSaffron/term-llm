package ui

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

var mediaMarkdownPattern = regexp.MustCompile(`(?im)!\[([^\]\n]*)\]\([ \t]*<?term-llm-media://([a-f0-9]{32})>?[ \t]*\)[ \t]*$`)

func mediaReferenceInFence(content string, offset int) bool {
	fence := ""
	for _, line := range strings.Split(content[:offset], "\n") {
		trimmed := strings.TrimSpace(line)
		marker := ""
		switch {
		case strings.HasPrefix(trimmed, "```"):
			marker = "```"
		case strings.HasPrefix(trimmed, "~~~"):
			marker = "~~~"
		}
		if marker == "" {
			continue
		}
		if fence == "" {
			fence = marker
		} else if fence == marker {
			fence = ""
		}
	}
	return fence != ""
}

// RenderMediaMarkdown renders registered term-llm media references at their
// position in assistant Markdown. Unknown references become an explicit
// unavailable placeholder rather than leaking the opaque URI.
func RenderMediaMarkdown(content string, media map[string]llm.MediaArtifact, renderMarkdown func(string) string, imageRenderer ImageArtifactRenderer) string {
	if content == "" || !strings.Contains(strings.ToLower(content), "term-llm-media://") {
		return renderMarkdown(content)
	}
	matches := mediaMarkdownPattern.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return renderMarkdown(content)
	}
	var out strings.Builder
	cursor := 0
	for _, match := range matches {
		if mediaReferenceInFence(content, match[0]) {
			continue
		}
		reference := strings.ToLower(content[match[4]:match[5]])
		item, ok := media[reference]
		if before := content[cursor:match[0]]; before != "" {
			if cursor > 0 {
				ensureMediaStartsFreshLine(&out)
			}
			out.WriteString(renderMarkdown(before))
		}
		ensureMediaStartsFreshLine(&out)
		if !ok {
			label := terminaltext.SanitizeSingleLine(strings.TrimSpace(content[match[2]:match[3]]))
			if label == "" {
				label = "media"
			}
			out.WriteString("[" + label + " unavailable]")
			cursor = match[1]
			continue
		}
		if alt := strings.TrimSpace(content[match[2]:match[3]]); alt != "" {
			item.Name = alt
		}
		rendered := ""
		if strings.HasPrefix(strings.ToLower(item.MediaType), "image/") {
			if image := renderMediaImageWithRenderer(item.Path(), imageRenderer); image != "" {
				// Rich terminal image protocols are not universally available (and can
				// be hidden by multiplexers), so always retain an explicit file link.
				rendered = image
				linkItem := item
				linkItem.Caption = ""
				if link := RenderMediaLink(linkItem); link != "" {
					rendered += "\n" + link
				}
			}
		}
		if rendered == "" {
			rendered = RenderMediaLink(item)
		} else if item.Caption != "" {
			rendered += "\n" + terminaltext.Sanitize(item.Caption)
		}
		if rendered == "" {
			label := terminaltext.SanitizeSingleLine(item.Name)
			if label == "" {
				label = "media"
			}
			rendered = "[" + label + " unavailable]"
		}
		out.WriteString(rendered)
		cursor = match[1]
	}
	if cursor == 0 {
		return renderMarkdown(content)
	}
	if cursor < len(content) {
		if tail := renderMarkdown(content[cursor:]); tail != "" {
			ensureMediaStartsFreshLine(&out)
			out.WriteString(tail)
		}
	}
	return out.String()
}

func ensureMediaStartsFreshLine(out *strings.Builder) {
	if out.Len() == 0 {
		return
	}
	visible := ansi.Strip(out.String())
	if visible != "" && !strings.HasSuffix(visible, "\n") && !strings.HasSuffix(visible, "\r") {
		out.WriteByte('\n')
	}
}

func renderMediaImageWithRenderer(path string, renderer ImageArtifactRenderer) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if renderer == nil {
		renderer = DefaultImageArtifactRenderer
	}
	// Positioned show_media references provide their own open/file URI row, so
	// retain only the protocol display here and avoid the legacy path caption.
	return renderer(path).Display
}

// ReplaceMediaMarkdown replaces registered references without rendering the
// surrounding Markdown, for plain streaming output.
func ReplaceMediaMarkdown(content string, media map[string]llm.MediaArtifact) string {
	return RenderMediaMarkdown(content, media, func(value string) string { return value }, func(string) ImageArtifact { return ImageArtifact{} })
}

type MediaMarkdownStream struct {
	pending strings.Builder
}

// Write emits text that cannot be part of an unfinished Markdown media token,
// retaining only the small ambiguous suffix across provider deltas.
func (s *MediaMarkdownStream) Write(chunk string, media map[string]llm.MediaArtifact) string {
	if chunk == "" {
		return ""
	}
	s.pending.WriteString(chunk)
	value := s.pending.String()
	hold := len(value)
	if start := strings.LastIndex(value, "!["); start >= 0 && !strings.Contains(value[start:], ")") {
		hold = start
	} else if strings.HasSuffix(value, "!") {
		hold = len(value) - 1
	}
	if hold == 0 {
		return ""
	}
	ready := value[:hold]
	tail := value[hold:]
	s.pending.Reset()
	s.pending.WriteString(tail)
	return ReplaceMediaMarkdown(ready, media)
}

func (s *MediaMarkdownStream) Finish(media map[string]llm.MediaArtifact) string {
	value := s.pending.String()
	s.pending.Reset()
	return ReplaceMediaMarkdown(value, media)
}

// LocalFileURI returns a correctly escaped file URI for a local path.
func LocalFileURI(path string) string {
	return localFileURIForOS(path, runtime.GOOS)
}

func localFileURIForOS(path, goos string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if goos == "windows" {
		slash := strings.ReplaceAll(filepath.Clean(path), `\`, "/")
		if strings.HasPrefix(slash, "//") {
			parts := strings.SplitN(strings.TrimPrefix(slash, "//"), "/", 2)
			host := parts[0]
			uriPath := "/"
			if len(parts) == 2 {
				uriPath += parts[1]
			}
			return (&url.URL{Scheme: "file", Host: host, Path: uriPath}).String()
		}
		if len(slash) >= 2 && slash[1] == ':' && !strings.HasPrefix(slash, "/") {
			slash = "/" + slash
		}
		return (&url.URL{Scheme: "file", Path: slash}).String()
	}
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	return (&url.URL{Scheme: "file", Path: filepath.Clean(path)}).String()
}

func terminalFileHyperlink(label, path string) string {
	uri := LocalFileURI(path)
	if uri == "" {
		return label
	}
	return ansi.SetHyperlink(uri, "") + label + ansi.ResetHyperlink()
}

// RenderMediaLink renders a clickable terminal link with a visible file URI
// fallback. It is used for videos and when rich image rendering is unavailable.
func RenderMediaLink(item llm.MediaArtifact) string {
	path := strings.TrimSpace(item.Path())
	if path == "" {
		return ""
	}
	name := terminaltext.SanitizeSingleLine(strings.TrimSpace(item.Name))
	if name == "" {
		name = terminaltext.SanitizeSingleLine(filepath.Base(path))
	}
	kind := "Image"
	if strings.HasPrefix(strings.ToLower(item.MediaType), "video/") {
		kind = "Video"
	}
	label := fmt.Sprintf("Open %s", name)
	location := terminaltext.SanitizeSingleLine(path)
	if uri := LocalFileURI(path); uri != "" {
		label = terminalFileHyperlink(label, path)
		// Keep the URI itself visible so terminals without OSC-8 support still
		// provide a copyable value that browsers understand.
		location = terminalFileHyperlink(uri, path)
	}
	result := fmt.Sprintf("%s: %s\n%s", kind, label, location)
	if caption := strings.TrimSpace(terminaltext.Sanitize(item.Caption)); caption != "" {
		result += "\n" + caption
	}
	return result
}
