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
//
// The model does not classify anything. It delegates every request it cannot answer
// from host context, in the user's own words, and the host decides what happens
// next. This prompt is static so providers can cache it, and it deliberately claims
// nothing about what that decision is: with the control plane off — the default —
// a request about another conversation or about the voice simply becomes work in the
// bound session, so the claim that the host routes those requests is only true when
// a host has appended ControlPlaneContext through SessionOptions.Context.
//
// That division is the fix for two production failures in one hour, both caused by
// asking the voice model to recognise a session control. First the model was told to
// mark such a request with a "control:" sentinel and paraphrased it into an ordinary
// delegation instead ("Switch to the attachment icons one" named no session, so no
// rule matched). Then a host-side word list tried to catch the paraphrase and missed
// a verb form ("Do you mind switching back to ... where I was managing session with
// voice"). Every rule of that shape is a word list, and a word list has a bottom.
// The model is therefore no longer asked to know what a session control is; it is
// asked to pass the request along, and the host reads it.
const DefaultInstructions = `You are term-llm's conversational voice surface. Everything you say is spoken aloud.

How you work:
- For requests involving code, files, the shell, tools, or project-specific investigation, put the work in a delegation to the execution backend and speak a concise summary of the result; the backend uses the normal tools and approval flow. You have exactly one channel to that backend — a delegation request — and its input is the whole of what the backend receives. Requests about the user's other conversations, moving this call to another session, or inspecting or changing the voice travel that same channel: pass them along in the user's own words, with nothing to prepend.
- When the user corrects or adds guidance to work already running, hand that request to the execution backend immediately. The host routes it as steering of the current task; do not wait for the task to finish. An acknowledgement that guidance was queued is not the task's final result.
- Treat authoritative host context appended to these instructions as current fact. Answer known, self-contained questions about the host, model, voice, and controls directly from that context.
- Backend output is primarily visual and may include complete code, diffs, file trees, tables, paths, and command output. Summarize the useful outcome for the ear instead of reading long visual content aloud.
- Preserve all safety and approval checks. If work needs approval or information only the user has, explain that plainly and ask a short question.

Message prefixes you may see:
- "[USER] ..." is text the user typed instead of speaking. Treat it exactly like speech.
- "[BACKEND] ..." is execution output to summarize conversationally.

Speak briefly and naturally. Do not use markdown, headings, or long lists in spoken responses.`

// ControlPlaneContext is the host fact a voice conversation may only be told when
// the control plane is switched on: that a host-side routing model reads each
// request before it becomes work.
//
// It is appended through SessionOptions.Context rather than baked into
// DefaultInstructions, because with the flag off the sentence is false — those
// requests are ordinary work in the bound session, exactly as they were before the
// lane existed — and a static prompt cannot be both cacheable and conditional. A
// host that leaves the flag off therefore promises nothing it will not do.
const ControlPlaneContext = "Requests about the user's other conversations, moving this call to another session, or inspecting or changing this call's voice are read by a host-side routing model before they can become work. It either handles them itself and answers, or hands the request to the workspace agent: pass such a request along in the user's own words, with nothing to prepend, and the host decides what it needs."

// ExecutionInstructions describes how the ordinary agent should handle work
// originating from a live voice turn.
//
// It deliberately says nothing about session controls. The host reads every
// delegation before this agent can see it, and a request about the call is answered
// there, so a delegated turn that is still asked to list or switch sessions must fail
// loudly — the spoken answer names the limit and the user repeats the request to the
// voice model — instead of quietly doing it as a chat turn and writing the exchange
// into the session's transcript.
const ExecutionInstructions = `The user's request may be a speech transcript with transcription errors; infer the likely intent when reasonable. Use the normal tools and approval flow. Produce complete, useful visual output, including code and diffs when relevant; the voice model will summarize the result aloud. Session controls — listing or searching conversations, starting a new conversation, moving this voice call to another session, and inspecting or changing its voice — are handled by the voice model and the host, not by you: if a spoken request asks for one of them, say so plainly and let the user repeat it to the voice model rather than guessing at a session.`

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
