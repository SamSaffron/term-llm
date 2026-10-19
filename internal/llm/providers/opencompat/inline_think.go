package opencompat

import "strings"

type inlineThinkPart struct {
	Reasoning bool
	Text      string
}

type inlineThinkParser struct {
	inThink bool
	pending string
}

func (p *inlineThinkParser) Process(chunk string) []inlineThinkPart {
	if chunk == "" && p.pending == "" {
		return nil
	}
	s := p.pending + chunk
	p.pending = ""
	parts := make([]inlineThinkPart, 0, 2)
	for s != "" {
		if p.inThink {
			idx := strings.Index(s, "</think>")
			if idx >= 0 {
				appendInlineThinkPart(&parts, true, s[:idx])
				s = s[idx+len("</think>"):]
				p.inThink = false
				continue
			}
			emit, pending := splitPossibleTagSuffix(s, "</think>")
			appendInlineThinkPart(&parts, true, emit)
			p.pending = pending
			break
		}

		idx := strings.Index(s, "<think>")
		if idx >= 0 {
			appendInlineThinkPart(&parts, false, s[:idx])
			s = s[idx+len("<think>"):]
			p.inThink = true
			continue
		}
		emit, pending := splitPossibleTagSuffix(s, "<think>")
		appendInlineThinkPart(&parts, false, emit)
		p.pending = pending
		break
	}
	return parts
}

func (p *inlineThinkParser) Flush() []inlineThinkPart {
	if p.pending == "" {
		return nil
	}
	part := inlineThinkPart{Reasoning: p.inThink, Text: p.pending}
	p.pending = ""
	return []inlineThinkPart{part}
}

func appendInlineThinkPart(parts *[]inlineThinkPart, reasoning bool, text string) {
	if text == "" {
		return
	}
	last := len(*parts) - 1
	if last >= 0 && (*parts)[last].Reasoning == reasoning {
		(*parts)[last].Text += text
		return
	}
	*parts = append(*parts, inlineThinkPart{Reasoning: reasoning, Text: text})
}

func splitPossibleTagSuffix(s, tag string) (emit, pending string) {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return s[:len(s)-n], s[len(s)-n:]
		}
	}
	return s, ""
}
