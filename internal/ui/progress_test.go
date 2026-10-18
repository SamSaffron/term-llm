package ui

import (
	"strings"
	"testing"
	"time"
)

func TestFormatElapsedDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{name: "negative clamps to zero", d: -time.Second, want: "0s"},
		{name: "zero", d: 0, want: "0s"},
		{name: "rounds to nearest second", d: 1500 * time.Millisecond, want: "2s"},
		{name: "seconds only", d: 8 * time.Second, want: "8s"},
		{name: "max seconds only", d: 59 * time.Second, want: "59s"},
		{name: "one minute keeps seconds", d: time.Minute, want: "1m00s"},
		{name: "minutes and seconds", d: 82 * time.Second, want: "1m22s"},
		{name: "minutes zero-pads seconds", d: 12*time.Minute + 4*time.Second, want: "12m04s"},
		{name: "max below hour", d: 59*time.Minute + 59*time.Second, want: "59m59s"},
		{name: "one hour keeps minutes and seconds", d: time.Hour, want: "1h00m00s"},
		{name: "hours minutes and seconds", d: time.Hour + 2*time.Minute + 33*time.Second, want: "1h02m33s"},
		{name: "hours zero-pads seconds", d: 3*time.Hour + 41*time.Minute + 8*time.Second, want: "3h41m08s"},
		{name: "one day keeps all units", d: 24 * time.Hour, want: "1d00h00m00s"},
		{name: "days hours minutes and seconds", d: 28*time.Hour + 22*time.Minute + 9*time.Second, want: "1d04h22m09s"},
		{name: "multiple days", d: 51*time.Hour + time.Second, want: "2d03h00m01s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatElapsedDuration(tt.d); got != tt.want {
				t.Fatalf("FormatElapsedDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestStreamingIndicator_RendersReadableElapsedWithoutPhaseEllipsis(t *testing.T) {
	styles := DefaultStyles()

	out := StreamingIndicator{
		Spinner:      "•",
		Phase:        "Thinking",
		Elapsed:      8732 * time.Second,
		ShowCancel:   true,
		HideProgress: false,
	}.Render(styles)

	plain := StripANSI(out)
	if strings.Contains(plain, "Thinking...") {
		t.Fatalf("indicator should not render ellipsis after phase label; got %q", plain)
	}
	if strings.Contains(plain, "8732.0s") || strings.Contains(plain, "8732s") {
		t.Fatalf("indicator should not render raw elapsed seconds; got %q", plain)
	}
	if !strings.Contains(plain, "Thinking 2h25m32s") {
		t.Fatalf("indicator should render readable elapsed duration; got %q", plain)
	}
	if !strings.Contains(plain, "(esc to cancel)") {
		t.Fatalf("indicator should preserve cancel hint; got %q", plain)
	}
}

func TestStreamingIndicator_AddsBlankLineAfterFlushedText_WhenRenderingActiveTool(t *testing.T) {
	styles := DefaultStyles()

	out := StreamingIndicator{
		Segments: []*Segment{
			{
				Type:       SegmentTool,
				ToolName:   "web_search",
				ToolInfo:   "(query: latest updates)",
				ToolStatus: ToolPending,
			},
		},
		WavePos:         0,
		Width:           80,
		RenderMarkdown:  RenderMarkdown,
		HasFlushed:      true,
		LastFlushedType: SegmentText,
	}.Render(styles)

	plain := StripANSI(out)
	if !strings.HasPrefix(plain, "\n") {
		t.Fatalf("active tool indicator should start with one compensating newline after flushed text; got %q", plain)
	}
	if strings.HasPrefix(plain, "\n\n") {
		t.Fatalf("active tool indicator should start with exactly one newline after flushed text; got %q", plain)
	}
	if !strings.Contains(plain, "web_search") {
		t.Fatalf("expected active tool text in indicator, got %q", plain)
	}
}

func TestStreamingIndicator_NoLeadingNewlineAfterFlush_WhenRenderingSpinner(t *testing.T) {
	styles := DefaultStyles()

	out := StreamingIndicator{
		Spinner:         "•",
		Phase:           "Thinking",
		Elapsed:         time.Second,
		Width:           80,
		HasFlushed:      true,
		LastFlushedType: SegmentText,
	}.Render(styles)

	plain := StripANSI(out)
	if strings.HasPrefix(plain, "\n") {
		t.Fatalf("spinner indicator should not start with a newline after flush; got %q", plain)
	}
	if !strings.Contains(plain, "Thinking") {
		t.Fatalf("expected phase text in indicator, got %q", plain)
	}
}

func TestTokenCountFormattingAcrossUI(t *testing.T) {
	for _, tt := range []struct {
		n    int
		want string
	}{
		{999, "999"},
		{2000, "2k"},
		{12780, "12.78k"},
		{999999, "1M"},
		{12780000, "12.78M"},
	} {
		if got := FormatTokenCount(tt.n); got != tt.want {
			t.Errorf("FormatTokenCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
		p := SubagentProgress{AgentName: "worker", ToolCalls: 1, InputTokens: tt.n}
		if got, want := p.RenderHeader(false), "@worker  1 calls · "+tt.want+" tokens"; got != want {
			t.Errorf("RenderHeader() = %q, want %q", got, want)
		}
		if got, want := formatSpawnAgentStats(1, tt.n, time.Time{}, time.Time{}), "1 call · "+tt.want+" tokens"; got != want {
			t.Errorf("formatSpawnAgentStats() = %q, want %q", got, want)
		}
	}
}
