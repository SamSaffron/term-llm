package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestAcknowledgedImageStreamingTicksDoNotRetransmitPayload(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()
	m := newTestChatModel(true)
	m.width, m.height = 60, 18
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.tracker = ui.NewToolTracker()
	m.tracker.AddImageSegment(writeChatTestPNG(t))
	m.streaming = true
	m.bumpContentVersion()

	first := m.View().PostFrame
	if len(first) == 0 || strings.Count(string(first), "a=t") != 1 {
		t.Fatalf("initial image payload = %q, want one upload", first)
	}
	acknowledgePostFrameForTest(t, m)
	second := m.View().PostFrame
	if len(second) != 0 {
		t.Fatalf("first unchanged acknowledged payload = %q, want empty", second)
	}

	started := time.Now()
	bytes600 := 0
	uploads600 := 0
	for range 600 {
		payload := m.View().PostFrame
		bytes600 += len(payload)
		uploads600 += strings.Count(string(payload), "a=t")
	}
	if bytes600 != 0 || uploads600 != 0 {
		t.Fatalf("600 unchanged ticks emitted bytes=%d uploads=%d, want zero", bytes600, uploads600)
	}
	t.Logf("initial=%d modeled_unacknowledged_600=%d acknowledged_600=%d modeled_uploads_before=%d uploads_after=%d compose_600=%s", len(first), len(first)*600, bytes600, 600, uploads600, time.Since(started))
}
