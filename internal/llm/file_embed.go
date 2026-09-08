package llm

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

// EmbeddedFileIntro introduces one or more file bodies embedded directly in a
// prompt. It is exported so UI/export code can strip embedded bodies from display
// while preserving them in session history.
const EmbeddedFileIntro = "The following user-provided file attachments are embedded below:"

const embeddedFileBeginMarker = "--- BEGIN USER-PROVIDED FILE:"

// EmbeddedFileDisplayName returns a single-line, path-free name suitable for
// prompt markers and provider filenames. Browser uploads normally provide a base
// name already, but API clients may send absolute paths or control characters.
func EmbeddedFileDisplayName(filename string) string {
	name := strings.TrimSpace(filename)
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		name = "upload"
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "upload"
	}
	return name
}

// FormatEmbeddedFileText wraps user-provided file contents in explicit markers
// so models can tell where an embedded attachment starts and ends. The contents
// are fenced as markdown, using a fence longer than any backtick run in the file.
func FormatEmbeddedFileText(filename, mediaType, text string) string {
	name := EmbeddedFileDisplayName(filename)
	mediaType = NormalizeMediaType(mediaType)

	header := fmt.Sprintf("%s %s", embeddedFileBeginMarker, name)
	if mediaType != "" {
		header += fmt.Sprintf(" (%s)", mediaType)
	}
	header += " ---"

	fence := markdownFenceForText(text)
	openingFence := fence
	if lang := embeddedFileFenceLanguage(name, mediaType); lang != "" {
		openingFence += lang
	}

	return fmt.Sprintf("%s\n%s\n%s\n%s\n--- END USER-PROVIDED FILE: %s ---\n\n", header, openingFence, text, fence, name)
}

func embeddedFileFenceLanguage(filename, mediaType string) string {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(filename, "\\", "/"))) {
	case ".md", ".markdown":
		return "markdown"
	case ".csv":
		return "csv"
	case ".tsv":
		return "tsv"
	case ".json", ".jsonl":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".xml":
		return "xml"
	case ".html", ".htm":
		return "html"
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".py", ".rb", ".rs", ".java", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".sh", ".bash", ".zsh", ".fish", ".sql", ".css", ".scss", ".toml", ".ini", ".conf":
		return strings.TrimPrefix(strings.ToLower(path.Ext(strings.ReplaceAll(filename, "\\", "/"))), ".")
	}
	switch NormalizeMediaType(mediaType) {
	case "application/json", "application/x-ndjson":
		return "json"
	case "text/csv":
		return "csv"
	case "text/tab-separated-values":
		return "tsv"
	case "application/yaml", "application/x-yaml", "text/yaml":
		return "yaml"
	case "text/markdown":
		return "markdown"
	case "application/xml", "text/xml":
		return "xml"
	case "text/html":
		return "html"
	}
	return "text"
}

func markdownFenceForText(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence
}

// StripEmbeddedFileText removes embedded file bodies from a display/export copy
// of a user message. It intentionally does not mutate stored message parts.
func StripEmbeddedFileText(content string) string {
	for _, marker := range []string{
		"\n\n" + EmbeddedFileIntro,
		"\n" + embeddedFileBeginMarker,
		embeddedFileBeginMarker,
		"\n\n---\n**Attached files:**", // legacy TUI marker
	} {
		if idx := strings.Index(content, marker); idx >= 0 {
			return strings.TrimSpace(content[:idx])
		}
	}
	return content
}

// ExtractEmbeddedFileNames returns display names from embedded file markers.
func ExtractEmbeddedFileNames(content string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, embeddedFileBeginMarker) {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, embeddedFileBeginMarker))
		name = strings.TrimSuffix(name, "---")
		name = strings.TrimSpace(name)
		if idx := strings.LastIndex(name, " ("); idx >= 0 && strings.HasSuffix(name, ")") {
			maybeMediaType := strings.TrimSuffix(name[idx+2:], ")")
			if strings.Contains(maybeMediaType, "/") {
				name = strings.TrimSpace(name[:idx])
			}
		}
		name = EmbeddedFileDisplayName(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// FormatUploadedFileNotice describes an upload without embedding its contents.
// The local path lets tools inspect files the model cannot consume natively.
func FormatUploadedFileNotice(filename, mediaType, localPath string, sizeBytes int64) string {
	mediaType = NormalizeMediaType(mediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	notice := fmt.Sprintf("[User uploaded file: %q (MIME type: %q, size: %d bytes). Contents are not included in model context.", EmbeddedFileDisplayName(filename), mediaType, sizeBytes)
	if localPath != "" {
		notice += fmt.Sprintf(" Saved local path: %q. Use available file or shell tools to inspect it.", localPath)
	}
	return notice + "]\n\n"
}
