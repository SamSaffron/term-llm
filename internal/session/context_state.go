package session

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// HasCompactionBoundary reports whether a session has an explicit persisted
// compaction boundary. Checking the count (or a positive sequence from older
// persisted sessions) avoids treating a zero-value Session as compacted at
// sequence 0.
func HasCompactionBoundary(sess *Session) bool {
	return sess != nil && sess.CompactionSeq >= 0 && (sess.CompactionCount > 0 || sess.CompactionSeq > 0)
}

// LoadActiveMessages loads the messages that should be sent as active LLM
// context for a session. If the session has been compacted, older scrollback
// rows are intentionally skipped and callers receive only rows at/after the
// compaction boundary.
func LoadActiveMessages(ctx context.Context, store Store, sess *Session) ([]Message, error) {
	if store == nil || sess == nil {
		return nil, nil
	}
	if HasCompactionBoundary(sess) {
		messages, err := store.GetMessagesFrom(ctx, sess.ID, sess.CompactionSeq, 0)
		if err != nil || len(messages) > 0 {
			return messages, err
		}
		// A stale boundary should not silently erase active context. Fall back to
		// the full history and best-effort clear the boundary so later appends do
		// not make the stale sequence become active again.
		clearCompactionBoundary(ctx, store, sess)
		return store.GetMessages(ctx, sess.ID, 0, 0)
	}
	return store.GetMessages(ctx, sess.ID, 0, 0)
}

func clearCompactionBoundary(ctx context.Context, store Store, sess *Session) {
	if store == nil || sess == nil || !HasCompactionBoundary(sess) {
		return
	}
	sess.CompactionSeq = -1
	sess.CompactionCount = 0
	if clearer, ok := store.(interface {
		ClearCompactionBoundary(context.Context, string) error
	}); ok {
		_ = clearer.ClearCompactionBoundary(ctx, sess.ID)
	}
}

func persistCompactionTailHints(ctx context.Context, store Store, sess *Session, messageIDs []int64) {
	if store == nil || sess == nil || len(messageIDs) == 0 {
		return
	}
	if persister, ok := store.(interface {
		PersistCompactionTailHints(context.Context, string, []int64) error
	}); ok {
		_ = persister.PersistCompactionTailHints(ctx, sess.ID, messageIDs)
	}
}

// LoadScrollbackWithBoundary loads all persisted messages for display while
// also returning the index where active LLM context begins.
func LoadScrollbackWithBoundary(ctx context.Context, store Store, sess *Session) ([]Message, int, error) {
	if store == nil || sess == nil {
		return nil, 0, nil
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return nil, 0, err
	}
	if !HasCompactionBoundary(sess) {
		return messages, 0, nil
	}
	for i, msg := range messages {
		if msg.Sequence >= sess.CompactionSeq {
			var persistedTailIDs []int64
			messages, persistedTailIDs = markCompactionDisplayTailsForPersistence(messages)
			// Legacy compacted sessions may predate persisted display-tail hints.
			// Backfill the hints best-effort so future loads skip duplicate detection.
			persistCompactionTailHints(ctx, store, sess, persistedTailIDs)
			return messages, i, nil
		}
	}
	// If the persisted boundary is stale (for example after an older replace), do
	// not hide all history from the model/UI. Treat the full scrollback as active
	// and best-effort clear the persisted boundary.
	clearCompactionBoundary(ctx, store, sess)
	return messages, 0, nil
}

// LoadInitialScrollbackWithBoundary loads the minimum scrollback needed for the
// first resumed paint, plus the index where active LLM context begins. For
// compacted sessions it starts at the persisted compaction boundary and reports
// the older prefix as deferred so callers can lazy-load that historical
// scrollback on demand instead of blocking startup on a full transcript read.
func LoadInitialScrollbackWithBoundary(ctx context.Context, store Store, sess *Session) ([]Message, int, int, error) {
	if store == nil || sess == nil {
		return nil, 0, 0, nil
	}
	if !HasCompactionBoundary(sess) {
		messages, idx, err := LoadScrollbackWithBoundary(ctx, store, sess)
		return messages, idx, 0, err
	}

	messages, err := store.GetMessagesFrom(ctx, sess.ID, sess.CompactionSeq, 0)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(messages) == 0 {
		clearCompactionBoundary(ctx, store, sess)
		messages, idx, err := LoadScrollbackWithBoundary(ctx, store, sess)
		return messages, idx, 0, err
	}

	var persistedTailIDs []int64
	messages, persistedTailIDs = markCompactionDisplayTailsForPersistence(messages)
	persistCompactionTailHints(ctx, store, sess, persistedTailIDs)
	return messages, 0, sess.CompactionSeq, nil
}

// ApplyCompaction persists a compaction result, appends the compacted rows to
// the caller's scrollback snapshot, returns the new active start index, refreshes
// session metadata when possible, and best-effort clears any persisted context
// estimate baseline so future turns don't seed context management with
// pre-compaction token counts. The full argument is the caller's scrollback
// snapshot and may be nil for non-UI owners that only need persistence.
func ApplyCompaction(ctx context.Context, store Store, sess *Session, full []Message, result *llm.CompactionResult) ([]Message, int, *Session, error) {
	activeStart := len(full)
	if result == nil {
		return full, activeStart, sess, nil
	}

	newSessionMsgs := make([]Message, 0, len(result.NewMessages))
	sessionID := ""
	if sess != nil {
		sessionID = sess.ID
	}
	for _, msg := range dropLeadingCompactionSystemMessages(result.NewMessages) {
		newSessionMsgs = append(newSessionMsgs, *NewMessage(sessionID, msg, -1))
	}
	markNewCompactionTailRows(newSessionMsgs)

	refreshed := sess
	if store != nil && sess != nil {
		if err := store.CompactMessages(ctx, sess.ID, newSessionMsgs); err != nil {
			return full, activeStart, refreshed, err
		}
		// Once CompactMessages succeeds, the persistence boundary has moved. Treat
		// the context-estimate reset and metadata refresh as best-effort so callers
		// still update their active in-memory context and don't diverge from the DB.
		bestEffortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = ResetContextEstimate(bestEffortCtx, store, sess)
		if got, err := store.Get(bestEffortCtx, sess.ID); err == nil && got != nil {
			refreshed = got
		}
	}

	updated := append(append([]Message(nil), full...), newSessionMsgs...)
	return updated, activeStart, refreshed, nil
}

func dropLeadingCompactionSystemMessages(messages []llm.Message) []llm.Message {
	start := 0
	for start < len(messages) && messages[start].Role == llm.RoleSystem {
		start++
	}
	// A compaction result should always include a summary row after any system
	// prompt. If it somehow does not, preserve the original result instead of
	// creating an empty persisted boundary.
	if start >= len(messages) {
		return messages
	}
	return messages[start:]
}

func markNewCompactionTailRows(messages []Message) {
	summaryIdx := -1
	for i := range messages {
		if isInternalCompactionSummaryMessage(messages[i]) {
			summaryIdx = i
			break
		}
	}
	if summaryIdx < 0 {
		return
	}
	for i := summaryIdx + 1; i < len(messages); i++ {
		messages[i].CompactionTail = true
	}
}

func markCompactionDisplayTails(messages []Message) []Message {
	marked, _ := markCompactionDisplayTailsForPersistence(messages)
	return marked
}

func markCompactionDisplayTailsForPersistence(messages []Message) ([]Message, []int64) {
	if len(messages) == 0 {
		return messages, nil
	}
	out := append([]Message(nil), messages...)
	var fingerprints []string
	var persistedTailIDs []int64
	for i := 0; i < len(out); i++ {
		if !isInternalCompactionSummaryMessage(out[i]) {
			continue
		}
		ackWasTail := i+1 < len(out) && out[i+1].CompactionTail
		if persistedCompactionTailAlreadyMarked(out, i) {
			if !ackWasTail && i+1 < len(out) && isSyntheticCompactionAckMessage(out[i+1]) && out[i+1].CompactionTail && out[i+1].ID != 0 {
				persistedTailIDs = append(persistedTailIDs, out[i+1].ID)
			}
			continue
		}
		if fingerprints == nil {
			fingerprints = make([]string, len(out))
			for j := range out {
				fingerprints[j] = messageDisplayFingerprint(out[j])
			}
		}
		start, dupLen := compactionDuplicateTailRange(out, fingerprints, i)
		if dupLen > 0 {
			// The rows from just after the summary through the retained raw suffix
			// are compacted active context. They remain in m.messages / active loads,
			// but are already visible in the pre-compaction transcript, so display
			// renderers should suppress them instead of echoing them a second time.
			for j := i + 1; j < start+dupLen && j < len(out); j++ {
				if out[j].CompactionTail {
					continue
				}
				out[j].CompactionTail = true
				if out[j].ID != 0 {
					persistedTailIDs = append(persistedTailIDs, out[j].ID)
				}
			}
			continue
		}
		if i+1 < len(out) && isSyntheticCompactionAckMessage(out[i+1]) && !out[i+1].CompactionTail {
			out[i+1].CompactionTail = true
			if out[i+1].ID != 0 {
				persistedTailIDs = append(persistedTailIDs, out[i+1].ID)
			}
		}
	}
	return out, persistedTailIDs
}

func persistedCompactionTailAlreadyMarked(messages []Message, summaryIdx int) bool {
	if summaryIdx+1 >= len(messages) {
		return false
	}
	if messages[summaryIdx+1].CompactionTail {
		return true
	}
	if isSyntheticCompactionAckMessage(messages[summaryIdx+1]) && summaryIdx+2 < len(messages) && messages[summaryIdx+2].CompactionTail {
		messages[summaryIdx+1].CompactionTail = true
		return true
	}
	return false
}

func compactionDuplicateTailRange(messages []Message, fingerprints []string, summaryIdx int) (int, int) {
	if summaryIdx <= 0 || summaryIdx+1 >= len(messages) {
		return -1, 0
	}

	candidates := []int{summaryIdx + 1}
	if isSyntheticCompactionAckMessage(messages[summaryIdx+1]) {
		candidates = append(candidates, summaryIdx+2)
	}

	bestStart, bestLen := -1, 0
	for _, start := range candidates {
		if start >= len(messages) {
			continue
		}
		maxLen := summaryIdx
		if after := len(messages) - start; after < maxLen {
			maxLen = after
		}
		if overlap := messageFingerprintSuffixPrefixOverlap(fingerprints[:summaryIdx], fingerprints[start:start+maxLen]); overlap > bestLen {
			bestStart, bestLen = start, overlap
		}
	}
	return bestStart, bestLen
}

type fingerprintToken struct {
	value string
	sep   bool
}

// messageFingerprintSuffixPrefixOverlap returns the longest prefix of right
// that matches a suffix of left using a single prefix-function scan.
func messageFingerprintSuffixPrefixOverlap(left, right []string) int {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}

	combined := make([]fingerprintToken, 0, len(right)+1+len(left))
	for _, fingerprint := range right {
		combined = append(combined, fingerprintToken{value: fingerprint})
	}
	combined = append(combined, fingerprintToken{sep: true})
	for _, fingerprint := range left {
		combined = append(combined, fingerprintToken{value: fingerprint})
	}

	prefix := make([]int, len(combined))
	for i := 1; i < len(combined); i++ {
		j := prefix[i-1]
		for j > 0 && combined[i] != combined[j] {
			j = prefix[j-1]
		}
		if combined[i] == combined[j] {
			j++
		}
		prefix[i] = j
	}
	return prefix[len(prefix)-1]
}

func isSyntheticCompactionAckMessage(msg Message) bool {
	if msg.Role != llm.RoleAssistant {
		return false
	}
	return strings.TrimSpace(msg.TextContent) == "I've reviewed the context summary. I'll continue from where we left off."
}

func messageDisplayFingerprint(msg Message) string {
	parts, err := json.Marshal(msg.Parts)
	if err != nil {
		parts = []byte(msg.TextContent)
	}
	return string(msg.Role) + "\x00" + msg.TextContent + "\x00" + string(parts)
}

func isInternalCompactionSummaryMessage(msg Message) bool {
	if llm.IsInternalCompactionSummaryText(msg.TextContent) {
		return true
	}
	for _, part := range msg.Parts {
		if part.Type == llm.PartText && llm.IsInternalCompactionSummaryText(part.Text) {
			return true
		}
	}
	return false
}

// LLMActiveMessages converts the active slice of session messages into LLM
// messages, injecting systemPrompt only when the active context doesn't already
// include a system message, then applies provider-safe conversation filtering.
func LLMActiveMessages(messages []Message, activeStart int, systemPrompt string) []llm.Message {
	if activeStart > 0 {
		if activeStart < len(messages) {
			messages = messages[activeStart:]
		} else {
			messages = nil
		}
	}

	out := make([]llm.Message, 0, len(messages)+1)
	hasSystem := len(messages) > 0 && messages[0].Role == llm.RoleSystem
	if strings.TrimSpace(systemPrompt) != "" && !hasSystem {
		out = append(out, llm.SystemText(systemPrompt))
	}
	for i := range messages {
		out = append(out, messages[i].ToLLMMessage())
	}
	return llm.FilterConversationMessages(out)
}

// ResetContextEstimate clears the persisted context estimate baseline for the
// session after compaction.
func ResetContextEstimate(ctx context.Context, store Store, sess *Session) error {
	if store == nil || sess == nil {
		return nil
	}
	if err := store.UpdateContextEstimate(ctx, sess.ID, 0, 0); err != nil {
		return err
	}
	sess.LastTotalTokens = 0
	sess.LastMessageCount = 0
	return nil
}
