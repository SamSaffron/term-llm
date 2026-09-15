package live

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

const delegationFieldMaxBytes = 4 * 1024

// DefaultInstructions is the default prompt for term-llm's conversational
// voice surface.
const DefaultInstructions = `You are term-llm's conversational voice surface. Everything you say is spoken aloud.

How you work:
- For requests involving code, files, the shell, tools, or project-specific investigation, send the work to the execution backend and speak a concise summary of its result. The backend uses the normal tools and approval flow.
- When the user corrects or adds guidance to work already running, hand that request to the execution backend immediately. The host routes it as steering of the current task; do not wait for the task to finish. An acknowledgement that guidance was queued is not the task's final result.
- Treat authoritative host context appended to these instructions as current fact. Answer known, self-contained questions about the host, model, voice, and controls directly from that context.
- Backend output is primarily visual and may include complete code, diffs, file trees, tables, paths, and command output. Summarize the useful outcome for the ear instead of reading long visual content aloud.
- Preserve all safety and approval checks. If work needs approval or information only the user has, explain that plainly and ask a short question.

Message prefixes you may see:
- "[USER] ..." is text the user typed instead of speaking. Treat it exactly like speech.
- "[BACKEND] ..." is execution output to summarize conversationally.

Speak briefly and naturally. Do not use markdown, headings, or long lists in spoken responses.`

// ExecutionInstructions describes how the ordinary agent should handle work
// originating from a live voice turn.
const ExecutionInstructions = `The user's request may be a speech transcript with transcription errors; infer the likely intent when reasonable. Use the normal tools and approval flow. Produce complete, useful visual output, including code and diffs when relevant; the voice model will summarize the result aloud.`

// DelegationPrompt returns only the bounded XML wrapper used to pass structured
// live input to an executing agent. Input keeps its head and transcript context
// keeps its tail; each escaped field is at most 4 KiB.
func DelegationPrompt(input, transcriptDelta string) string {
	var b strings.Builder
	b.WriteString("<realtime_delegation>\n  <input>")
	b.WriteString(escapedHead(strings.TrimSpace(input), delegationFieldMaxBytes))
	b.WriteString("</input>\n")
	if delta := strings.TrimSpace(transcriptDelta); delta != "" {
		b.WriteString("  <transcript_delta>")
		b.WriteString(escapedTail(delta, delegationFieldMaxBytes))
		b.WriteString("</transcript_delta>\n")
	}
	b.WriteString("</realtime_delegation>")
	return b.String()
}

func escapedHead(text string, limit int) string {
	var b strings.Builder
	for _, r := range text {
		escaped := html.EscapeString(string(r))
		if b.Len()+len(escaped) > limit {
			break
		}
		b.WriteString(escaped)
	}
	return b.String()
}

func escapedTail(text string, limit int) string {
	start := len(text)
	used := 0
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		escapedBytes := len(html.EscapeString(string(r)))
		if used+escapedBytes > limit {
			break
		}
		used += escapedBytes
		start -= size
	}
	return escapedHead(text[start:], limit)
}

func resolvedInstructions(configured string, opts SessionOptions) string {
	instructions := strings.TrimSpace(opts.Instructions)
	if instructions == "" {
		instructions = strings.TrimSpace(configured)
	}
	if instructions == "" {
		instructions = DefaultInstructions
	}
	if context := strings.TrimSpace(opts.Context); context != "" {
		instructions += "\n\n" + context
	}
	return instructions
}

// userTextPrefix marks text the user typed rather than spoke.
const userTextPrefix = "[USER] "

// prefixUserText tags typed user input for the voice model.
func prefixUserText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	return userTextPrefix + trimmed
}

// delegationFailureText is what the voice model hears when a delegated turn
// could not complete, so it always has something to say.
func delegationFailureText(err error) string {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "unknown error"
	}
	return fmt.Sprintf("The task failed: %s", message)
}
