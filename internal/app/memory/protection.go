package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"strings"
	"unicode/utf8"

	"github.com/term-llm/term-llm/internal/config"
	memorydb "github.com/term-llm/term-llm/internal/memory"
	"github.com/term-llm/term-llm/internal/secrettext"
	"github.com/term-llm/term-llm/internal/session"
)

var errMemorySafety = errors.New("memory content rejected by native safety validation")

// memoryDetectorRejection carries only stable detector IDs, never source text or
// owner pattern literals. Keep it typed so batch diagnostics need not parse errors.
type memoryDetectorRejection struct{ detectors []string }

func (e *memoryDetectorRejection) Error() string {
	return fmt.Sprintf("%v: %s; use credential locations instead of values, or correct owner policy", errMemorySafety, strings.Join(e.detectors, ", "))
}
func (e *memoryDetectorRejection) Unwrap() error { return errMemorySafety }

// memoryProtection is an immutable snapshot of trusted owner configuration.
type memoryProtection struct {
	redactor     *secrettext.Redactor
	excludePaths []string
}

var defaultProtection = &memoryProtection{redactor: secrettext.Default}

func protectionOrDefault(p *memoryProtection) *memoryProtection {
	if p == nil {
		return defaultProtection
	}
	return p
}
func newMemoryProtection(cfg config.MemoryConfig) (*memoryProtection, error) {
	r, err := secrettext.New(cfg.Secrets.Disable, cfg.Secrets.Patterns)
	if err != nil {
		return nil, err
	}
	for i, glob := range cfg.ExcludePaths {
		if _, err := doublestar.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("invalid memory.exclude_paths entry %d", i+1)
		}
	}
	return &memoryProtection{redactor: r, excludePaths: append([]string(nil), cfg.ExcludePaths...)}, nil
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
	out, _ := p.redactor.Redact(strings.ToValidUTF8(text, ""))
	return out
}
func (p *memoryProtection) check(text string) error {
	if !utf8.ValidString(text) {
		return errors.New("memory content is not valid UTF-8")
	}
	_, hits := p.redactor.Redact(text)
	if len(hits) > 0 {
		return &memoryDetectorRejection{detectors: hits}
	}
	return nil
}
func (p *memoryProtection) excluded(fragPath string) bool {
	fragPath = path.Clean(strings.TrimSpace(strings.ReplaceAll(fragPath, "\\", "/")))
	for _, glob := range p.excludePaths {
		matched, _ := doublestar.Match(glob, fragPath)
		if matched {
			return true
		}
	}
	return false
}

// Only secret checks belong here. Path/empty/size errors remain ordinary,
// actionable model tool errors. Check original and exact persisted forms.
func (p *memoryProtection) fragment(fragPath, content string) error {
	normalized, err := validateFragmentPath(fragPath)
	if err != nil {
		return err
	}
	for _, field := range []string{fragPath, normalized, content, strings.TrimSpace(content)} {
		if err := p.check(field); err != nil {
			return err
		}
	}
	return nil
}

var errRecentSize = errors.New("recent summary exceeds byte budget after compaction")

func (p *memoryProtection) recent(content string, maxBytes int) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("recent summary is empty")
	}
	if maxBytes <= 0 || len(content) > maxBytes {
		return errRecentSize
	}
	return p.check(content)
}

// Unchecked provider output may contain secrets even when input was sanitized.
// Keep useful diagnostics, redacting by the same owner policy; never retain an
// unsafe error body in an unwrap chain. Cancellation identity is preserved.
func (p *memoryProtection) providerError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s: %s", operation, p.sanitize(err.Error()))
}

// Raw diagnostics are an explicit owner optout only. Detectors-enabled runs
// retain sanitized errors but suppress unvalidated model response logging.
func (p *memoryProtection) prepareDebug(eng *engine.Engine, raw bool) bool {
	if !p.redactor.Enabled() {
		return raw
	}
	eng.SetDebugLogger(nil)
	if raw {
		fmt.Fprintln(os.Stderr, "memory: raw model debug suppressed while secret detectors are enabled; owner memory.secrets.disable: [all] with no patterns permits raw diagnostics")
	}
	return false
}

// Taxonomy reads only paths. Sanitize each entire path before directory names
// or budget selection can split a recognizable credential across boundaries.
func (p *memoryProtection) taxonomy(fragments []memorydb.Fragment, maxTokens int) string {
	safe := make([]memorydb.Fragment, 0, len(fragments))
	for _, frag := range fragments {
		if p.excluded(frag.Path) {
			continue
		}
		frag.Path = p.sanitize(frag.Path)
		safe = append(safe, frag)
	}
	return buildTaxonomyMap(safe, maxTokens)
}

// candidate sanitizes provider-visible metadata without changing persistence IDs.
func (p *memoryProtection) candidate(c memoryMineCandidate) memoryMineCandidate {
	safeSession := *c.Session
	safeSession.ID = p.sanitize(safeSession.ID)
	c.Session = &safeSession
	c.Agent = p.sanitize(c.Agent)
	return c
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
		if p.excluded(frag.Path) {
			continue
		}
		frag.Path = p.sanitize(frag.Path)
		frag.Content = p.sanitize(frag.Content)
		safe = append(safe, frag)
	}
	return p.sanitize(buildPromotePrompt(safe, p.sanitize(existingRecent)))
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

func (p *memoryProtection) included(fragments []memorydb.Fragment) []memorydb.Fragment {
	out := make([]memorydb.Fragment, 0, len(fragments))
	for _, frag := range fragments {
		if !p.excluded(frag.Path) {
			out = append(out, frag)
		}
	}
	return out
}
