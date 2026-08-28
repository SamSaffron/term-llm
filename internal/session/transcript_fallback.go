package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// FallbackTranscriptIndexer adapts a Store without native revisioned transcript
// reads to the single web transcript protocol. Revisions are monotonic for the
// lifetime of the adapter and advance whenever the coherent message projection
// or compaction metadata changes.
type FallbackTranscriptIndexer struct {
	store   Store
	baseRev int64
	mu      sync.Mutex
	state   map[string]fallbackTranscriptState
}

type fallbackTranscriptState struct {
	fingerprint [32]byte
	rev         int64
}

func NewFallbackTranscriptIndexer(store Store) *FallbackTranscriptIndexer {
	return &FallbackTranscriptIndexer{
		store:   store,
		baseRev: time.Now().UnixMilli(),
		state:   make(map[string]fallbackTranscriptState),
	}
}

func (f *FallbackTranscriptIndexer) read(ctx context.Context, sessionID string) (TranscriptSnapshot, []Message, error) {
	sess, err := f.store.Get(ctx, sessionID)
	if err != nil {
		return TranscriptSnapshot{}, nil, err
	}
	if sess == nil {
		return TranscriptSnapshot{}, nil, ErrNotFound
	}
	messages, err := f.store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return TranscriptSnapshot{}, nil, err
	}
	sort.SliceStable(messages, func(i, j int) bool {
		if messages[i].Sequence != messages[j].Sequence {
			return messages[i].Sequence < messages[j].Sequence
		}
		return messages[i].ID < messages[j].ID
	})
	visible := make([]Message, 0, len(messages))
	for i := range messages {
		msg := messages[i]
		if msg.Role == llm.RoleSystem || msg.Role == llm.RoleDeveloper || msg.IsGoalSteering() {
			continue
		}
		visible = append(visible, msg)
	}
	messages = visible

	fingerprintPayload := struct {
		CompactionSeq   int
		CompactionCount int
		Messages        []Message
	}{sess.CompactionSeq, sess.CompactionCount, messages}
	encoded, err := json.Marshal(fingerprintPayload)
	if err != nil {
		return TranscriptSnapshot{}, nil, err
	}
	fingerprint := sha256.Sum256(encoded)
	f.mu.Lock()
	state := f.state[sessionID]
	if state.rev == 0 {
		state.rev = f.baseRev
		state.fingerprint = fingerprint
	} else if state.fingerprint != fingerprint {
		state.rev++
		state.fingerprint = fingerprint
	}
	f.state[sessionID] = state
	f.mu.Unlock()

	snapshot := TranscriptSnapshot{
		Rev:             state.rev,
		CompactionSeq:   sess.CompactionSeq,
		CompactionCount: sess.CompactionCount,
		Items:           make([]TranscriptIndexItem, 0, len(messages)),
	}
	planToolCalls := make(map[string]bool)
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.Name == "update_plan" {
				planToolCalls[part.ToolCall.ID] = true
			}
		}
	}
	for _, msg := range messages {
		item := TranscriptIndexItem{
			Seq:                     msg.Sequence,
			ID:                      msg.ID,
			Role:                    string(msg.Role),
			ResponseID:              msg.ResponseID,
			AssistantSegmentOrdinal: msg.AssistantSegmentOrdinal,
		}
		if msg.Role == llm.RoleUser {
			item.ClientMessageID = msg.ClientMessageID
		}
		if msg.CompactionTail {
			item.Flags |= TranscriptFlagCompactionTail
		}
		if !transcriptRowHasDisplayBody(msg.Role, msg.Parts, planToolCalls) {
			item.Flags |= TranscriptFlagEmptyBody
		}
		snapshot.Items = append(snapshot.Items, item)
	}
	return snapshot, append([]Message(nil), messages...), nil
}

func (f *FallbackTranscriptIndexer) GetTranscriptSnapshot(ctx context.Context, sessionID string) (TranscriptSnapshot, error) {
	snapshot, _, err := f.read(ctx, sessionID)
	return snapshot, err
}

func (f *FallbackTranscriptIndexer) GetTranscriptIndex(ctx context.Context, sessionID string) (int64, []TranscriptIndexItem, error) {
	snapshot, err := f.GetTranscriptSnapshot(ctx, sessionID)
	return snapshot.Rev, snapshot.Items, err
}

func transcriptTupleWithin(msg Message, r TranscriptRange) bool {
	afterStart := msg.Sequence > r.StartSeq || (msg.Sequence == r.StartSeq && msg.ID >= r.StartID)
	beforeEnd := msg.Sequence < r.EndSeq || (msg.Sequence == r.EndSeq && msg.ID <= r.EndID)
	return afterStart && beforeEnd
}

func (f *FallbackTranscriptIndexer) GetMessagesByTranscriptRanges(ctx context.Context, sessionID string, ranges []TranscriptRange) (int64, []Message, error) {
	snapshot, messages, err := f.read(ctx, sessionID)
	if err != nil {
		return 0, nil, err
	}
	selected := make([]Message, 0)
	seen := make(map[int64]struct{})
	for _, msg := range messages {
		for _, transcriptRange := range ranges {
			if transcriptTupleWithin(msg, transcriptRange) {
				if _, ok := seen[msg.ID]; !ok {
					selected = append(selected, msg)
					seen[msg.ID] = struct{}{}
				}
				break
			}
		}
	}
	return snapshot.Rev, selected, nil
}

func (f *FallbackTranscriptIndexer) TranscriptRev(ctx context.Context, sessionID string) (int64, error) {
	snapshot, err := f.GetTranscriptSnapshot(ctx, sessionID)
	return snapshot.Rev, err
}
