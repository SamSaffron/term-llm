package clibin

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func claudeAssistantUsageLine(id, parent string, input, cacheRead, cacheWrite int) string {
	parentField := `null`
	if parent != "" {
		parentField = `"` + parent + `"`
	}
	return `{"type":"assistant","parent_tool_use_id":` + parentField + `,"message":{"id":"` + id +
		`","model":"claude-opus-5-5","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":` + strconv.Itoa(input) +
		`,"cache_read_input_tokens":` + strconv.Itoa(cacheRead) + `,"cache_creation_input_tokens":` + strconv.Itoa(cacheWrite) +
		`,"output_tokens":16}}}`
}

// Shapes mirror a real Claude Code 2.1.303 stream-json tool loop: per-call
// usage on each assistant event, and a result whose usage is summed across all
// calls while modelUsage carries the context window.
func TestClaudeBinReportsContextPerAPICall(t *testing.T) {
	p := NewClaudeBinProvider("", nil)
	events := make(chan core.Event, 32)
	send := eventSender{ctx: context.Background(), ch: events}
	var (
		lastUsage       *core.Usage
		sawTextDelta    bool
		fallbackText    string
		handledTerminal bool
	)
	lines := []string{
		claudeAssistantUsageLine("msg_1", "", 2, 12597, 12915),
		// Same API call repeated for a second content block: no new reading.
		claudeAssistantUsageLine("msg_1", "", 2, 12597, 12915),
		// A subagent call runs in its own window and must not move the meter.
		claudeAssistantUsageLine("msg_side", "toolu_1", 2, 900000, 0),
		claudeAssistantUsageLine("msg_2", "", 2, 25512, 78),
		claudeAssistantUsageLine("msg_3", "", 2, 25590, 78),
		`{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":6,"cache_creation_input_tokens":13071,"cache_read_input_tokens":63699,"output_tokens":184},"modelUsage":{"claude-opus-5-5":{"inputTokens":6,"contextWindow":1000000,"maxOutputTokens":128000}}}`,
	}
	for _, line := range lines {
		if err := p.handleClaudeLine(context.Background(), line, false, send, &lastUsage, &sawTextDelta, &fallbackText, &handledTerminal, false); err != nil {
			t.Fatalf("handleClaudeLine: %v", err)
		}
	}
	close(events)

	var got []core.ContextUsage
	for ev := range events {
		if ev.Type == core.EventContextUsage {
			got = append(got, *ev.Context)
		}
	}
	want := []core.ContextUsage{
		{UsedTokens: 25514},
		{UsedTokens: 25592},
		{UsedTokens: 25670},
		{UsedTokens: 25670, Window: 1000000},
	}
	if len(got) != len(want) {
		t.Fatalf("context readings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("context reading %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The window is known from the first turn onwards, so later turns report
	// it with every mid-turn checkpoint.
	events = make(chan core.Event, 4)
	send = eventSender{ctx: context.Background(), ch: events}
	if err := p.handleClaudeLine(context.Background(), claudeAssistantUsageLine("msg_4", "", 3, 25700, 200), false, send, &lastUsage, &sawTextDelta, &fallbackText, &handledTerminal, false); err != nil {
		t.Fatalf("handleClaudeLine: %v", err)
	}
	close(events)
	ev := <-events
	if ev.Type != core.EventContextUsage || *ev.Context != (core.ContextUsage{UsedTokens: 25903, Window: 1000000}) {
		t.Fatalf("next turn reading = %+v, want 25903/1000000", ev.Context)
	}

	// A mid-run model change (e.g. Claude Code's fallback model) invalidates
	// the window until that model's result reports its own.
	events = make(chan core.Event, 4)
	send = eventSender{ctx: context.Background(), ch: events}
	sonnet := strings.Replace(claudeAssistantUsageLine("msg_5", "", 3, 26000, 0), "claude-opus-5-5", "claude-sonnet-5", 1)
	if err := p.handleClaudeLine(context.Background(), sonnet, false, send, &lastUsage, &sawTextDelta, &fallbackText, &handledTerminal, false); err != nil {
		t.Fatalf("handleClaudeLine: %v", err)
	}
	close(events)
	ev = <-events
	if ev.Type != core.EventContextUsage || *ev.Context != (core.ContextUsage{UsedTokens: 26003}) {
		t.Fatalf("model-change reading = %+v, want 26003 with unknown window", ev.Context)
	}
}

func TestClaudeBinEphemeralRequestsDoNotReportContext(t *testing.T) {
	p := NewClaudeBinProvider("", nil)
	events := make(chan core.Event, 4)
	send := eventSender{ctx: context.Background(), ch: events}
	var (
		lastUsage       *core.Usage
		sawTextDelta    bool
		fallbackText    string
		handledTerminal bool
	)
	if err := p.handleClaudeLine(context.Background(), claudeAssistantUsageLine("msg_1", "", 2, 100, 100), false, send, &lastUsage, &sawTextDelta, &fallbackText, &handledTerminal, true); err != nil {
		t.Fatalf("handleClaudeLine: %v", err)
	}
	close(events)
	for ev := range events {
		if ev.Type == core.EventContextUsage {
			t.Fatalf("ephemeral request reported context %+v", ev.Context)
		}
	}
}
