package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/ui"
)

const (
	// Longer multi-line notices, and any that start with a heading, open the
	// content dialog instead of collapsing into the footer.
	maxFooterNoticeLines = 4
	maxFooterNoticeChars = 200
)

// showAltScreenSystemMessage shows multi-line command output while the chat
// owns the alternate screen. Short notices (usage hints, unknown commands)
// collapse into the footer; longer output opens a scrollable content dialog
// titled by its first line.
func (m *Model) showAltScreenSystemMessage(content string) (tea.Model, tea.Cmd) {
	if notice, ok := footerNotice(content); ok {
		return m.showFooterMessageWithToneFor(notice, "", 2*transientFooterMessageDuration)
	}
	title, body, _ := strings.Cut(content, "\n")
	title = strings.TrimSuffix(sanitizeFooterMessage(strings.TrimLeft(title, "# ")), ":")
	m.completions.Hide()
	m.dialog.ShowContent(title, m.renderDialogMarkdown(strings.TrimSpace(body)))
	return m, nil
}

// footerNotice flattens a short multi-line notice into one footer line,
// dropping list bullets and separating its lines with " · ".
func footerNotice(content string) (string, bool) {
	if strings.HasPrefix(content, "#") {
		return "", false
	}
	var notice strings.Builder
	lines := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if line == "" {
			continue
		}
		if lines++; lines > 1 {
			if text := notice.String(); strings.HasSuffix(text, ":") || strings.HasSuffix(text, ",") {
				notice.WriteString(" ")
			} else {
				notice.WriteString(" · ")
			}
		}
		notice.WriteString(line)
	}
	text := sanitizeFooterMessage(notice.String())
	return text, lines <= maxFooterNoticeLines && len(text) <= maxFooterNoticeChars
}

// renderDialogMarkdown renders markdown to fit the content dialog's text area.
func (m *Model) renderDialogMarkdown(content string) string {
	if content == "" {
		return ""
	}
	if m.textMode {
		return wordwrap.String(content, m.dialogTextWidth())
	}
	return ui.RenderMarkdownWithOptions(content, m.dialogTextWidth(), ui.MarkdownRenderOptions{NormalizeTabs: true})
}

// dialogTextWidth is the content dialog's text area: its width minus border
// and padding. Pre-wrapping to it keeps the dialog from splitting words.
func (m *Model) dialogTextWidth() int {
	return max(20, m.dialog.contentWidth()-6)
}
