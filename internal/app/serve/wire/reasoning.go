package wire

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/reasoning"
)

func ReasoningDisplay(kind, content, title string, cfg config.ReasoningConfig) (string, string) {
	content = reasoning.LimitReasoningText(kind, content, cfg)
	if kind == reasoning.KindSummary {
		if !cfg.ExtractTitles {
			title = ""
		} else if title == "" {
			title = reasoning.SummaryTitle(content, cfg)
		}
		return title, reasoning.SummaryBody(content, cfg)
	}
	return "", content
}

type ReasoningStream struct {
	cfg          *config.ReasoningConfig
	nextIndex    int
	Active       bool
	index        int
	kind         core.ReasoningKind
	content      strings.Builder
	contentRunes int
	itemID       string
	body, title  string
}

func ReasoningBoundary(ev core.Event) bool {
	switch ev.Type {
	case core.EventTextDelta:
		return ev.Text != ""
	case core.EventToolActivity:
		return ev.ToolActivity != nil
	case core.EventToolCall, core.EventToolExecStart, core.EventToolExecEnd, core.EventSteering, core.EventCompaction, core.EventModelSwitch, core.EventAttemptDiscard:
		return true
	}
	return false
}

func (r *ReasoningStream) Configured() bool                     { return r.cfg != nil }
func (r *ReasoningStream) Configure(cfg config.ReasoningConfig) { r.cfg = &cfg }

// NextIndex is the index the next burst will use. A rebuilt run stream state
// (reload, fallback retry) must carry it on: clients and recovery snapshots
// key thoughts by index, so a reused index would append to an older thought.
func (r *ReasoningStream) NextIndex() int { return r.nextIndex }

// Continue returns an idle stream that keeps this run's numbering.
func (r *ReasoningStream) Continue() ReasoningStream { return ResumeReasoningStream(r.nextIndex) }

// ResumeReasoningStream returns an idle stream whose next burst uses index next.
func ResumeReasoningStream(next int) ReasoningStream { return ReasoningStream{nextIndex: next} }

func (r *ReasoningStream) Append(ev core.Event) map[string]any {
	// Never accumulate opaque or unclassified provider payloads, even alongside safe text.
	if core.IsEncryptedReasoningDelta(ev) || core.NormalizeReasoningKind(ev.ReasoningKind) == core.ReasoningKindUnknown {
		return nil
	}
	if !r.Active {
		r.Active = true
		r.index = r.nextIndex
		r.nextIndex++
		r.kind = ""
		r.content.Reset()
		r.contentRunes = 0
		r.itemID = ""
		r.body = ""
		r.title = ""
	}
	previousKind := r.kind
	r.kind = core.MergeReasoningKind(r.kind, ev.ReasoningKind)
	if !reasoning.InteractiveSourceAllows(string(r.kind), *r.cfg) {
		// Retract the safe projection using its allowed kind; never transport a disallowed kind.
		if r.body != "" || r.title != "" {
			r.body = ""
			r.title = ""
			return map[string]any{"reasoning_index": r.index, "kind": string(previousKind), "title": "", "text": ""}
		}
		return nil
	}
	displayLimit, bufferLimit := r.limits()
	wasFull := displayLimit > 0 && r.contentRunes >= displayLimit
	r.appendItemText(ev.Text, ev.ReasoningItemID, bufferLimit)
	// Classification and source policy must run even after the display fills:
	// a summary can become raw, requiring replacement or retraction. Retain a
	// bounded lookahead only when that transition could expose a longer prefix.
	if wasFull && previousKind == r.kind {
		return nil
	}
	title, body := ReasoningDisplay(string(r.kind), r.content.String(), "", *r.cfg)
	// A visible row still needs its classification updated when its text is
	// unchanged. Metadata-only events must not create an empty row, though.
	visibleKindChanged := previousKind != r.kind && (r.body != "" || r.title != "")
	if title == r.title && body == r.body && !visibleKindChanged {
		return nil
	}
	payload := map[string]any{"reasoning_index": r.index, "kind": string(r.kind), "title": title}
	if strings.HasPrefix(body, r.body) {
		payload["delta"] = strings.TrimPrefix(body, r.body)
	} else {
		payload["text"] = body
	}
	r.title, r.body = title, body
	return payload
}

// limits returns the current display cap and the largest prefix a later allowed
// kind can expose. Raw wins kind merges, so only summaries need lookahead.
// Nonpositive limits retain their existing meaning of unlimited text.
func (r *ReasoningStream) limits() (display, buffer int) {
	if r.kind == core.ReasoningKindRaw {
		return r.cfg.MaxRawChars, r.cfg.MaxRawChars
	}
	display, buffer = r.cfg.MaxSummaryChars, r.cfg.MaxSummaryChars
	if reasoning.InteractiveSourceAllows(reasoning.KindRaw, *r.cfg) {
		if display <= 0 || r.cfg.MaxRawChars <= 0 {
			buffer = 0
		} else {
			buffer = max(buffer, r.cfg.MaxRawChars)
		}
	}
	return display, buffer
}

func (r *ReasoningStream) appendItemText(text, itemID string, limit int) {
	if text == "" || limit > 0 && r.contentRunes >= limit {
		return
	}
	if r.content.Len() > 0 && itemID != "" && r.itemID != "" && itemID != r.itemID {
		r.appendPrefix(reasoningItemSeparator(r.content.String(), text), limit)
	}
	r.appendPrefix(text, limit)
	if itemID != "" {
		r.itemID = itemID
	}
}

// appendPrefix writes only complete runes that fit, without retaining the rest
// of a potentially large provider delta or rebuilding the accumulated prefix.
func (r *ReasoningStream) appendPrefix(text string, limit int) {
	for i := range text {
		if limit > 0 && r.contentRunes >= limit {
			text = text[:i]
			break
		}
		r.contentRunes++
	}
	r.content.WriteString(text)
}

// reasoningItemSeparator delegates paragraph/CRLF rules to the shared item
// accumulator. Two CRLFs on each side are enough to determine the separator;
// using only these edges avoids copying either the buffer or the full delta.
func reasoningItemSeparator(previous, next string) string {
	previous = previous[max(0, len(previous)-4):]
	next = next[:min(4, len(next))]
	var probe strings.Builder
	probe.WriteString(previous)
	itemID := "previous"
	reasoning.AppendStreamItemText(&probe, &itemID, next, "next")
	return probe.String()[len(previous) : probe.Len()-len(next)]
}

// ReasoningETag keeps user-controlled labels out of HTTP header values.
func ReasoningETag(policy map[string]any) string {
	data, _ := json.Marshal(policy)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
