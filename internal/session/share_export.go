package session

import (
	"errors"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ShareScope identifies the transcript content included in a share.
type ShareScope string

const (
	ShareScopeSession      ShareScope = "session"
	ShareScopeResponse     ShareScope = "response"
	ShareScopeConversation ShareScope = "conversation"
)

var (
	ErrInvalidShareScope  = errors.New("session: invalid share scope")
	ErrInvalidShareAnchor = errors.New("session: invalid share anchor")
)

// ShareSelection is the authoritative content of a point-in-time share.
type ShareSelection struct {
	Messages []Message
	// Media holds artifacts referenced by term-llm-media:// URLs in Messages
	// whose producing tool results are not themselves part of the selection.
	Media []llm.MediaArtifact
}

// SelectShareMessages validates anchorMessageID and returns an authoritative,
// human-visible subset for a point-in-time share. Response shares contain only
// the assistant's final answer and displayed images; conversation shares
// include the transcript up to and including the anchored row.
func SelectShareMessages(messages []Message, anchorMessageID int64, scope ShareScope) ([]Message, error) {
	selection, err := SelectShare(messages, anchorMessageID, scope)
	return selection.Messages, err
}

// SelectShare is SelectShareMessages plus the media artifacts needed to
// render images in a response-only share.
func SelectShare(messages []Message, anchorMessageID int64, scope ShareScope) (ShareSelection, error) {
	if anchorMessageID <= 0 {
		return ShareSelection{}, ErrInvalidShareAnchor
	}
	ordered := append([]Message(nil), messages...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Sequence == ordered[j].Sequence {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Sequence < ordered[j].Sequence
	})
	anchorIndex := -1
	for i := range ordered {
		if ordered[i].ID == anchorMessageID {
			anchorIndex = i
			break
		}
	}
	if anchorIndex < 0 || ordered[anchorIndex].Role != llm.RoleAssistant || !shareMessageVisible(ordered[anchorIndex]) {
		return ShareSelection{}, ErrInvalidShareAnchor
	}

	// Some providers (for example the Claude CLI) persist a whole response as
	// one assistant row followed by its tool-result rows. Those results belong
	// to the anchored response, and their media is referenced by its text.
	end := anchorIndex
	anchorResponse := ordered[anchorIndex].ResponseID
	for end+1 < len(ordered) && ordered[end+1].Role == llm.RoleTool &&
		(anchorResponse == "" || ordered[end+1].ResponseID == anchorResponse) {
		end++
	}

	switch scope {
	case ShareScopeConversation:
		return ShareSelection{Messages: VisibleExportMessages(ordered[:end+1])}, nil
	case ShareScopeResponse:
		anchor := ordered[anchorIndex]
		start := anchorIndex
		for start > 0 && ordered[start-1].Role != llm.RoleUser {
			start--
		}
		// A response share is the answer, not the work: only text written
		// after the response's last tool call, preceded by the images shown
		// to the user during the response. Interim narration between tool
		// calls is dropped. If nothing follows the last tool call, the last
		// text written before it is used instead.
		var segments [][]string
		current := []string{}
		var images []llm.Part
		var media []llm.MediaArtifact
		closeSegment := func() {
			if len(current) > 0 {
				segments = append(segments, current)
				current = []string{}
			}
		}
		for i := start; i <= end; i++ {
			msg := ordered[i]
			if anchor.ResponseID != "" && msg.ResponseID != anchor.ResponseID {
				continue
			}
			if msg.Role == llm.RoleTool {
				for _, part := range msg.Parts {
					if part.Type != llm.PartToolResult || part.ToolResult == nil {
						continue
					}
					media = append(media, part.ToolResult.Media...)
					for _, source := range toolResultDisplayedImages(part.ToolResult) {
						images = append(images, llm.Part{Type: llm.PartImage, ImagePath: source.Path})
					}
				}
				continue
			}
			if msg.Role != llm.RoleAssistant || !shareMessageVisible(msg) {
				continue
			}
			parts := msg.Parts
			if len(parts) == 0 && msg.TextContent != "" {
				parts = []llm.Part{{Type: llm.PartText, Text: msg.TextContent}}
			}
			for _, part := range parts {
				switch part.Type {
				case llm.PartToolCall:
					closeSegment()
				case llm.PartText:
					if text := strings.TrimSpace(part.Text); text != "" {
						current = append(current, text)
					}
				}
			}
		}
		closeSegment()
		if len(segments) == 0 {
			return ShareSelection{}, ErrInvalidShareAnchor
		}
		finalText := strings.Join(segments[len(segments)-1], "\n\n")
		response := anchor
		response.Parts = append(images, llm.Part{Type: llm.PartText, Text: finalText})
		response.TextContent = finalText
		response.CompactionTail = false
		return ShareSelection{Messages: []Message{response}, Media: media}, nil
	default:
		return ShareSelection{}, ErrInvalidShareScope
	}
}

func shareMessageVisible(message Message) bool {
	if message.CompactionTail || message.IsGoalSteering() || llm.IsInternalCompactionSummaryText(message.TextContent) {
		return false
	}
	return true
}

// StripToolActivity removes tool calls, tool results, and tool-only output
// (diffs, tool screenshots) from a transcript so it can be shared without
// exposing commands, file contents, or credentials seen by tools. Images the
// client displayed to the user — image_generate output and unreferenced media
// — are kept as assistant images in their original position, and the returned
// media resolves term-llm-media:// references in assistant text. Consecutive
// assistant content from one response is merged into a single message.
func StripToolActivity(messages []Message) ([]Message, []llm.MediaArtifact) {
	out := make([]Message, 0, len(messages))
	var media []llm.MediaArtifact
	// mergeable is the index in out of the assistant message produced by this
	// function that later content of the same response may extend.
	mergeable := -1
	appendAssistant := func(source Message, parts []llm.Part) {
		if len(parts) == 0 {
			return
		}
		text := assistantPartsText(parts)
		if mergeable >= 0 && out[mergeable].ResponseID != "" && out[mergeable].ResponseID == source.ResponseID {
			target := &out[mergeable]
			target.Parts = append(target.Parts, parts...)
			if text != "" {
				if target.TextContent != "" {
					target.TextContent += "\n\n"
				}
				target.TextContent += text
			}
			return
		}
		msg := source
		msg.Role = llm.RoleAssistant
		msg.Parts = parts
		msg.TextContent = text
		out = append(out, msg)
		mergeable = len(out) - 1
	}
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleTool:
			var images []llm.Part
			for _, part := range msg.Parts {
				if part.Type != llm.PartToolResult || part.ToolResult == nil {
					continue
				}
				media = append(media, part.ToolResult.Media...)
				for _, source := range toolResultDisplayedImages(part.ToolResult) {
					images = append(images, llm.Part{Type: llm.PartImage, ImagePath: source.Path})
				}
			}
			appendAssistant(msg, images)
		case llm.RoleAssistant:
			parts := msg.Parts
			if len(parts) == 0 && msg.TextContent != "" {
				parts = []llm.Part{{Type: llm.PartText, Text: msg.TextContent}}
			}
			kept := make([]llm.Part, 0, len(parts))
			for _, part := range parts {
				if part.Type == llm.PartToolCall || part.Type == llm.PartToolResult {
					continue
				}
				kept = append(kept, part)
			}
			appendAssistant(msg, kept)
		default:
			out = append(out, msg)
			mergeable = -1
		}
	}
	return out, media
}

func assistantPartsText(parts []llm.Part) string {
	var texts []string
	for _, part := range parts {
		if part.Type == llm.PartText && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(texts, "\n\n")
}
