package session

import (
	_ "embed"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/htmlesc"
)

// The HTML export is rendered by hand rather than with html/template: template
// execution forces the linker to retain every exported method in the binary
// (see internal/htmlesc). Interpolated values go through htmlesc, which
// reproduces html/template's escaping for each context used here.

//go:embed export_html.css
var exportHTMLStyle string

//go:embed export_html.js
var exportHTMLScript string

const exportHTMLHead = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="theme-color" content="#f7f7f4">
`

func renderHTMLExport(v *htmlExportView) string {
	var b strings.Builder
	b.Grow(len(exportHTMLStyle) + len(exportHTMLScript) + 16<<10)
	label := "term-llm transcript"
	if v.ResponseOnly {
		label = "term-llm response"
	}

	b.WriteString(exportHTMLHead)
	b.WriteString(`<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:`)
	if v.AssetImages {
		b.WriteString(` 'self'`)
	}
	b.WriteString(`;`)
	if v.HasAudio {
		b.WriteString(` media-src data:;`)
	}
	b.WriteString(" style-src 'unsafe-inline'; script-src 'unsafe-inline';\">\n")
	b.WriteString(`<title>` + htmlesc.Text(v.Title) + ` · ` + label + "</title>\n")
	b.WriteString("<style>\n" + exportHTMLStyle + "</style>\n</head>\n<body>\n")

	b.WriteString("<header class=\"hero\"><div class=\"wrap\">\n")
	b.WriteString(`<div class="eyebrow">` + label + `</div><h1>` + htmlesc.Text(v.Title) + "</h1>\n")
	b.WriteString(`<div class="chips">`)
	if !v.ResponseOnly {
		b.WriteString(`<span class="chip status">` + htmlesc.Text(v.Status) + `</span>`)
		if v.Agent != "" {
			b.WriteString(`<span class="chip">Agent: ` + htmlesc.Text(v.Agent) + `</span>`)
		}
	}
	if v.ModelLabel != "" {
		b.WriteString(`<span class="chip">` + htmlesc.Text(v.ModelLabel) + `</span>`)
	}
	if !v.ResponseOnly {
		b.WriteString(`<span class="chip">` + htmlesc.Text(v.Mode))
		if v.Origin != "" {
			b.WriteString(` · ` + htmlesc.Text(v.Origin))
		}
		b.WriteString(`</span>`)
		if v.Reasoning != "" {
			b.WriteString(`<span class="chip">Reasoning: ` + htmlesc.Text(v.Reasoning) + `</span>`)
		}
	}
	b.WriteString("</div>\n")
	b.WriteString(`<div class="controls" role="group" aria-label="Transcript controls"><button type="button" data-action="theme"><span class="control-icon" aria-hidden="true">◐</span><span data-control-label>Dark mode</span></button>`)
	if v.DetailCount != 0 {
		b.WriteString(`<button type="button" data-action="details" aria-pressed="false"><span class="control-icon" aria-hidden="true">＋</span><span data-control-label>Expand details</span></button>`)
	}
	b.WriteString("</div>\n</div></header>\n<main class=\"wrap\">\n")

	if !v.ResponseOnly {
		b.WriteString("<section aria-labelledby=\"setup\"><h2 id=\"setup\" class=\"section-title\">Session</h2><div class=\"metadata\">\n")
		writeExportCard(&b, "Created", v.Created)
		writeExportCard(&b, "Updated", v.Updated)
		b.WriteString("\n")
		if v.CWD != "" {
			writeExportCard(&b, "Working directory", v.CWD)
		}
		if v.Worktree != "" {
			writeExportCard(&b, "Worktree", v.Worktree)
		}
		b.WriteString("\n")
		if v.Tags != "" {
			writeExportCard(&b, "Tags", v.Tags)
		}
		writeExportCard(&b, "Session", v.SessionID)
		b.WriteString("\n</div>")
		if v.Tools != "" || v.MCP != "" {
			b.WriteString(`<details><summary>Tools and integrations</summary><div class="details-body">`)
			if v.Tools != "" {
				b.WriteString(`<p><strong>Tools:</strong> ` + htmlesc.Text(v.Tools) + `</p>`)
			}
			if v.MCP != "" {
				b.WriteString(`<p><strong>MCP:</strong> ` + htmlesc.Text(v.MCP) + `</p>`)
			}
			b.WriteString(`</div></details>`)
		}
		b.WriteString(`</section>`)
	}
	b.WriteString("\n")

	if !v.Partial && !v.ResponseOnly {
		b.WriteString("<section aria-labelledby=\"metrics\"><h2 id=\"metrics\" class=\"section-title\">Activity</h2><div class=\"stats\">\n")
		writeExportCard(&b, "User turns", v.UserTurns)
		writeExportCard(&b, "LLM turns", v.LLMTurns)
		writeExportCard(&b, "Tool calls", v.ToolCalls)
		writeExportCard(&b, "Input tokens", v.Input)
		writeExportCard(&b, "Cached input", v.Cached)
		writeExportCard(&b, "Cache write", v.CacheWrite)
		writeExportCard(&b, "Output tokens", v.Output)
		b.WriteString("\n</div></section>")
	}
	b.WriteString("\n")

	conversation := "Conversation"
	if v.ResponseOnly {
		conversation = "Response"
	}
	b.WriteString(`<section aria-labelledby="conversation"><h2 id="conversation" class="section-title">` + conversation + "</h2><div class=\"timeline\">\n")
	for i := range v.Messages {
		writeExportMessage(&b, &v.Messages[i])
	}
	b.WriteString("\n</div></section>\n")
	b.WriteString("</main><footer class=\"footer wrap\"><a href=\"https://term-llm.com/\">Exported from term-llm</a></footer>\n\n")
	b.WriteString("<script>" + strings.TrimSuffix(exportHTMLScript, "\n") + "</script>\n</body></html>\n\n\n")
	return b.String()
}

func writeExportCard(b *strings.Builder, label, value string) {
	b.WriteString(`<div class="card"><span class="label">` + label + `</span><span class="value">` + htmlesc.Text(value) + `</span></div>`)
}

func writeExportMessage(b *strings.Builder, m *htmlExportMessage) {
	b.WriteString(`<article class="message ` + htmlesc.Text(m.Role))
	if m.Compaction {
		b.WriteString(` compaction`)
	}
	b.WriteString(`"><header class="message-head"><span class="role">`)
	if m.Compaction {
		b.WriteString(`Compaction`)
	} else {
		b.WriteString(htmlesc.Text(m.RoleLabel))
	}
	b.WriteString(`</span><span class="message-meta">` + htmlesc.Text(m.Time))
	if m.Duration != "" {
		if m.Time != "" {
			b.WriteString(` · `)
		}
		b.WriteString(htmlesc.Text(m.Duration))
	}
	b.WriteString("</span></header>\n")

	systemWrap := m.Role == "system" && !m.Compaction
	if systemWrap {
		b.WriteString(`<details><summary>System content</summary><div class="details-body">`)
	}
	if m.ToolGroup {
		b.WriteString(`<details class="tool-group"><summary>` + strconv.Itoa(m.ToolCount) + ` tool calls completed</summary><div class="details-body">`)
	}
	for i := range m.Blocks {
		writeExportBlock(b, &m.Blocks[i])
	}
	if m.ToolGroup {
		b.WriteString(`</div></details>`)
	}
	if systemWrap {
		b.WriteString(`</div></details>`)
	}
	b.WriteString(`</article>`)
}

func writeExportBlock(b *strings.Builder, block *htmlExportBlock) {
	switch block.Kind {
	case "markdown":
		// HTML is safe Goldmark output (see renderSafeMarkdown).
		b.WriteString(`<div class="content">` + block.HTML + `</div>`)
	case "reasoning":
		b.WriteString(`<details class="reasoning`)
		if block.Raw {
			b.WriteString(` raw`)
		}
		b.WriteString(`"><summary>`)
		if block.Raw {
			b.WriteString(`Raw reasoning — explicitly included`)
		} else {
			b.WriteString(`Reasoning summary`)
			if block.Title != "" {
				b.WriteString(`: ` + htmlesc.Text(block.Title))
			}
		}
		b.WriteString(`</summary><div class="details-body content">` + block.HTML + `</div></details>`)
	// The template failed the whole export on a nil payload; skipping the block
	// is deliberate.
	case "tool":
		if block.Tool != nil {
			writeExportTool(b, block.Tool)
		}
	case "image":
		if block.Image != nil {
			writeExportImage(b, block.Image)
		}
	case "audio":
		if block.Audio != nil {
			writeExportAudio(b, block.Audio)
		}
	case "file":
		if f := block.File; f != nil {
			b.WriteString(`<div class="attachment"><strong>` + htmlesc.Text(f.Filename) + `</strong><span class="attachment-meta">` + htmlesc.Text(f.MediaType))
			if f.MediaType != "" && f.Size != "" {
				b.WriteString(` · `)
			}
			b.WriteString(htmlesc.Text(f.Size) + `</span></div>`)
		}
	}
}

func writeExportTool(b *strings.Builder, tool *htmlExportTool) {
	b.WriteString(`<details class="tool-card`)
	if tool.IsError {
		b.WriteString(` error`)
	}
	b.WriteString(`"><summary>` + htmlesc.Text(tool.Name))
	switch {
	case tool.IsError:
		b.WriteString(`<span class="tool-state">error</span>`)
	case tool.HasResult:
		b.WriteString(`<span class="tool-state">complete</span>`)
	default:
		b.WriteString(`<span class="tool-state">no result</span>`)
	}
	if tool.ID != "" {
		b.WriteString(`<span class="tool-id">` + htmlesc.Text(tool.ID) + `</span>`)
	}
	b.WriteString(`</summary><div class="details-body">`)
	if tool.HasCall {
		b.WriteString(`<div class="tool-section"><h4>Arguments</h4><pre>` + htmlesc.Text(tool.Arguments) + `</pre></div>`)
	} else {
		b.WriteString(`<p class="omitted">No matching tool call was stored.</p>`)
	}
	if tool.HasResult {
		heading := "Result"
		if tool.IsError {
			heading = "Error"
		}
		b.WriteString(`<div class="tool-section"><h4>` + heading + `</h4><pre>` + htmlesc.Text(tool.Result) + `</pre></div>`)
		for _, text := range tool.ExtraTexts {
			b.WriteString(`<pre>` + htmlesc.Text(text) + `</pre>`)
		}
		for _, diff := range tool.Diffs {
			b.WriteString(`<div class="diff"><strong>` + htmlesc.Text(diff.File))
			if diff.Line != 0 {
				b.WriteString(`:` + strconv.Itoa(diff.Line))
			}
			b.WriteString(`</strong><div class="diff-grid"><div><span class="label">Before</span><pre>` + htmlesc.Text(diff.Old) + `</pre></div><div><span class="label">After</span><pre>` + htmlesc.Text(diff.New) + `</pre></div></div></div>`)
		}
		for i := range tool.Images {
			writeExportImage(b, &tool.Images[i])
		}
	}
	b.WriteString(`</div></details>`)
}

func writeExportImage(b *strings.Builder, image *htmlExportImage) {
	b.WriteString(`<div class="media">`)
	if image.Omitted {
		b.WriteString(`<span class="omitted">Image omitted`)
		if image.Reason != "" {
			b.WriteString(` — ` + htmlesc.Text(image.Reason))
		}
		if image.MediaType != "" {
			b.WriteString(` (` + htmlesc.Text(image.MediaType) + `)`)
		}
		b.WriteString(`</span>`)
	} else {
		// URL is exporter-generated and validated (see exportImages.resolve).
		b.WriteString(`<img src="` + htmlesc.TrustedURL(image.URL) + `" alt="` + htmlesc.Text(image.Alt) + `" loading="lazy">`)
	}
	b.WriteString(`</div>`)
}

// writeExportAudio renders an audio card. It is shared by typed audio blocks
// and the Goldmark renderer for inline audio references, so model-supplied
// Markdown never produces raw HTML.
func writeExportAudio(b interface{ WriteString(string) (int, error) }, audio *htmlExportAudio) {
	_, _ = b.WriteString(`<span class="audio-media">`)
	switch {
	case audio.Omitted:
		_, _ = b.WriteString(`<span class="omitted">Audio: ` + htmlesc.Text(audio.Name) + ` (` + htmlesc.Text(audio.Reason) + `)</span>`)
	case audio.Repeated:
		_, _ = b.WriteString(`<a href="#` + htmlesc.URLComponent(audio.ID) + `">` + htmlesc.Text(audio.Label) + ` — same recording as above</a>`)
	default:
		_, _ = b.WriteString(`<strong>` + htmlesc.Text(audio.Label) + `</strong><span class="attachment-meta">` + htmlesc.Text(audio.Name) + `</span>`)
		if audio.Caption != "" && audio.Caption != audio.Label {
			_, _ = b.WriteString(`<span>` + htmlesc.Text(audio.Caption) + `</span>`)
		}
		// URL is a validated exporter-generated data URL (see buildHTMLAudio).
		_, _ = b.WriteString(`<audio controls id="` + htmlesc.Text(audio.ID) + `" preload="metadata" src="` + htmlesc.TrustedURL(audio.URL) + `" aria-label="` + htmlesc.Text(audio.Label) + `"></audio><a href="#` + htmlesc.URLComponent(audio.ID) + `" data-audio-download="` + htmlesc.Text(audio.ID) + `" download="` + htmlesc.Text(audio.Name) + `">Download audio</a>`)
	}
	_, _ = b.WriteString(`</span>`)
}
