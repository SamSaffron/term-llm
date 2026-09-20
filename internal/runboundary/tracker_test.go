package runboundary

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestNilTrackerSnapshotHasNoCompletedTurn(t *testing.T) {
	var tracker *Tracker
	if got := tracker.CompletedSnapshot(); got.TurnIndex != -1 || got.Durable || got.DurableAnchorID != 0 {
		t.Fatalf("nil tracker snapshot = %#v", got)
	}
}

func TestTrackerCompletedAndDurableBoundaries(t *testing.T) {
	base := []llm.Message{llm.SystemText("system"), llm.UserText("question")}
	tracker := New("run-1", base, 10, true, ProviderContext{})
	tracker.UpdateAssistant("run-1", llm.AssistantText("partial"))
	if got := tracker.CompletedSnapshot(); len(got.Messages) != len(base) || got.DurableAnchorID != 10 || !got.Durable {
		t.Fatalf("partial advanced boundary: %#v", got)
	}
	toolCall := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "read_file"}}}}
	toolResult := llm.ToolResultMessage("call-1", "read_file", "result", nil)
	if !tracker.Commit("run-1", 0, []llm.Message{toolCall, toolResult}, ProviderContext{}) {
		t.Fatal("commit rejected")
	}
	if got := tracker.CompletedSnapshot(); len(got.Messages) != 4 || got.DurableAnchorID != 10 {
		t.Fatalf("completed boundary = %#v", got)
	}
	if !tracker.PublishDurable("run-1", 0, 12) {
		t.Fatal("durable publication rejected")
	}
	tracker.UpdateAssistant("run-1", llm.AssistantText("next partial"))
	got := tracker.CompletedSnapshot()
	if len(got.Messages) != 4 || got.DurableAnchorID != 12 || !got.Durable {
		t.Fatalf("next partial advanced boundary: %#v", got)
	}
}

func TestTrackerPublishesProviderStateWithTheMessagesItBelongsTo(t *testing.T) {
	base := []llm.Message{llm.SystemText("system"), llm.UserText("question")}
	start := ProviderContext{State: []byte(`{"session_id":"S1","messages_sent":2}`), WorkingDir: "/w", ProviderKey: "claude-bin", Model: "opus-max"}
	tracker := New("run-1", base, 10, true, start)

	got := tracker.CompletedSnapshot()
	if string(got.Provider.State) != string(start.State) || got.Provider.WorkingDir != "/w" {
		t.Fatalf("run-start provider context = %#v", got.Provider)
	}

	turn := ProviderContext{State: []byte(`{"session_id":"S1","messages_sent":3}`), WorkingDir: "/w", ProviderKey: "claude-bin", Model: "opus-max"}
	if !tracker.Commit("run-1", 0, []llm.Message{llm.AssistantText("answer")}, turn) {
		t.Fatal("commit rejected")
	}
	got = tracker.CompletedSnapshot()
	if string(got.Provider.State) != string(turn.State) || len(got.Messages) != 3 {
		t.Fatalf("committed boundary = %#v", got)
	}

	// A turn completion with no provider advance (steering injection, error
	// recovery) must keep the previous state rather than publish nothing: being
	// behind the messages is safe, being ahead of them is not.
	if !tracker.Commit("run-1", 1, []llm.Message{llm.UserText("steering")}, ProviderContext{}) {
		t.Fatal("stateless commit rejected")
	}
	got = tracker.CompletedSnapshot()
	if string(got.Provider.State) != string(turn.State) || len(got.Messages) != 4 {
		t.Fatalf("stateless commit dropped the prior state: %#v", got)
	}

	// A rejected out-of-order commit must not pair new state with old messages.
	ahead := ProviderContext{State: []byte(`{"session_id":"S1","messages_sent":99}`), ProviderKey: "claude-bin", Model: "opus-max"}
	if tracker.Commit("run-1", 1, []llm.Message{llm.AssistantText("duplicate")}, ahead) {
		t.Fatal("accepted an out-of-order commit")
	}
	if got = tracker.CompletedSnapshot(); string(got.Provider.State) != string(turn.State) {
		t.Fatalf("rejected commit published state ahead of its messages: %#v", got.Provider)
	}

	// Durability and branchability are different questions.
	if !tracker.InvalidateDurable("run-1") {
		t.Fatal("invalidate rejected")
	}
	if got = tracker.CompletedSnapshot(); string(got.Provider.State) != string(turn.State) || got.Durable {
		t.Fatalf("durable invalidation dropped provider state: %#v", got)
	}
}

func TestTrackerResetReplacesProviderContext(t *testing.T) {
	tracker := New("run-1", nil, 0, false, ProviderContext{State: []byte("old"), ProviderKey: "claude-bin", Model: "opus"})
	tracker.Reset("run-2", nil, 0, false, ProviderContext{State: []byte("new"), ProviderKey: "chatgpt", Model: "gpt-5"})
	got := tracker.CompletedSnapshot()
	if string(got.Provider.State) != "new" || got.Provider.ProviderKey != "chatgpt" {
		t.Fatalf("reset provider context = %#v", got.Provider)
	}
	if got.Provider.MatchesProviderModel("claude-bin", "opus") {
		t.Fatal("a provider/model swap must make the boundary ineligible")
	}
	if !got.Provider.MatchesProviderModel("chatgpt", "gpt-5") || !got.Provider.Available() {
		t.Fatalf("captured identity = %#v", got.Provider)
	}
}

func TestSnapshotProviderStateIsDetached(t *testing.T) {
	state := []byte("mutable")
	tracker := New("run-1", nil, 0, false, ProviderContext{State: state})
	state[0] = 'M'
	got := tracker.CompletedSnapshot()
	if string(got.Provider.State) != "mutable" {
		t.Fatalf("captured state aliased the caller's buffer: %q", got.Provider.State)
	}
	got.Provider.State[0] = 'X'
	if again := tracker.CompletedSnapshot(); string(again.Provider.State) != "mutable" {
		t.Fatalf("snapshot state aliased tracker state: %q", again.Provider.State)
	}
}

func TestTrackerFailsClosedAndRejectsStaleUpdates(t *testing.T) {
	tracker := New("old", []llm.Message{llm.UserText("old")}, 1, true, ProviderContext{})
	tracker.Reset("new", []llm.Message{llm.UserText("new")}, 0, false, ProviderContext{})
	if tracker.Commit("old", 0, []llm.Message{llm.AssistantText("stale")}, ProviderContext{}) || tracker.PublishDurable("old", 0, 99) {
		t.Fatal("accepted stale run update")
	}
	if got := tracker.CompletedSnapshot(); got.Durable || got.DurableAnchorID != 0 || len(got.Messages) != 1 {
		t.Fatalf("new boundary corrupted: %#v", got)
	}
	if !tracker.Commit("new", 0, []llm.Message{llm.AssistantText("done")}, ProviderContext{}) {
		t.Fatal("new commit rejected")
	}
	if tracker.PublishDurable("new", -1, 2) || tracker.PublishDurable("new", 0, 0) {
		t.Fatal("accepted mismatched or root durable publication")
	}
	if !tracker.PublishDurable("new", 0, 2) || !tracker.InvalidateDurable("new") {
		t.Fatal("publish/invalidate failed")
	}
	if got := tracker.CompletedSnapshot(); got.Durable || got.DurableAnchorID != 0 {
		t.Fatalf("invalidation failed: %#v", got)
	}
}
