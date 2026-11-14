package wire

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/reasoning"
)

func TestReasoningStreamSourceAndLimits(t *testing.T) {
	for _, source := range []string{"summary_only", "summary_or_provider_safe", "all"} {
		for _, kind := range []core.ReasoningKind{core.ReasoningKindSummary, core.ReasoningKindRaw, core.ReasoningKindEncrypted, core.ReasoningKindUnknown} {
			cfg := config.DefaultReasoningConfig()
			cfg.Source = source
			cfg.Display = "off"
			cfg.MaxSummaryChars = 3
			cfg.MaxRawChars = 3
			var state ReasoningStream
			state.Configure(cfg)
			p := state.Append(core.Event{Type: core.EventReasoningDelta, ReasoningKind: kind, Text: "a🙂bc"})
			want := kind == core.ReasoningKindSummary || kind == core.ReasoningKindRaw && source != "summary_only"
			if (p != nil) != want {
				t.Fatalf("source=%s kind=%s payload=%#v", source, kind, p)
			}
			if want && p["delta"] != "a🙂b" {
				t.Fatalf("uncapped: %#v", p)
			}
		}
	}
}
func TestReasoningStreamNumberingSurvivesRebuild(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	var s ReasoningStream
	s.Configure(cfg)
	s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, Text: "one"})
	s.Active = false
	s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, Text: "two"})
	for name, next := range map[string]ReasoningStream{"retry": s.Continue(), "reload": ResumeReasoningStream(s.NextIndex())} {
		next.Configure(cfg)
		if p := next.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, Text: "three"}); p["reasoning_index"] != 2 {
			t.Fatalf("%s reused an index: %#v", name, p)
		}
	}
}

func TestReasoningStreamItemBoundaryAndRetraction(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.Source = "summary_only"
	var s ReasoningStream
	s.Configure(cfg)
	s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, ReasoningItemID: "a", Text: "first"})
	p := s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, ReasoningItemID: "b", Text: "second"})
	if p["delta"] != "\n\nsecond" {
		t.Fatalf("items=%#v", p)
	}
	p = s.Append(core.Event{ReasoningKind: core.ReasoningKindRaw, Text: "must not leak"})
	if p["text"] != "" || p["kind"] != "summary" {
		t.Fatalf("did not retract: %#v", p)
	}
	if s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, Text: "also withheld"}) != nil {
		t.Fatal("non-displayable burst resumed")
	}
}

func TestReasoningDisplayExtractTitles(t *testing.T) {
	const content = "**Plan**\n\nFull summary body"
	for _, extract := range []bool{false, true} {
		cfg := config.DefaultReasoningConfig()
		cfg.ExtractTitles = extract
		liveTitle, liveBody := ReasoningDisplay("summary", content, "", cfg)
		historyTitle, historyBody := ReasoningDisplay("summary", content, "Plan", cfg)
		if liveTitle != historyTitle || liveBody != historyBody {
			t.Fatalf("extract=%v: live=(%q, %q), history=(%q, %q)", extract, liveTitle, liveBody, historyTitle, historyBody)
		}
		wantTitle, wantBody := "", content
		if extract {
			wantTitle, wantBody = "Plan", "Full summary body"
		}
		if liveTitle != wantTitle || liveBody != wantBody {
			t.Fatalf("extract=%v: got (%q, %q), want (%q, %q)", extract, liveTitle, liveBody, wantTitle, wantBody)
		}
	}
}

// Compare every projection to the unbounded item accumulator used by history.
// Caps apply to the combined prefix, including any inserted paragraph breaks.
func TestReasoningStreamBoundedProjection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		texts, ids []string
	}{
		{"multibyte", []string{"a🙂", "界🙂éz", "ignored"}, nil},
		{"new item", []string{"abc", "🙂界body", "ignored"}, []string{"a", "b", "b"}},
		{"empty item", []string{"abc", "", "🙂body"}, []string{"a", "b", "a"}},
		{"missing item", []string{"abc", "x", "🙂body"}, []string{"a", "", "b"}},
		{"title replacement", []string{"**Pl", "an**", "\n\n🙂Body", " more"}, nil},
		{"title becomes inline", []string{"**Plan**", " inline", " more"}, nil},
		{"split title CRLF", []string{"**Plan**\r", "\n\r", "\nBody"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, kind := range []core.ReasoningKind{core.ReasoningKindSummary, core.ReasoningKindRaw} {
				for limit := 1; limit <= 24; limit++ {
					cfg := config.DefaultReasoningConfig()
					cfg.MaxSummaryChars, cfg.MaxRawChars = limit, limit
					var s ReasoningStream
					s.Configure(cfg)
					var full strings.Builder
					var item, title, body string
					for i, text := range tc.texts {
						id := ""
						if tc.ids != nil {
							id = tc.ids[i]
						}
						reasoning.AppendStreamItemText(&full, &item, text, id)
						p := s.Append(core.Event{ReasoningKind: kind, ReasoningItemID: id, Text: text})
						title, body = applyReasoningPayload(title, body, p)
						wantTitle, wantBody := ReasoningDisplay(string(kind), full.String(), "", cfg)
						if title != wantTitle || body != wantBody {
							t.Fatalf("kind=%s limit=%d delta=%d: got (%q, %q), want (%q, %q), payload=%#v", kind, limit, i, title, body, wantTitle, wantBody, p)
						}
						if !utf8.ValidString(s.content.String()) || utf8.RuneCountInString(s.content.String()) > limit {
							t.Fatalf("kind=%s limit=%d: invalid or unbounded buffer %q", kind, limit, s.content.String())
						}
					}
				}
			}
		})
	}
}

func TestReasoningStreamBoundedItemSeparators(t *testing.T) {
	for _, suffix := range []string{"", "\n", "\r", "\r\n", "\n\n", "\r\n\r\n"} {
		for _, prefix := range []string{"", "\n", "\r", "\r\n", "\n\n", "\r\n\r\n"} {
			for limit := 1; limit <= 16; limit++ {
				cfg := config.DefaultReasoningConfig()
				cfg.MaxRawChars = limit
				var s ReasoningStream
				s.Configure(cfg)
				var full strings.Builder
				var item string
				for i, text := range []string{"🙂a" + suffix, prefix + "界body"} {
					id := []string{"a", "b"}[i]
					reasoning.AppendStreamItemText(&full, &item, text, id)
					s.Append(core.Event{ReasoningKind: core.ReasoningKindRaw, ReasoningItemID: id, Text: text})
				}
				want := reasoning.LimitReasoningText("raw", full.String(), cfg)
				if s.content.String() != want || s.body != want {
					t.Fatalf("suffix=%q prefix=%q limit=%d: content=%q body=%q want=%q", suffix, prefix, limit, s.content.String(), s.body, want)
				}
			}
		}
	}
}

func TestReasoningStreamCappedKindTransition(t *testing.T) {
	for _, source := range []string{"summary_only", "summary_or_provider_safe", "all"} {
		for _, limits := range [][2]int{{8, 20}, {20, 8}, {8, 8}} {
			cfg := config.DefaultReasoningConfig()
			cfg.Source = source
			cfg.MaxSummaryChars, cfg.MaxRawChars = limits[0], limits[1]
			var s ReasoningStream
			s.Configure(cfg)
			const text = "**Plan**\n\n🙂body more than either cap"
			s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, ReasoningItemID: "a", Text: text})
			if p := s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, ReasoningItemID: "b", Text: "more withheld summary"}); p != nil {
				t.Fatalf("source=%s limits=%v: full summary emitted %#v", source, limits, p)
			}
			// Even a classification-only event must be processed after the cap.
			p := s.Append(core.Event{ReasoningKind: core.ReasoningKindRaw})
			if source == "summary_only" {
				if p["kind"] != "summary" || p["title"] != "" || p["text"] != "" {
					t.Fatalf("limits=%v: did not retract capped summary: %#v", limits, p)
				}
				if s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, Text: "still withheld"}) != nil {
					t.Fatal("disallowed raw merged kind resumed")
				}
				continue
			}
			want := reasoning.LimitReasoningText("raw", text+"\n\nmore withheld summary", cfg)
			if s.kind != core.ReasoningKindRaw || s.title != "" || s.body != want || p == nil || p["kind"] != "raw" {
				t.Fatalf("source=%s limits=%v: kind=%s title=%q body=%q want=%q payload=%#v", source, limits, s.kind, s.title, s.body, want, p)
			}
		}
	}
}

func TestReasoningStreamVisibleKindOnlyUpdate(t *testing.T) {
	for _, source := range []string{"summary_or_provider_safe", "all"} {
		for _, limit := range []int{3, 6} {
			cfg := config.DefaultReasoningConfig()
			cfg.Source = source
			cfg.MaxSummaryChars, cfg.MaxRawChars = limit, limit
			var s ReasoningStream
			s.Configure(cfg)
			initial := s.Append(core.Event{ReasoningKind: core.ReasoningKindSummary, Text: "abc"})
			if initial["kind"] != "summary" || initial["delta"] != "abc" {
				t.Fatalf("initial projection: %#v", initial)
			}
			p := s.Append(core.Event{ReasoningKind: core.ReasoningKindRaw})
			if p == nil || p["kind"] != "raw" || p["title"] != "" || p["delta"] != "" || p["reasoning_index"] != initial["reasoning_index"] {
				t.Fatalf("source=%s limit=%d: missing kind-only update: %#v", source, limit, p)
			}
			if _, replaced := p["text"]; replaced {
				t.Fatalf("kind-only update replaced body: %#v", p)
			}
			for _, kind := range []core.ReasoningKind{core.ReasoningKindRaw, core.ReasoningKindSummary} {
				if p := s.Append(core.Event{ReasoningKind: kind}); p != nil {
					t.Fatalf("unchanged merged kind emitted %#v", p)
				}
			}
		}
	}
}

func TestReasoningStreamInitialMetadataDoesNotEmit(t *testing.T) {
	for _, kinds := range [][]core.ReasoningKind{
		{core.ReasoningKindSummary, core.ReasoningKindRaw},
		{core.ReasoningKindRaw, core.ReasoningKindSummary},
	} {
		cfg := config.DefaultReasoningConfig()
		var s ReasoningStream
		s.Configure(cfg)
		for _, kind := range kinds {
			if p := s.Append(core.Event{ReasoningKind: kind}); p != nil {
				t.Fatalf("metadata-only kind=%s emitted empty row: %#v", kind, p)
			}
		}
		if p := s.Append(core.Event{ReasoningKind: core.ReasoningKindRaw, Text: "abc"}); p["kind"] != "raw" || p["delta"] != "abc" {
			t.Fatalf("first visible projection: %#v", p)
		}
	}
}

func TestReasoningStreamSummaryLookahead(t *testing.T) {
	for _, source := range []string{"summary_or_provider_safe", "all"} {
		for _, rawLimit := range []int{5, 9, 20} {
			cfg := config.DefaultReasoningConfig()
			cfg.Source = source
			cfg.MaxSummaryChars, cfg.MaxRawChars = 3, rawLimit
			var s ReasoningStream
			s.Configure(cfg)
			var full strings.Builder
			var item string
			var title, body string
			for i, text := range []string{"a🙂b", "界", "\r\nnext", "a long ignored tail", "raw"} {
				kind := core.ReasoningKindSummary
				if i == 4 {
					kind = core.ReasoningKindRaw
				}
				id := []string{"a", "a", "b", "b", "c"}[i]
				reasoning.AppendStreamItemText(&full, &item, text, id)
				p := s.Append(core.Event{ReasoningKind: kind, ReasoningItemID: id, Text: text})
				title, body = applyReasoningPayload(title, body, p)
				wantTitle, wantBody := ReasoningDisplay(string(kind), full.String(), "", cfg)
				if title != wantTitle || body != wantBody {
					t.Fatalf("source=%s rawLimit=%d delta=%d: got (%q, %q), want (%q, %q)", source, rawLimit, i, title, body, wantTitle, wantBody)
				}
				if i > 0 && i < 4 && p != nil {
					t.Fatalf("capped summary emitted %#v", p)
				}
				if utf8.RuneCountInString(s.content.String()) > rawLimit {
					t.Fatalf("lookahead exceeded raw cap: %q", s.content.String())
				}
			}
		}
	}
}

func TestReasoningStreamFullRawNoAllocations(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	var s ReasoningStream
	s.Configure(cfg)
	s.Append(core.Event{ReasoningKind: core.ReasoningKindRaw, ReasoningItemID: "a", Text: strings.Repeat("🙂", cfg.MaxRawChars+100)})
	before, capacity := s.content.String(), s.content.Cap()
	delta := core.Event{ReasoningKind: core.ReasoningKindRaw, ReasoningItemID: "b", Text: strings.Repeat("界", 100)}
	var emitted bool
	allocs := testing.AllocsPerRun(100, func() {
		emitted = emitted || s.Append(delta) != nil
	})
	if emitted || allocs != 0 {
		t.Fatalf("full raw stream emitted=%v allocations=%v", emitted, allocs)
	}
	if s.content.String() != before || s.content.Cap() != capacity || utf8.RuneCountInString(before) != cfg.MaxRawChars {
		t.Fatalf("full raw buffer grew: runes=%d bytes=%d capacity=%d", utf8.RuneCountInString(s.content.String()), s.content.Len(), s.content.Cap())
	}
	// A new burst must discard the cap and per-item state.
	s.Active = false
	if p := s.Append(core.Event{ReasoningKind: core.ReasoningKindRaw, ReasoningItemID: "c", Text: "new"}); p["delta"] != "new" {
		t.Fatalf("new burst did not reset: %#v", p)
	}
}

func applyReasoningPayload(title, body string, p map[string]any) (string, string) {
	if p == nil {
		return title, body
	}
	title = p["title"].(string)
	if text, ok := p["text"].(string); ok {
		body = text
	} else if delta, ok := p["delta"].(string); ok {
		body += delta
	}
	return title, body
}
