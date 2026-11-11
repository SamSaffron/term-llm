package memory

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/secrettext"
	"github.com/samsaffron/term-llm/internal/session"
)

var errMemorySafety = errors.New("memory content rejected by native safety validation")

// memoryProtection is a compiled, additive-only data policy. A nil value still
// applies the built-in detectors; neither environment variables nor config can
// disable them. No policy field names executable code or a shell command.
type memoryProtection struct{ additional []*regexp.Regexp }

func newMemoryProtection(cfg config.MemoryConfig) (*memoryProtection, error) {
	if len(cfg.SecretPatterns) > 32 {
		return nil, errors.New("memory.secret_patterns exceeds 32 entries")
	}
	p := &memoryProtection{}
	for i, pattern := range cfg.SecretPatterns {
		if len(pattern) > 1024 {
			return nil, fmt.Errorf("memory.secret_patterns entry %d exceeds 1024 bytes", i+1)
		}
		re, err := regexp.Compile(pattern)
		if err != nil || re.MatchString("") {
			return nil, fmt.Errorf("invalid memory.secret_patterns entry %d", i+1)
		}
		p.additional = append(p.additional, re)
	}
	return p, nil
}

func (e Env) withProtection(cfg *config.Config) (Env, error) {
	if e.protection != nil {
		return e, nil
	}
	p, err := newMemoryProtection(cfg.Memory)
	e.protection = p
	return e, err
}

func (p *memoryProtection) sanitize(text string) string {
	text = strings.ToValidUTF8(text, "")
	// Evaluate every added detector against the original field, never against
	// another detector's replacement. A match redacts the complete field. This
	// conservative projection avoids an ordering-dependent regex/span framework.
	if p != nil {
		for _, re := range p.additional {
			if re.MatchString(text) || re.MatchString(strings.TrimSpace(text)) {
				return "[redacted]"
			}
		}
	}
	return secrettext.Redact(text)
}

func (p *memoryProtection) check(text string) error {
	if !utf8.ValidString(text) || p.sanitize(text) != text {
		return errMemorySafety
	}
	return nil
}

func excludedMemoryPath(fragPath string) bool {
	fragPath = path.Clean(strings.TrimSpace(strings.ReplaceAll(fragPath, "\\", "/")))
	return strings.HasPrefix(fragPath, "fragments/credentials/") || strings.HasSuffix(fragPath, "/accounts-and-credentials.md")
}

func (p *memoryProtection) fragment(path, content string) error {
	normalized, err := validateFragmentPath(path)
	if err != nil {
		return errMemorySafety
	}
	if excludedMemoryPath(normalized) || strings.TrimSpace(content) == "" || len(content) > 8192 {
		return errMemorySafety
	}
	if err := p.check(path); err != nil {
		return err
	}
	if err := p.check(normalized); err != nil {
		return err
	}
	if err := p.check(content); err != nil {
		return err
	}
	return p.check(strings.TrimSpace(content))
}

var invalidRecentLine = regexp.MustCompile(`(?im)^\s*(?:i (?:cannot|can't|won't|am unable)\b|sorry[,!]|error:|traceback\b|provider error\b|rate limit\b|unauthorized\b|as an ai\b)`)

func (p *memoryProtection) recent(content string, maxBytes int) error {
	if len(content) > maxBytes || maxBytes <= 0 || !strings.HasPrefix(content, "#") || strings.Contains(content, "```") || invalidRecentLine.MatchString(content) {
		return errMemorySafety
	}
	if err := p.check(content); err != nil {
		return err
	}
	pendingDepth := 0
	body := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			depth := len(line) - len(strings.TrimLeft(line, "#"))
			if pendingDepth != 0 && depth <= pendingDepth {
				return errMemorySafety
			}
			pendingDepth = depth
		} else if strings.Trim(line, "-*_> ") != "" {
			body = true
			pendingDepth = 0
		}
	}
	if !body || pendingDepth != 0 {
		return errMemorySafety
	}
	return nil
}

// Taxonomy reads only paths. Sanitize each entire path before directory names
// or budget selection can split a recognizable credential across boundaries.
func (p *memoryProtection) taxonomy(fragments []memorydb.Fragment, maxTokens int) string {
	safe := make([]memorydb.Fragment, 0, len(fragments))
	for _, frag := range fragments {
		if excludedMemoryPath(frag.Path) {
			continue
		}
		frag.Path = p.sanitize(frag.Path)
		safe = append(safe, frag)
	}
	return buildTaxonomyMap(safe, maxTokens)
}

// Tool summaries also contain source text. Copy before sanitizing so clipping
// ToolInfo or a name cannot destroy its detection context, without mutating DB
// message objects or provider arguments that are never used as mining evidence.
func (p *memoryProtection) message(msg session.Message) session.Message {
	msg.TextContent = p.sanitize(msg.TextContent)
	msg.Parts = append(msg.Parts[:0:0], msg.Parts...)
	for i := range msg.Parts {
		if call := msg.Parts[i].ToolCall; call != nil {
			safe := *call
			safe.Name = p.sanitize(call.Name)
			safe.ToolInfo = p.sanitize(call.ToolInfo)
			msg.Parts[i].ToolCall = &safe
		}
		if result := msg.Parts[i].ToolResult; result != nil {
			safe := *result
			safe.Name = p.sanitize(result.Name)
			msg.Parts[i].ToolResult = &safe
		}
	}
	return msg
}

func (p *memoryProtection) promotePrompt(changed []memorydb.Fragment, existingRecent string) string {
	safe := make([]memorydb.Fragment, 0, len(changed))
	for _, frag := range changed {
		if excludedMemoryPath(frag.Path) {
			continue
		}
		frag.Path = p.sanitize(frag.Path)
		frag.Content = p.sanitize(frag.Content)
		safe = append(safe, frag)
	}
	return p.sanitize(buildPromotePrompt(safe, p.sanitize(existingRecent)))
}

func (p *memoryProtection) checkImage(img memorydb.ImageRecord) error {
	for _, field := range []string{img.Prompt, img.OutputPath, img.Provider, img.SessionID} {
		if err := p.check(field); err != nil {
			return err
		}
	}
	return nil
}

// Check both the model's original fields and the exact normalized values used
// by persistence. No trim or other content transformation follows this gate.
func (p *memoryProtection) insight(item insightExtractionItem) (insightExtractionItem, error) {
	for _, field := range []string{item.Rule, item.Compact, item.Trigger, item.Category} {
		if err := p.check(field); err != nil {
			return item, err
		}
	}
	item.Rule = strings.TrimSpace(item.Rule)
	item.Compact = strings.TrimSpace(item.Compact)
	item.Trigger = strings.TrimSpace(item.Trigger)
	item.Category = strings.TrimSpace(item.Category)
	for _, field := range []string{item.Rule, item.Compact, item.Trigger, item.Category} {
		if err := p.check(field); err != nil {
			return item, err
		}
	}
	return item, nil
}
