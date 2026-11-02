package clibin

import (
	"errors"
	"strings"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// Claude Code streams thinking text only when asked for summaries and allowed
// to send its first-party API betas; otherwise each thinking block carries
// just an encrypted signature. claude-bin asks for summaries by default, and
// providers.claude-bin.thinking_summaries: false keeps the betas switched off.
const (
	claudeThinkingDisplayFlag  = "--thinking-display"
	claudeExperimentalBetasEnv = "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"
)

// claudeThinkingDisplayUnsupported records that the installed Claude Code
// rejected --thinking-display, which its --help does not list. Later runs
// skip the flag and keep the betas off instead of failing every turn.
var claudeThinkingDisplayUnsupported atomic.Bool

// SetThinkingSummaries controls whether Claude's thinking summaries are
// requested and streamed as reasoning (enabled by default).
func (p *ClaudeBinProvider) SetThinkingSummaries(enable bool) {
	p.thinkingSummaries = enable
}

func (p *ClaudeBinProvider) requestThinkingSummaries() bool {
	return p.thinkingSummaries && !claudeThinkingDisplayUnsupported.Load()
}

// forcesIsolationEnv reports whether this run applies the forced value of a
// forcedClaudeCodeIsolationEnv key. Summaries need the betas left on.
func (p *ClaudeBinProvider) forcesIsolationEnv(key string) bool {
	return key != claudeExperimentalBetasEnv || !p.requestThinkingSummaries()
}

// claudeRejectedThinkingDisplay reports a Claude Code start-up failure caused
// by --thinking-display: an unknown option, or a value it does not accept.
func claudeRejectedThinkingDisplay(err error) bool {
	var cmdErr *CLICommandError
	return errors.As(err, &cmdErr) && strings.Contains(cmdErr.StderrTail, "'"+claudeThinkingDisplayFlag)
}

func withoutThinkingDisplay(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == claudeThinkingDisplayFlag {
			i++ // and its value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// claudeThinkingStream keeps the thinking blocks of one Claude Code run in
// separate paragraphs: Claude Code runs a whole tool loop as one turn, and the
// engine joins a turn's reasoning into a single thought.
type claudeThinkingStream struct {
	tail     string // last two bytes of the thinking forwarded so far
	newBlock bool   // the next thinking text starts another block
}

// handleClaudeThinking forwards Claude's thinking summaries as provider
// thinking. Signatures stay with Claude Code, which owns the session replay.
func handleClaudeThinking(event claudeStreamEventBody, state *claudeThinkingStream, send eventSender) error {
	if event.Type == "content_block_start" && event.ContentBlock.Type == "thinking" && state != nil {
		state.newBlock = state.tail != ""
		return nil
	}
	if event.Type != "content_block_delta" || event.Delta.Type != "thinking_delta" || event.Delta.Thinking == "" {
		return nil
	}
	text := event.Delta.Thinking
	if state != nil {
		if state.newBlock && !strings.HasSuffix(state.tail, "\n\n") && !strings.HasPrefix(text, "\n") {
			text = "\n\n" + text
		}
		state.newBlock = false
		state.tail += text
		if len(state.tail) > 2 {
			state.tail = state.tail[len(state.tail)-2:]
		}
	}
	return send.Send(core.Event{Type: core.EventReasoningDelta, Text: text, ReasoningKind: core.ReasoningKindRaw})
}
