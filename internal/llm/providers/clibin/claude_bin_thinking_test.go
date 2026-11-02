package clibin

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func hasThinkingDisplay(args []string) bool {
	i := slices.Index(args, claudeThinkingDisplayFlag)
	return i >= 0 && i+1 < len(args) && args[i+1] == "summarized"
}

func TestClaudeBinThinkingSummariesAreOnByDefaultAndCanBeTurnedOff(t *testing.T) {
	t.Setenv(claudeExperimentalBetasEnv, "1") // a shell value must not block the default
	for _, enabled := range []bool{true, false} {
		p := NewClaudeBinProvider("opus", map[string]string{claudeExperimentalBetasEnv: "1"})
		if !enabled {
			p.SetThinkingSummaries(false)
		}
		args, _ := p.buildArgs(context.Background(), core.Request{}, eventSender{})
		if hasThinkingDisplay(args) != enabled {
			t.Fatalf("summaries=%v: args %q", enabled, args)
		}
		env := envSliceMap(p.buildCommandEnv(""))
		debugEnv, _ := p.commandEnvDebugFields("")
		value, set := env[claudeExperimentalBetasEnv]
		// With summaries off, the full isolation applies; with them on, the
		// betas stay enabled whatever the shell or provider env says.
		if enabled && set || !enabled && value != "1" {
			t.Fatalf("summaries=%v: %s=%q (set %v)", enabled, claudeExperimentalBetasEnv, value, set)
		}
		if _, debugSet := debugEnv[claudeExperimentalBetasEnv]; debugSet != !enabled {
			t.Fatalf("summaries=%v: diagnostic env %v disagrees with the command env", enabled, debugEnv)
		}
		if env["CLAUDE_CODE_DISABLE_CLAUDE_MDS"] != "1" {
			t.Fatalf("summaries=%v dropped the rest of the isolation env", enabled)
		}
		if clone := p.cloneHelperProvider(); clone.ThinkingSummariesEnabled() != enabled {
			t.Fatalf("helper clone lost thinking_summaries=%v", enabled)
		}
	}
}

func TestClaudeBinStreamsThinkingSummariesAsProviderThinking(t *testing.T) {
	p := NewClaudeBinProvider("opus", nil)
	p.sessionID = "session"
	events := make(chan core.Event, 16)
	send := eventSender{ctx: context.Background(), ch: events}
	var (
		usage    *core.Usage
		sawText  bool
		fallback string
		handled  bool
		thinking claudeThinkingStream
	)
	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Checking the config."}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque"}}}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":8,"session_id":"other"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Reading it."}}}`,
		// The next API call of Claude Code's tool loop starts a second block.
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"It sets the port."}}}`,
	}
	for _, line := range lines {
		if err := p.handleClaudeLine(context.Background(), line, false, send, &usage, &sawText, &fallback, &handled, &thinking, false); err != nil {
			t.Fatal(err)
		}
	}
	close(events)
	var got []string
	for event := range events {
		switch event.Type {
		case core.EventReasoningDelta:
			if event.ReasoningKind != core.ReasoningKindRaw || event.ReasoningEncryptedContent != "" || event.ReasoningItemID != "" {
				t.Fatalf("reasoning event %+v, want plain provider thinking", event)
			}
			got = append(got, "thought:"+event.Text)
		case core.EventTextDelta:
			got = append(got, "text:"+event.Text)
		}
	}
	want := []string{"thought:Checking the config.", "text:Reading it.", "thought:\n\nIt sets the port."}
	if !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if p.sessionID != "session" {
		t.Fatalf("thinking-token line replaced the session id with %q", p.sessionID)
	}
}

func TestClaudeBinRetriesWithoutThinkingDisplayWhenClaudeCodeRejectsIt(t *testing.T) {
	t.Cleanup(func() { claudeThinkingDisplayUnsupported.Store(false) })
	p := NewClaudeBinProvider("opus", nil)
	var runs [][]string
	var betasForced []bool
	p.commandRunner = func(_ context.Context, args []string, _, _, _ string, _ bool, send eventSender, _, _ bool) error {
		runs = append(runs, append([]string(nil), args...))
		_, forced := envSliceMap(p.buildCommandEnv(""))[claudeExperimentalBetasEnv]
		betasForced = append(betasForced, forced)
		if hasThinkingDisplay(args) {
			return &UserFacingProviderError{Summary: "Claude Code exited", Cause: &CLICommandError{
				ExitCode: 1, StderrTail: "error: unknown option '--thinking-display'",
			}}
		}
		return send.Send(core.Event{Type: core.EventTextDelta, Text: "answer"})
	}
	for turn := 0; turn < 2; turn++ {
		stream, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hi")}, Ephemeral: true})
		if err != nil {
			t.Fatal(err)
		}
		for {
			event, err := stream.Recv()
			if errors.Is(err, io.EOF) || event.Type == core.EventDone {
				break
			}
			if err != nil || event.Err != nil {
				t.Fatalf("turn %d: %v %v", turn, err, event.Err)
			}
		}
		stream.Close()
	}
	// One rejected start, then every run goes without the flag and with the
	// full isolation restored.
	if len(runs) != 3 || !hasThinkingDisplay(runs[0]) || hasThinkingDisplay(runs[1]) || hasThinkingDisplay(runs[2]) {
		t.Fatalf("runs %q", runs)
	}
	if !slices.Equal(betasForced, []bool{false, true, true}) {
		t.Fatalf("experimental betas forced per run = %v", betasForced)
	}
	if strings.Contains(strings.Join(runs[1], " "), "summarized") {
		t.Fatalf("retry kept the flag value: %q", runs[1])
	}
}
