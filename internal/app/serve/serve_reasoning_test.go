package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestWebReasoningStreamProjectionAndReplay(t *testing.T) {
	s := &serveServer{}
	run := newResponseRun("resp-thought", "sess", "", "debug", time.Now().Unix(), nil)
	state := newResponseRunStreamState("debug", "")
	send := func(kind core.ReasoningKind, text string) {
		t.Helper()
		if err := s.appendResponseRunEvent(nil, run, state, core.Event{Type: core.EventReasoningDelta, ReasoningKind: kind, Text: text, ReasoningItemID: "one"}); err != nil {
			t.Fatal(err)
		}
	}
	send(core.ReasoningKindSummary, "**Checking")
	send(core.ReasoningKindSummary, " the path**\n\nBody")
	send(core.ReasoningKindSummary, " text")
	send(core.ReasoningKindEncrypted, "secret")
	send(core.ReasoningKindUnknown, "unclassified")
	events := run.activeEventsLocked()
	if len(events) != 3 {
		t.Fatalf("events=%d", len(events))
	}
	var payload map[string]any
	if err := json.Unmarshal(events[1].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["text"] != "Body" || payload["title"] != "Checking the path" || payload["reasoning_index"] != float64(0) {
		t.Fatalf("replacement: %#v", payload)
	}
	if err := json.Unmarshal(events[2].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["delta"] != " text" {
		t.Fatalf("extension: %#v", payload)
	}
	snapshot := run.recoveryPayloadLocked()["messages"].([]map[string]any)
	if len(snapshot) != 1 || snapshot[0]["role"] != "reasoning" || snapshot[0]["content"] != "Body text" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	replay := run.subscribe(1)
	if len(replay.replay) != 2 {
		t.Fatalf("replay=%#v", replay)
	}
	run.unsubscribe(replay.ch)
	if err := s.appendResponseRunEvent(nil, run, state, core.Event{Type: core.EventAttemptDiscard}); err != nil {
		t.Fatal(err)
	}
	if len(run.recoveryMessages) != 0 {
		t.Fatal("discard retained thoughts")
	}
	send(core.ReasoningKindRaw, "provider thinking")
	if run.recoveryMessages[0].ReasoningIndex != 1 {
		t.Fatal("index reused after discard")
	}
}

// A web-run reload rebuilds the stream state. Clients and recovery key thoughts
// by index, so a thought after the reload must not append to the first one.
func TestWebReasoningIndexSurvivesReload(t *testing.T) {
	s := &serveServer{}
	run := newResponseRun("resp-reload-thought", "sess", "", "debug", time.Now().Unix(), nil)
	state := newResponseRunStreamState("debug", "")
	think := func(run *responseRun, state *responseRunStreamState, text string) {
		t.Helper()
		if err := s.appendResponseRunEvent(nil, run, state, core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	think(run, state, "first")
	if err := s.appendResponseRunEvent(nil, run, state, core.Event{Type: core.EventTextDelta, Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	think(run, state, "second")
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
	resumed := restoreWebRun(&restored, func() {})
	think(resumed, restored.streamState(), "after reload")
	var thoughts []string
	for _, msg := range resumed.recoveryMessages {
		if msg.Role == "reasoning" {
			thoughts = append(thoughts, fmt.Sprintf("%d:%s", msg.ReasoningIndex, msg.Content))
		}
	}
	if want := []string{"0:first", "1:second", "2:after reload"}; !reflect.DeepEqual(thoughts, want) {
		t.Fatalf("thoughts=%q want %q", thoughts, want)
	}
}

func TestWebReasoningBurstBoundaries(t *testing.T) {
	for _, typ := range []core.EventType{core.EventTextDelta, core.EventToolCall, core.EventToolExecStart, core.EventToolExecEnd, core.EventToolActivity, core.EventSteering, core.EventCompaction, core.EventModelSwitch, core.EventAttemptDiscard} {
		t.Run(string(typ), func(t *testing.T) {
			s := &serveServer{}
			run := newResponseRun("r", "s", "", "debug", 0, nil)
			state := newResponseRunStreamState("debug", "")
			thought := core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, Text: "before"}
			if err := s.appendResponseRunEvent(nil, run, state, thought); err != nil {
				t.Fatal(err)
			}
			ev := core.Event{Type: typ, ToolActivity: &core.ToolActivity{ID: "native", Name: "web_search", Status: core.ToolActivityCompleted}, Text: "visible", SteeringID: "steer", Tool: &core.ToolCall{ID: "call", Name: "read_file"}, ToolCallID: "call", ToolName: "read_file", PreviousModel: "old", Model: "new", ModelSwitchBoundaryID: "swap", ProviderTurnIndexSet: true, ProviderTurnIndex: 1}
			if err := s.appendResponseRunEvent(&serveRuntime{}, run, state, ev); err != nil {
				t.Fatal(err)
			}
			thought.Text = "after"
			if err := s.appendResponseRunEvent(nil, run, state, thought); err != nil {
				t.Fatal(err)
			}
			if run.recoveryMessages[len(run.recoveryMessages)-1].ReasoningIndex != 1 {
				t.Fatal("no new index")
			}
		})
	}
}

func TestWebReasoningHistoryParts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    core.ReasoningKind
		source  string
		persist bool
		want    bool
	}{
		{"summary", core.ReasoningKindSummary, "summary_only", true, true},
		{"legacy", "", "summary_only", true, true},
		{"raw safe", core.ReasoningKindRaw, "summary_or_provider_safe", false, true},
		{"raw blocked", core.ReasoningKindRaw, "summary_only", true, false},
		{"encrypted", core.ReasoningKindEncrypted, "all", true, false},
		{"unknown", core.ReasoningKindUnknown, "all", true, false},
		{"not persisted", core.ReasoningKindSummary, "all", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultReasoningConfig()
			cfg.Display = "off"
			cfg.Source = tc.source
			cfg.PersistSummaries = tc.persist
			cfg.PersistSummariesSet = true
			s := &serveServer{cfgRef: &config.Config{Reasoning: cfg}}
			part := core.Part{Type: core.PartText, Text: "answer", ReasoningContent: "ignored", ReasoningSummaryParts: []string{"**Title**", "body"}, ReasoningKind: tc.kind}
			entry := sessionMessageEntry{}
			appendSessionMessageTextWithReasoning(&entry, &session.Message{Role: core.RoleAssistant}, part, nil, "", s.cfgRef.ResolveReasoning("serve"))
			wantTypes := []string{"text"}
			if tc.want {
				wantTypes = []string{"reasoning", "text"}
			}
			var types []string
			for _, p := range entry.Parts {
				types = append(types, p.Type)
			}
			if !reflect.DeepEqual(types, wantTypes) {
				t.Fatalf("parts=%#v", entry.Parts)
			}
			if tc.want && tc.kind != core.ReasoningKindRaw && (entry.Parts[0].Text != "body" || entry.Parts[0].ReasoningTitle != "Title") {
				t.Fatalf("split=%#v", entry.Parts)
			}
			part.Text = ""
			entry.Parts = nil
			appendSessionMessageTextWithReasoning(&entry, &session.Message{Role: core.RoleAssistant}, part, nil, "", s.cfgRef.ResolveReasoning("serve"))
			if tc.want && (len(entry.Parts) != 1 || entry.Parts[0].Type != "reasoning") {
				t.Fatal("reasoning-only part omitted")
			}
		})
	}
}

func TestWebReasoningCapabilities(t *testing.T) {
	t.Setenv("TERM_LLM_SHOW_RAW_REASONING", "")
	s := &serveServer{}
	get := func() (map[string]any, string) {
		w := httptest.NewRecorder()
		s.handleCapabilities(w, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d", w.Code)
		}
		var p map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p["reasoning"].(map[string]any), w.Header().Get("ETag")
	}
	defaults, etag := get()
	if defaults["display"] != "off" || defaults["raw"] != false || defaults["status"] != "title" || defaults["history"] != "collapsed" || defaults["hidden_label"] != "Thinking..." {
		t.Fatalf("defaults=%#v", defaults)
	}
	cfg := config.DefaultReasoningConfig()
	cfg.Serve.Display = "raw"
	cfg.Serve.Status = "summary"
	cfg.Serve.History = "expanded"
	cfg.HiddenLabel = "Private thought"
	s.cfgRef = &config.Config{Reasoning: cfg}
	blocked, nextETag := get()
	if blocked["display"] != "collapsed" || blocked["status"] != "summary" || blocked["history"] != "expanded" || blocked["hidden_label"] != "Private thought" || etag == nextETag {
		t.Fatalf("blocked=%#v etag=%s", blocked, nextETag)
	}
	raw := true
	cfg.Serve.Raw = &raw
	s.cfgRef.Reasoning = cfg
	allowed, _ := get()
	if allowed["display"] != "raw" || allowed["raw"] != true {
		t.Fatalf("allowed=%#v", allowed)
	}
}

func TestWebReasoningOnlyPartPrecedesToolsAndLimitsHistory(t *testing.T) {
	t.Setenv("TERM_LLM_SHOW_RAW_REASONING", "")
	cfg := config.DefaultReasoningConfig()
	cfg.MaxSummaryChars = 4
	s := &serveServer{cfgRef: &config.Config{Reasoning: cfg}}
	msg := &session.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, ReasoningKind: core.ReasoningKindSummary, ReasoningContent: "a🙂bcd", ReasoningSummaryTitle: "Saved title"}, {Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "read", Name: "read_file"}}}}
	var entry sessionMessageEntry
	s.appendSessionMessageParts(&entry, msg, sessionMessageProjectionIndex{}, map[string]bool{}, "")
	if len(entry.Parts) != 2 || entry.Parts[0].Type != "reasoning" || entry.Parts[0].Text != "a🙂bc" || entry.Parts[0].ReasoningTitle != "Saved title" || entry.Parts[1].Type != "tool_call" {
		t.Fatalf("parts=%#v", entry.Parts)
	}
}

func TestWebReasoningConfigIsResolvedOncePerRun(t *testing.T) {
	t.Setenv("TERM_LLM_SHOW_RAW_REASONING", "")
	cfg := config.DefaultReasoningConfig()
	cfg.MaxSummaryChars = 4
	s := &serveServer{cfgRef: &config.Config{Reasoning: cfg}}
	run := newResponseRun("r", "s", "", "debug", 0, nil)
	state := newResponseRunStreamState("debug", "")
	for i, text := range []string{"ab", "cdefgh"} {
		if i == 1 {
			s.cfgRef.Reasoning.MaxSummaryChars = 99
		}
		if err := s.appendResponseRunEvent(nil, run, state, core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	if string(run.recoveryMessages[0].Content) != "abcd" {
		t.Fatalf("policy changed mid-run: %#v", run.recoveryMessages)
	}
}
