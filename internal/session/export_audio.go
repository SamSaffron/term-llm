package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/mediatype"
)

const maxHTMLExportInlineAudioBytes = 2 << 20

// htmlExportAudio is presentation data, never a model-supplied HTML fragment.
type htmlExportAudio struct {
	ID       string
	Repeated bool
	URL      string // trusted: validated, exporter-generated data URL
	Name     string
	Label    string
	Caption  string
	Reason   string
	Omitted  bool
}

// ExportResult reports audio actually embedded or omitted from rendered references.
// Counts deduplicate repeated recordings; they do not imply sharing consent.
type ExportResult struct {
	AudioEmbedded int
	AudioOmitted  int
}

type exportAudioResult struct {
	URL    string
	Reason string
}

// exportAudio has its own budget and cache; audio never enters image decoding.
// Cached content is copied as-is, including any embedded metadata.
type exportAudio struct {
	include bool
	used    int
	paths   map[string]exportAudioResult
	content map[[32]byte]string
	players map[string]string
	omitted map[string]bool
}

func newExportAudio(opts ExportOptions) *exportAudio {
	return &exportAudio{include: opts.IncludeAudio, paths: make(map[string]exportAudioResult), content: make(map[[32]byte]string), players: make(map[string]string), omitted: make(map[string]bool)}
}

func (e *exportAudio) resolve(item core.MediaArtifact) exportAudioResult {
	if !e.include {
		return exportAudioResult{Reason: "not included in export"}
	}
	format, ok := mediatype.Lookup(normalizedMediaType(item.MediaType))
	if !ok || format.Kind != mediatype.Audio {
		return exportAudioResult{Reason: "invalid audio format"}
	}
	path := item.Path()
	if path == "" {
		return exportAudioResult{Reason: "file unavailable"}
	}
	key := filepath.Clean(path) + "\x00" + format.MIME
	if result, ok := e.paths[key]; ok {
		return result
	}
	result := e.resolveFile(path, format)
	e.paths[key] = result
	return result
}

func (e *exportAudio) resolveFile(path string, declared mediatype.Format) exportAudioResult {
	// Stat before opening: oversized files (and nonregular files such as FIFOs)
	// must not be read even for format inspection.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return exportAudioResult{Reason: "file unavailable"}
	}
	if info.Size() > maxHTMLExportInlineAudioBytes {
		return exportAudioResult{Reason: "exceeds the 2 MiB audio export budget"}
	}
	file, err := os.Open(path)
	if err != nil {
		return exportAudioResult{Reason: "file unavailable"}
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != info.Size() || !os.SameFile(info, opened) {
		return exportAudioResult{Reason: "file changed during export"}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHTMLExportInlineAudioBytes+1))
	if err != nil || int64(len(data)) != opened.Size() {
		return exportAudioResult{Reason: "file changed or unreadable"}
	}
	// Inspect the exact snapshot that will be embedded, not a previous header.
	format, err := mediatype.Inspect(bytes.NewReader(data), int64(len(data)))
	if err != nil || format.Kind != mediatype.Audio || format.MIME != declared.MIME {
		return exportAudioResult{Reason: "invalid or mismatched audio bytes"}
	}
	sum := sha256.Sum256(data)
	if url, ok := e.content[sum]; ok {
		return exportAudioResult{URL: url}
	}
	if len(data) > maxHTMLExportInlineAudioBytes-e.used {
		return exportAudioResult{Reason: "exceeds the 2 MiB audio export budget"}
	}
	url := fmt.Sprintf("data:%s;base64,%s", format.MIME, base64.StdEncoding.EncodeToString(data))
	e.used += len(data)
	e.content[sum] = url
	return exportAudioResult{URL: url}
}

func buildHTMLAudio(audio *exportAudio, item core.MediaArtifact, label string) htmlExportAudio {
	name := exportAudioName(item)
	label = exportAudioText(label, item)
	caption := exportAudioText(item.Caption, item)
	if label == "" {
		label = caption
	}
	if label == "" {
		label = name
	}
	result := audio.resolve(item)
	view := htmlExportAudio{Name: name, Label: label, Caption: caption, Reason: result.Reason, Omitted: result.URL == ""}
	if result.URL != "" {
		// Only allowlisted, re-inspected exporter-generated data URLs reach here.
		id, repeated := audio.players[result.URL]
		if !repeated {
			id = fmt.Sprintf("audio-%d", len(audio.players)+1)
			audio.players[result.URL] = id
			view.URL = result.URL // validated generated audio data URL.
		}
		view.ID, view.Repeated = id, repeated
	} else {
		key := filepath.Clean(item.Path()) + "\x00" + normalizedMediaType(item.MediaType)
		if item.Path() == "" {
			key = item.Reference
		}
		audio.omitted[key] = true
	}
	return view
}

// Never use private paths/URLs as display fallbacks, including metadata which
// happens to repeat the artifact's private locations.
func exportAudioText(value string, item core.MediaArtifact) string {
	for _, private := range []string{item.SourcePath, item.StoredPath} {
		if private != "" {
			value = strings.ReplaceAll(value, private, "")
		}
	}
	if strings.Contains(value, "://") || strings.Contains(value, "/media/") {
		return ""
	}
	return strings.Join(strings.Fields(value), " ")
}

func (e *exportAudio) result() ExportResult {
	return ExportResult{AudioEmbedded: len(e.players), AudioOmitted: len(e.omitted)}
}

// exportAudioName is shared by HTML cards and text-only Markdown placeholders.
func exportAudioName(item core.MediaArtifact) string {
	name := filepath.Base(strings.ReplaceAll(item.Name, `\`, "/"))
	name = exportAudioText(name, item)
	if name == "" || name == "." || name == "/" {
		return "Audio recording"
	}
	return name
}
