package chat

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	copyUsage          = "Usage: /copy [N] where N is 1 (latest), 2, 3, …"
	copyStatusDuration = 3 * time.Second
)

var copyTextBestEffort = clipboard.CopyTextBestEffort

type copyResultKind uint8

const (
	copyResultStatus copyResultKind = iota + 1
	copyResultSuccess
	copyResultFailure
)

type copyResultMsg struct {
	kind        copyResultKind
	status      string
	err         error
	method      clipboard.CopyMethod
	runeCount   int
	targetLabel string
}

type copyStatusClearMsg struct {
	seq uint64
}

type completedResponseResolution struct {
	Text        string
	LoadedCount int
	Found       bool
}

// resolveCompletedAssistantResponse selects one source-visible assistant
// response from a completed transcript snapshot. Response IDs are authoritative
// when present; legacy rows fall back to adjacent assistant/tool runs.
func resolveCompletedAssistantResponse(messages []session.Message, ordinal int) completedResponseResolution {
	var responses []string
	for i := 0; i < len(messages); {
		start := i
		if responseID := messages[i].ResponseID; responseID != "" {
			i++
			for i < len(messages) && messages[i].ResponseID == responseID {
				i++
			}
		} else if messages[i].Role == llm.RoleAssistant || messages[i].Role == llm.RoleTool {
			i++
			for i < len(messages) && messages[i].ResponseID == "" &&
				(messages[i].Role == llm.RoleAssistant || messages[i].Role == llm.RoleTool) {
				i++
			}
		} else {
			i++
			continue
		}

		text := assistantResponseSourceText(messages[start:i])
		if strings.TrimSpace(text) != "" {
			responses = append(responses, text)
		}
	}

	result := completedResponseResolution{LoadedCount: len(responses)}
	if ordinal <= 0 || ordinal > len(responses) {
		return result
	}
	result.Text = responses[len(responses)-ordinal]
	result.Found = true
	return result
}

func assistantResponseSourceText(messages []session.Message) string {
	var contributions []string
	for _, message := range messages {
		if message.Role != llm.RoleAssistant ||
			session.IsSyntheticCompactionAckMessage(message) ||
			session.IsInternalCompactionSummaryMessage(message) {
			continue
		}

		for _, part := range message.Parts {
			if part.Type == llm.PartText && part.Text != "" {
				contributions = append(contributions, part.Text)
			}
		}
		// TextContent can contain text extracted from non-text parts (notably
		// PartFile), so it is a safe compatibility fallback only for genuinely
		// legacy rows that predate persisted Parts.
		if len(message.Parts) == 0 && message.TextContent != "" {
			contributions = append(contributions, message.TextContent)
		}
	}
	return strings.Join(contributions, "\n")
}

func (m *Model) cmdCopy(args []string) (tea.Model, tea.Cmd) {
	ordinal := 1
	explicitOrdinal := len(args) > 0
	if len(args) > 1 || (explicitOrdinal && !parseCopyOrdinal(args[0], &ordinal)) {
		return m, copyStatusCmd(copyUsage)
	}

	if !explicitOrdinal && m.streaming {
		live := m.currentResponse.String()
		if strings.TrimSpace(live) != "" {
			return m, copyTextCmd(live, "latest response")
		}
	}

	m.messagesMu.Lock()
	messages := append([]session.Message(nil), m.messages...)
	m.messagesMu.Unlock()
	resolved := resolveCompletedAssistantResponse(messages, ordinal)
	if !resolved.Found {
		if explicitOrdinal {
			return m, copyStatusCmd(loadedAssistantResponsesStatus(resolved.LoadedCount))
		}
		return m, copyStatusCmd("Nothing to copy yet")
	}

	target := "latest response"
	if explicitOrdinal {
		target = fmt.Sprintf("response %d", ordinal)
	}
	return m, copyTextCmd(resolved.Text, target)
}

func loadedAssistantResponsesStatus(count int) string {
	if count == 1 {
		return "Only 1 assistant response is loaded"
	}
	return fmt.Sprintf("Only %d assistant responses are loaded", count)
}

func parseCopyOrdinal(raw string, ordinal *int) bool {
	if raw == "" {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return false
		}
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return false
	}
	*ordinal = value
	return true
}

func copyTextCmd(text, targetLabel string) tea.Cmd {
	runeCount := utf8.RuneCountInString(text)
	return func() tea.Msg {
		method, err := copyTextBestEffort(text)
		kind := copyResultSuccess
		if err != nil {
			kind = copyResultFailure
		}
		return copyResultMsg{kind: kind, err: err, method: method, runeCount: runeCount, targetLabel: targetLabel}
	}
}

func copyStatusCmd(status string) tea.Cmd {
	return func() tea.Msg {
		return copyResultMsg{kind: copyResultStatus, status: status}
	}
}

func (m *Model) handleCopyResult(msg copyResultMsg) (tea.Model, tea.Cmd) {
	switch msg.kind {
	case copyResultStatus:
		m.copyStatus = msg.status
	case copyResultSuccess:
		m.copyStatus = fmt.Sprintf("Copied %s · %s", msg.targetLabel, formatCopyCharacterCount(msg.runeCount))
		if msg.method == clipboard.CopyMethodOSC52 {
			m.copyStatus += " · OSC 52"
		}
	case copyResultFailure:
		if msg.err == nil {
			m.copyStatus = "Copy failed: clipboard delivery returned no error"
		} else {
			m.copyStatus = "Copy failed: " + msg.err.Error()
		}
	default:
		m.copyStatus = "Copy failed: invalid copy result"
	}

	m.copyStatusSeq++
	seq := m.copyStatusSeq
	return m, tea.Tick(copyStatusDuration, func(time.Time) tea.Msg {
		return copyStatusClearMsg{seq: seq}
	})
}

func formatCopyCharacterCount(value int) string {
	label := "chars"
	if value == 1 {
		label = "char"
	}
	return formatCopyCount(value) + " " + label
}

func formatCopyCount(value int) string {
	digits := strconv.Itoa(value)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}
