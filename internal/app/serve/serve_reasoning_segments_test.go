package serve

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestWebReasoningTextSegmentsMatchRecoveryAndReload(t *testing.T) {
	for _, reloadAt := range []string{"never", "after text", "after thought"} {
		t.Run(reloadAt, func(t *testing.T) {
			s := &serveServer{}
			run := newResponseRun("r", "s", "", "debug", 0, nil)
			state := newResponseRunStreamState("debug", "")
			send := func(ev core.Event) {
				t.Helper()
				if err := s.appendResponseRunEvent(nil, run, state, ev); err != nil {
					t.Fatal(err)
				}
			}
			reload := func(at string) {
				t.Helper()
				if reloadAt != at {
					return
				}
				saved, err := snapshotWebRun(run, state, &core.Continuation{Turn: 1}, &serveRuntime{providerKey: "debug"}, true, startResponseRunOptions{})
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(saved)
				if err != nil {
					t.Fatal(err)
				}
				var restored webRunContinuation
				if err := json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				run, state = restoreWebRun(&restored, func() {}), restored.streamState()
			}
			send(core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, Text: "first"})
			send(core.Event{Type: core.EventTextDelta, Text: "A", ProviderTurnIndexSet: true})
			reload("after text")
			send(core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, Text: "second"})
			send(core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, Text: " thought"})
			reload("after thought")
			send(core.Event{Type: core.EventTextDelta, Text: "B", ProviderTurnIndexSet: true})
			send(core.Event{Type: core.EventTextDelta, Text: "C", ProviderTurnIndexSet: true})

			want := responseRecoveryShape{Messages: []responseRecoveryMessageShape{
				{Role: "reasoning", Content: "first", ReasoningKind: "summary"},
				{Role: "assistant", Content: "A"},
				{Role: "reasoning", Content: "second thought", ReasoningKind: "summary", ReasoningIndex: 1},
				{Role: "assistant", Content: "BC", SegmentOrdinal: 1},
			}}
			if got := normalizeResponseRecovery(run.recoveryPayloadLocked()); !reflect.DeepEqual(got, want) {
				t.Fatalf("recovery = %#v, want %#v", got, want)
			}
			sub := run.subscribe(0)
			defer run.unsubscribe(sub.ch)
			folded, err := foldResponseEventStream(sub.replay)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(folded, want) {
				t.Fatalf("replay = %#v, want %#v", folded, want)
			}
			var boundaries []int
			for _, ev := range sub.replay {
				if ev.Event == "response.output_text.new_segment" {
					var payload map[string]any
					if err := json.Unmarshal(ev.Data, &payload); err != nil {
						t.Fatal(err)
					}
					boundaries = append(boundaries, responseRunIntValue(payload["assistant_segment_ordinal"], -1))
				}
			}
			if !reflect.DeepEqual(boundaries, []int{1}) {
				t.Fatalf("segment boundaries = %v, want [1]", boundaries)
			}
		})
	}
}

func TestWebHistoryReasoningProjectionUsesResolvedPolicy(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	s := &serveServer{cfgRef: &config.Config{Reasoning: cfg}}
	msg := session.Message{Role: core.RoleAssistant, Parts: []core.Part{
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "first", Kind: core.ReasoningKindSummary}, Text: "A"},
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "second", Kind: core.ReasoningKindRaw}, Text: "B"},
	}}
	// A cached batch policy must win over the server config for every part;
	// direct entry callers with a zero index still resolve the current policy.
	cached := cfg
	cached.Source, cached.MaxSummaryChars = "summary_only", 4
	for _, tc := range []struct {
		name  string
		index sessionMessageProjectionIndex
		want  []string
	}{
		{"cached", sessionMessageProjectionIndex{reasoningConfig: &cached}, []string{"reasoning:firs", "text:A", "text:B"}},
		{"fallback", sessionMessageProjectionIndex{}, []string{"reasoning:first", "text:A", "reasoning:second", "text:B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := s.sessionMessageEntryForMessage(&msg, tc.index)
			if !ok {
				t.Fatal("assistant entry omitted")
			}
			var got []string
			for _, part := range entry.Parts {
				got = append(got, part.Type+":"+part.Text)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parts = %q, want %q", got, tc.want)
			}
		})
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var restored session.Message
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	entries := s.sessionMessageEntries([]session.Message{restored})
	var got []string
	for _, part := range entries[0].Parts {
		got = append(got, part.Type+":"+part.Text)
	}
	if want := []string{"reasoning:first", "text:A", "reasoning:second", "text:B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("saved text/thought order = %q, want %q", got, want)
	}
}

func TestWebFilteredReasoningDoesNotSplitText(t *testing.T) {
	for _, kind := range []core.ReasoningKind{core.ReasoningKindUnknown, core.ReasoningKindEncrypted, core.ReasoningKindRaw} {
		t.Run(string(kind), func(t *testing.T) {
			cfg := config.DefaultReasoningConfig()
			cfg.Source = "summary_only"
			s := &serveServer{cfgRef: &config.Config{Reasoning: cfg}}
			run := newResponseRun("r", "s", "", "debug", 0, nil)
			state := newResponseRunStreamState("debug", "")
			for _, ev := range []core.Event{
				{Type: core.EventTextDelta, Text: "A"},
				{Type: core.EventReasoningDelta, ReasoningKind: kind, Text: "hidden"},
				{Type: core.EventTextDelta, Text: "B"},
			} {
				if err := s.appendResponseRunEvent(nil, run, state, ev); err != nil {
					t.Fatal(err)
				}
			}
			if len(run.recoveryMessages) != 1 || string(run.recoveryMessages[0].Content) != "AB" || state.assistantSegmentOrdinal != 0 {
				t.Fatalf("filtered thought split text: %#v", run.recoveryMessages)
			}
		})
	}
}
