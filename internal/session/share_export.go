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

// SelectShareMessages validates anchorMessageID and returns an authoritative,
// human-visible subset for a point-in-time share. Response shares contain only
// the assistant's rendered text; conversation shares include the transcript up
// to and including the anchored row.
func SelectShareMessages(messages []Message, anchorMessageID int64, scope ShareScope) ([]Message, error) {
	if anchorMessageID <= 0 {
		return nil, ErrInvalidShareAnchor
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
		return nil, ErrInvalidShareAnchor
	}

	switch scope {
	case ShareScopeConversation:
		return VisibleExportMessages(ordered[:anchorIndex+1]), nil
	case ShareScopeResponse:
		anchor := ordered[anchorIndex]
		start := anchorIndex
		for start > 0 && ordered[start-1].Role != llm.RoleUser {
			start--
		}
		parts := make([]string, 0, anchorIndex-start+1)
		for i := start; i <= anchorIndex; i++ {
			msg := ordered[i]
			if msg.Role != llm.RoleAssistant || !shareMessageVisible(msg) {
				continue
			}
			if anchor.ResponseID != "" && msg.ResponseID != anchor.ResponseID {
				continue
			}
			if text := strings.TrimSpace(msg.TextContent); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) == 0 {
			return nil, ErrInvalidShareAnchor
		}
		response := anchor
		response.Parts = llm.AssistantText(strings.Join(parts, "\n\n")).Parts
		response.TextContent = strings.Join(parts, "\n\n")
		response.CompactionTail = false
		return []Message{response}, nil
	default:
		return nil, ErrInvalidShareScope
	}
}

func shareMessageVisible(message Message) bool {
	if message.CompactionTail || message.IsGoalSteering() || llm.IsInternalCompactionSummaryText(message.TextContent) {
		return false
	}
	return true
}
