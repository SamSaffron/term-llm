package clibin

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// agyTurnMode describes where agy gets the prompt for one process.
type agyTurnMode string

const (
	// agyTurnInitial starts a new native conversation with the complete prepared
	// request and no --conversation selector.
	agyTurnInitial agyTurnMode = "initial"
	// agyTurnNative resumes the stored conversation with only new logical
	// messages plus current turn context.
	agyTurnNative agyTurnMode = "native_resume"
	// agyTurnFallback replays the complete prepared request because the stored
	// native continuation no longer matches the stable transcript.
	agyTurnFallback agyTurnMode = "transcript_fallback"
)

// Bounded reason codes for structured diagnostics. They never contain prompt
// text, credentials, or raw environment values.
const (
	agyReasonEphemeral            = "ephemeral_request"
	agyReasonNoNativeState        = "no_native_state"
	agyReasonVerifiedPrefix       = "verified_prefix"
	agyReasonTranscriptTruncated  = "transcript_truncated"
	agyReasonTranscriptMismatch   = "transcript_mismatch"
	agyReasonTurnFailedAfterStart = "turn_failed_after_start"
	agyReasonConversationMismatch = "conversation_mismatch"
	agyReasonIsolationUnverified  = "isolation_unverified"

	agyStateReasonMissingHome     = "missing_home"
	agyStateReasonMissingDatabase = "missing_conversation_db"
	agyStateReasonOldFormat       = "incompatible_state_format"
)

// agyTurnPlan is the provider-safe decision for one agy process. Messages is
// exactly what this process receives: the full prepared request on an initial
// start or transcript fallback, and only new logical messages plus current turn
// context on native resume. SentCount and TranscriptHash describe the stable
// conversation for the next request, never the transiently augmented payload.
type agyTurnPlan struct {
	Mode           agyTurnMode
	Reason         string
	Messages       []core.Message
	Prompt         string
	PromptBytes    int
	ResumeID       string
	SentCount      int
	TranscriptHash string
}

// agyStableConversation returns the provider-safe durable conversation and the
// request-only context generated for this turn. Direct provider requests that
// carry no metadata have no generated context.
func agyStableConversation(req core.Request) ([]core.Message, []core.Message) {
	if conversation := req.ProviderConversation; conversation != nil {
		return conversation.Messages, conversation.TurnContext
	}
	return req.Messages, nil
}

// planTurn selects native resume or a fresh/fallback replay. The caller holds
// stateMu, so a detected divergence can reset continuation state atomically.
func (p *AgyBinProvider) planTurn(req core.Request, workspaceDir string) (agyTurnPlan, error) {
	stable, turnContext := agyStableConversation(req)
	plan := agyTurnPlan{Mode: agyTurnInitial, Reason: agyReasonNoNativeState, Messages: req.Messages}
	switch {
	case req.Ephemeral:
		plan.Reason = agyReasonEphemeral
	case p.conversationID == "":
		// Nothing to resume; replay the complete prepared request but keep a
		// retained import/invalidation reason so the fallback stays explainable.
		if p.fallbackReason != "" {
			plan.Mode, plan.Reason = agyTurnFallback, p.fallbackReason
		}
	default:
		decision, err := p.nativeResumeDecisionLocked(stable, turnContext)
		if err != nil {
			return agyTurnPlan{}, err
		}
		if decision.Resume {
			plan.Mode, plan.Reason, plan.Messages, plan.ResumeID = agyTurnNative, agyReasonVerifiedPrefix, decision.Messages, p.conversationID
		} else {
			plan.Mode, plan.Reason = agyTurnFallback, decision.Reason
		}
	}
	plan.SentCount = len(stable)
	hash, err := agyTranscriptHash(stable)
	if err != nil {
		return agyTurnPlan{}, err
	}
	plan.TranscriptHash = hash
	plan.Prompt = buildAgyPrompt(plan.Messages, workspaceDir)
	if strings.TrimSpace(plan.Prompt) == "" {
		return agyTurnPlan{}, errors.New("build agy-bin prompt: no user-visible content")
	}
	plan.PromptBytes = len(plan.Prompt)
	logAgyTurnPlan(req, plan)
	return plan, nil
}

// agyResumeMessages drops the leading assistant/tool replay agy already owns.
// It deliberately keeps a system instruction appended after the boundary: unlike
// Claude/Grok replay, a changed system prompt is genuine provider-visible
// content that must reach the model on resume.
func agyResumeMessages(messages []core.Message) []core.Message {
	for i, message := range messages {
		switch message.Role {
		case core.RoleAssistant, core.RoleTool, core.RoleEvent:
			continue
		default:
			return messages[i:]
		}
	}
	return nil
}

type agyResumeDecision struct {
	Resume   bool
	Reason   string
	Messages []core.Message
}

// nativeResumeDecisionLocked validates the stored boundary against the stable
// conversation, not the transiently augmented request. A different roster or
// other request-only context must not invalidate native continuation, while a
// genuine edit to system/developer/user/assistant/tool content must.
func (p *AgyBinProvider) nativeResumeDecisionLocked(stable, turnContext []core.Message) (agyResumeDecision, error) {
	if p.messagesSent > len(stable) {
		runtimeoutput.Warn("agy-bin resume message boundary exceeded request transcript; resetting conversation state",
			"messages_sent", p.messagesSent, "request_messages", len(stable))
		p.dropResumeLocked(agyReasonTranscriptTruncated)
		return agyResumeDecision{Reason: agyReasonTranscriptTruncated}, nil
	}
	prefixHash, err := agyTranscriptHash(stable[:p.messagesSent])
	if err != nil {
		return agyResumeDecision{}, err
	}
	if p.transcriptHash == "" || prefixHash != p.transcriptHash {
		runtimeoutput.Warn("agy-bin request transcript diverged from resumed conversation; resetting conversation state",
			"messages_sent", p.messagesSent)
		p.dropResumeLocked(agyReasonTranscriptMismatch)
		return agyResumeDecision{Reason: agyReasonTranscriptMismatch}, nil
	}
	if p.messagesSent == len(stable) {
		return agyResumeDecision{}, errors.New("agy-bin resumed session has no new messages to send")
	}
	// agy already owns the leading generated assistant/tool replay. A suffix of
	// only native-owned output has no new logical message: current context alone
	// (a regenerated roster) must not start a model turn. Only then apply turn
	// context, which is excluded from historical identity.
	newLogical := agyResumeMessages(stable[p.messagesSent:])
	if len(newLogical) == 0 {
		return agyResumeDecision{}, errors.New("agy-bin resumed session has no new messages to send")
	}
	messages := core.WithTurnContext(newLogical, turnContext)
	if len(messages) == 0 {
		return agyResumeDecision{}, errors.New("agy-bin resumed session has no new messages to send")
	}
	return agyResumeDecision{Resume: true, Reason: agyReasonVerifiedPrefix, Messages: messages}, nil
}

// invalidateResumeAfterFailedTurn keeps a restored cursor only when the failure
// happened before agy could accept any input. Once the process started, the
// submitted delta may already be committed to agy's durable conversation, so
// resubmitting it could duplicate a user turn or a tool side effect. The cursor
// is dropped for the next explicit continuation to fall back to a full replay;
// nothing is retried or replayed automatically.
func (p *AgyBinProvider) invalidateResumeAfterFailedTurn(req core.Request, plan agyTurnPlan, err error) {
	if req.Ephemeral || plan.ResumeID == "" {
		return
	}
	reason := ""
	switch {
	case errors.Is(err, errAgyConversationMismatch):
		reason = agyReasonConversationMismatch
	case agyFailureAfterStart(err):
		reason = agyReasonTurnFailedAfterStart
	default:
		runtimeoutput.Debug("agy-bin retained resumed conversation after pre-exec failure",
			"session_id", req.SessionID, "conversation_id", plan.ResumeID, "stable_messages", plan.SentCount)
		if req.Debug || req.DebugRaw {
			runtimeoutput.Printf("[agy-bin] resume retained after pre-exec failure session_id=%q native_conversation=%q stable_messages=%d\n",
				req.SessionID, plan.ResumeID, plan.SentCount)
		}
		return
	}
	p.dropResumeLocked(reason)
	runtimeoutput.Warn("agy-bin dropped resumed conversation after failed turn",
		"reason", reason,
		"session_id", req.SessionID,
		"conversation_id", plan.ResumeID,
		"stable_messages", plan.SentCount)
	if req.Debug || req.DebugRaw {
		runtimeoutput.Printf("[agy-bin] resume dropped reason=%s session_id=%q native_conversation=%q stable_messages=%d next_turn=transcript_fallback\n",
			reason, req.SessionID, plan.ResumeID, plan.SentCount)
	}
}

// requireFilteredGeneration keeps fail-closed proxy verification: an unverified
// local generation must not be trusted as a resumed native conversation.
func (p *AgyBinProvider) requireFilteredGeneration(resumeID string) error {
	if p.isolation.FilteredGenerations() > 0 {
		return nil
	}
	if strings.TrimSpace(resumeID) != "" {
		// agy may already have committed the submitted delta to its durable DB.
		// Reset the local boundary so a retry starts a fresh conversation with the
		// complete term-llm transcript instead of duplicating that delta on resume.
		p.dropResumeLocked(agyReasonIsolationUnverified)
		runtimeoutput.Warn("agy-bin dropped resumed conversation after unverified turn",
			"reason", agyReasonIsolationUnverified, "conversation_id", resumeID)
	}
	return errors.New("agy did not route generation through the term-llm tool-filter proxy; refusing unverified turn")
}

func logAgyTurnPlan(req core.Request, plan agyTurnPlan) {
	runtimeoutput.Debug("agy-bin turn",
		"mode", string(plan.Mode),
		"reason", plan.Reason,
		"session_id", req.SessionID,
		"conversation_id", plan.ResumeID,
		"stable_messages", plan.SentCount,
		"payload_messages", len(plan.Messages),
		"prompt_bytes", plan.PromptBytes)
	if !req.Debug && !req.DebugRaw {
		return
	}
	// runtimeoutput.Debug is suppressed without an interactive sink, so an
	// explicit --debug/--debug-raw turn also prints the bounded decision summary.
	// Only counts, mode/reason codes and opaque IDs appear; never prompt text.
	runtimeoutput.Printf("[agy-bin] turn mode=%s reason=%s session_id=%q native_conversation=%q stable_messages=%d payload_messages=%d prompt_bytes=%d\n",
		plan.Mode, plan.Reason, req.SessionID, plan.ResumeID, plan.SentCount, len(plan.Messages), plan.PromptBytes)
}

// agyTranscriptHash fingerprints the provider-visible conversation. Only the
// semantic role and content agy actually renders participate: display
// attribution, response bookkeeping, cache hints, timestamps, reasoning
// payloads and provider replay metadata are not part of the prompt and must not
// invalidate a native continuation. Genuine system, developer, user, assistant,
// tool-call and tool-result changes still change the hash.
func agyTranscriptHash(messages []core.Message) (string, error) {
	data, err := json.Marshal(agySemanticTranscript(messages))
	if err != nil {
		return "", fmt.Errorf("hash agy-bin transcript: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// agySemanticTranscript preserves the historical nil/empty JSON encoding so an
// empty transcript fingerprints exactly as it did before state format 2.
func agySemanticTranscript(messages []core.Message) []agySemanticMessage {
	if messages == nil {
		return nil
	}
	out := make([]agySemanticMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role == core.RoleEvent {
			// Durable timeline markers are never provider context.
			continue
		}
		out = append(out, agySemanticMessage{Role: message.Role, Parts: agySemanticParts(message.Parts)})
	}
	return out
}

type agySemanticMessage struct {
	Role  core.Role         `json:"role"`
	Parts []agySemanticPart `json:"parts,omitempty"`
}

type agySemanticPart struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
}

// agySemanticParts keeps exactly the parts renderGrokConversationParts turns
// into prompt text for agy. Markers that only affect persistence/display are
// dropped, and images contribute only the "[image]" placeholder that is sent.
func agySemanticParts(parts []core.Part) []agySemanticPart {
	var out []agySemanticPart
	for _, part := range parts {
		switch part.Type {
		case core.PartText, core.PartFile:
			if part.Text != "" {
				out = append(out, agySemanticPart{Kind: "text", Text: part.Text})
			}
		case core.PartImage:
			out = append(out, agySemanticPart{Kind: "image"})
		case core.PartToolCall:
			if part.ToolCall != nil {
				out = append(out, agySemanticPart{Kind: "tool_call", Name: part.ToolCall.Name})
			}
		case core.PartToolResult:
			if part.ToolResult != nil {
				out = append(out, agySemanticPart{Kind: "tool_result", Name: part.ToolResult.Name, Text: core.ToolResultTextContent(part.ToolResult)})
			}
		}
	}
	return out
}
