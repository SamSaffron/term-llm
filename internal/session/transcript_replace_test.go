package session

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// These tests are the executable form of the transcript replacement contract
// in README.md ("Transcript replacement and compaction"):
//
//   - ReplaceMessages replaces the active context (what LoadActiveMessages
//     returns). The store reads the boundary inside the write transaction.
//   - ResetMessages replaces the whole transcript and clears the boundary.

type transcriptState struct {
	boundary  int // -1 when the session has no compaction boundary
	userTurns int
	rows      []Message
}

func loadTranscriptState(t *testing.T, ctx context.Context, store Store, id string) transcriptState {
	t.Helper()
	sess, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.GetMessages(ctx, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	boundary := -1
	if HasCompactionBoundary(sess) {
		boundary = sess.CompactionSeq
	}
	return transcriptState{boundary: boundary, userTurns: sess.UserTurns, rows: rows}
}

func texts(rows []Message) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.TextContent
	}
	return out
}

func messagesFor(id string, texts ...string) []Message {
	out := make([]Message, len(texts))
	for i, text := range texts {
		out[i] = *NewMessage(id, core.UserText(text), -1)
	}
	return out
}

// newTranscriptSession returns a session in one of the states the contract
// distinguishes. "compacted" has archive [system, old user, old assistant]
// and active tail [summary, ack] from sequence 3.
func newTranscriptSession(t *testing.T, ctx context.Context, store Store, state string) *Session {
	t.Helper()
	sess := seedContextStateSession(t, ctx, store)
	switch state {
	case "uncompacted":
	case "compacted":
		tail := []Message{*NewMessage(sess.ID, core.UserText("summary"), -1), *NewMessage(sess.ID, core.AssistantText("ack"), -1)}
		if err := store.CompactMessages(ctx, sess.ID, tail); err != nil {
			t.Fatal(err)
		}
	case "stale boundary":
		// A boundary with no rows at or after it (legacy/corrupt state).
		if _, err := store.(*SQLiteStore).db.ExecContext(ctx, "UPDATE sessions SET compaction_seq = 10, compaction_count = 1 WHERE id = ?", sess.ID); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown state %q", state)
	}
	return sess
}

func TestTranscriptReplacementContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		reset     bool
		write     []string
		wantErr   error
		wantRows  []string // whole transcript after the write
		wantBound int      // -1: no boundary
	}{
		{name: "replace without boundary rewrites whole transcript", state: "uncompacted",
			write: []string{"a", "b"}, wantRows: []string{"a", "b"}, wantBound: -1},
		{name: "replace with boundary rewrites only the active tail", state: "compacted",
			write: []string{"summary", "retried"}, wantRows: []string{"system", "old user", "old assistant", "summary", "retried"}, wantBound: 3},
		{name: "replace with stale boundary rewrites whole transcript and clears it", state: "stale boundary",
			write: []string{"a"}, wantRows: []string{"a"}, wantBound: -1},
		{name: "empty active tail is rejected and nothing changes", state: "compacted",
			write: nil, wantErr: ErrEmptyActiveContext, wantRows: []string{"system", "old user", "old assistant", "summary", "ack"}, wantBound: 3},
		{name: "empty replace without boundary clears the transcript", state: "uncompacted",
			write: nil, wantRows: []string{}, wantBound: -1},
		{name: "empty reset clears transcript and boundary", state: "compacted", reset: true,
			write: nil, wantRows: []string{}, wantBound: -1},
		{name: "reset rewrites whole transcript and clears boundary", state: "compacted", reset: true,
			write: []string{"fresh"}, wantRows: []string{"fresh"}, wantBound: -1},
		{name: "reset without boundary rewrites whole transcript", state: "uncompacted", reset: true,
			write: []string{"fresh"}, wantRows: []string{"fresh"}, wantBound: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newContextStateTestStore(t)
			sess := newTranscriptSession(t, ctx, store, tc.state)
			before := loadTranscriptState(t, ctx, store, sess.ID)

			write := store.ReplaceMessages
			if tc.reset {
				write = store.ResetMessages
			}
			err := write(ctx, sess.ID, messagesFor(sess.ID, tc.write...))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			after := loadTranscriptState(t, ctx, store, sess.ID)
			if got := texts(after.rows); !slices.Equal(got, tc.wantRows) {
				t.Fatalf("rows = %q, want %q", got, tc.wantRows)
			}
			for i := range after.rows {
				if after.rows[i].Sequence != i {
					t.Fatalf("row %d has sequence %d; sequences must stay contiguous", i, after.rows[i].Sequence)
				}
			}
			if after.boundary != tc.wantBound {
				t.Fatalf("boundary = %d, want %d", after.boundary, tc.wantBound)
			}
			// Archived rows are kept, not rewritten: same row identity.
			for i := 0; i < tc.wantBound; i++ {
				if after.rows[i].ID != before.rows[i].ID {
					t.Fatalf("archived row %d was rewritten", i)
				}
			}
			if tc.wantBound >= 0 {
				sessAfter, err := store.Get(ctx, sess.ID)
				if err != nil {
					t.Fatal(err)
				}
				active, err := LoadActiveMessages(ctx, store, sessAfter)
				if err != nil {
					t.Fatal(err)
				}
				if len(active) != len(tc.wantRows)-tc.wantBound {
					t.Fatalf("active context = %q, want rows from %d of %q", texts(active), tc.wantBound, tc.wantRows)
				}
			}
		})
	}
}

// user_turns is increment-only. A tail rewrite counts only user turns it adds
// beyond those it replaces; retained compaction-tail copies never count.
func TestReplaceActiveTailCountsOnlyNewUserTurns(t *testing.T) {
	ctx := context.Background()
	store := newContextStateTestStore(t)
	sess := newTranscriptSession(t, ctx, store, "compacted")
	start := loadTranscriptState(t, ctx, store, sess.ID).userTurns

	retained := *NewMessage(sess.ID, core.UserText("unanswered (retained copy)"), -1)
	retained.CompactionTail = true
	tail := append(messagesFor(sess.ID, "summary"), retained, *NewMessage(sess.ID, core.UserText("new turn"), -1))
	if err := store.ReplaceMessages(ctx, sess.ID, tail); err != nil {
		t.Fatal(err)
	}
	// "summary" replaces the existing summary user row; the retained copy does
	// not count; "new turn" does.
	if got := loadTranscriptState(t, ctx, store, sess.ID).userTurns; got != start+1 {
		t.Fatalf("user_turns = %d, want %d", got, start+1)
	}
	if err := store.ReplaceMessages(ctx, sess.ID, tail); err != nil {
		t.Fatal(err)
	}
	if got := loadTranscriptState(t, ctx, store, sess.ID).userTurns; got != start+1 {
		t.Fatalf("rewriting the same tail changed user_turns to %d, want %d", got, start+1)
	}
}

// Tail rows keep their compaction_tail marker; a whole-transcript rewrite has
// no boundary for rows to be a tail of.
func TestReplacementCompactionTailMarkers(t *testing.T) {
	ctx := context.Background()
	for _, reset := range []bool{false, true} {
		store := newContextStateTestStore(t)
		sess := newTranscriptSession(t, ctx, store, "compacted")
		retained := *NewMessage(sess.ID, core.UserText("retained"), -1)
		retained.CompactionTail = true
		write := store.ReplaceMessages
		if reset {
			write = store.ResetMessages
		}
		if err := write(ctx, sess.ID, append(messagesFor(sess.ID, "summary"), retained)); err != nil {
			t.Fatal(err)
		}
		rows := loadTranscriptState(t, ctx, store, sess.ID).rows
		if got := rows[len(rows)-1].CompactionTail; got == reset {
			t.Fatalf("reset=%t: compaction_tail = %t, want %t", reset, got, !reset)
		}
	}
}
