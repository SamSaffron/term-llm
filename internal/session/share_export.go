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
// the assistant's rendered text and displayed images; conversation shares
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

	switch scope {
	case ShareScopeConversation:
		return ShareSelection{Messages: VisibleExportMessages(ordered[:anchorIndex+1])}, nil
	case ShareScopeResponse:
		anchor := ordered[anchorIndex]
		start := anchorIndex
		for start > 0 && ordered[start-1].Role != llm.RoleUser {
			start--
		}
		parts := make([]string, 0, anchorIndex-start+1)
		// Keep images the client displayed in this response, in order, without
		// exposing the tool activity that produced them.
		var responseParts []llm.Part
		var pendingText []string
		var media []llm.MediaArtifact
		flushText := func() {
			if len(pendingText) > 0 {
				responseParts = append(responseParts, llm.Part{Type: llm.PartText, Text: strings.Join(pendingText, "\n\n")})
				pendingText = nil
			}
		}
		for i := start; i <= anchorIndex; i++ {
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
						flushText()
						responseParts = append(responseParts, llm.Part{Type: llm.PartImage, ImagePath: source.Path})
					}
				}
				continue
			}
			if msg.Role != llm.RoleAssistant || !shareMessageVisible(msg) {
				continue
			}
			if text := strings.TrimSpace(msg.TextContent); text != "" {
				parts = append(parts, text)
				pendingText = append(pendingText, text)
			}
		}
		if len(parts) == 0 {
			return ShareSelection{}, ErrInvalidShareAnchor
		}
		flushText()
		response := anchor
		response.Parts = responseParts
		response.TextContent = strings.Join(parts, "\n\n")
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
