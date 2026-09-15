package cmd

import (
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func modelUsageStatsRow(t *testing.T, rows []serveStatsModel, model string) serveStatsModel {
	t.Helper()
	for _, row := range rows {
		if row.Model == model {
			return row
		}
	}
	t.Fatalf("no %q row in %+v", model, rows)
	return serveStatsModel{}
}

func TestUsageCallKindClassifiesRetainedRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		call ui.UsageCall
		want session.ModelUsageKind
	}{
		{name: "session's own turn", call: ui.UsageCall{Model: "opus", InputTokens: 10}, want: session.ModelUsageMain},
		{name: "compaction", call: ui.UsageCall{Compaction: true}, want: session.ModelUsageCompaction},
		{name: "handover", call: ui.UsageCall{Handover: true}, want: session.ModelUsageHandover},
		{name: "side question", call: ui.UsageCall{SideQuestion: true}, want: session.ModelUsageSideQuestion},
		{name: "guardian", call: ui.UsageCall{Guardian: true}, want: session.ModelUsageGuardian},
		{name: "subagent", call: ui.UsageCall{Subagent: true}, want: session.ModelUsageSubagent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageCallKind(tc.call); got != tc.want {
				t.Fatalf("usageCallKind(%+v) = %q, want %q", tc.call, got, tc.want)
			}
		})
	}
}

// TestAppendRuntimeModelStatsIncludesSessionOwnModel pins the regression: the
// breakdown used to be built from child sessions alone, so a session's own
// model — the dominant spender — never appeared.
func TestAppendRuntimeModelStatsIncludesSessionOwnModel(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SetModel("opus")
	stats.AddUsage(1000, 60, 20000, 500)
	stats.AddGuardianUsageForModel("gpt-5.6-sol-high", 40, 12, 0, 0)
	stats.AddHandoverUsageForModel("haiku", 90, 8, 0, 0)
	stats.AddSubagentUsageForModel("haiku", 300, 25, 8000, 0)
	calls, _ := stats.UsageCalls()

	out := &serveStatsResponse{Scope: "runtime_local", Models: []serveStatsModel{}}
	appendRuntimeModelStats(out, calls, nil, map[string]bool{}, time.Now(), 0)

	models := []string{}
	for _, row := range out.Models {
		models = append(models, row.Model)
	}
	if !slices.Equal(models, []string{"gpt-5.6-sol-high", "haiku", "opus"}) {
		t.Fatalf("models = %v, want stable model order", models)
	}

	own := modelUsageStatsRow(t, out.Models, "opus")
	if own.InputTokens != 1000 || own.OutputTokens != 60 || own.CachedInputTokens != 20000 || own.CacheWriteTokens != 500 {
		t.Fatalf("session's own token split = %+v", own)
	}
	if own.LLMTurns != 1 || own.ToolCalls != 0 {
		t.Fatalf("session's own turn counts = %+v", own)
	}
	if !slices.Equal(own.Kinds, []string{string(session.ModelUsageMain)}) {
		t.Fatalf("session's own kinds = %v, want [main]", own.Kinds)
	}

	guardian := modelUsageStatsRow(t, out.Models, "gpt-5.6-sol-high")
	if guardian.InputTokens != 40 || guardian.OutputTokens != 12 || guardian.CachedInputTokens != 0 || guardian.CacheWriteTokens != 0 {
		t.Fatalf("guardian token split = %+v", guardian)
	}
	if !slices.Equal(guardian.Kinds, []string{string(session.ModelUsageGuardian)}) {
		t.Fatalf("guardian kinds = %v, want [guardian]", guardian.Kinds)
	}

	// Two helper kinds billed to the same model stay one row with both kinds.
	helper := modelUsageStatsRow(t, out.Models, "haiku")
	if helper.InputTokens != 390 || helper.OutputTokens != 33 || helper.CachedInputTokens != 8000 || helper.CacheWriteTokens != 0 {
		t.Fatalf("helper token split = %+v", helper)
	}
	if helper.LLMTurns != 2 || helper.ToolCalls != 0 {
		t.Fatalf("helper turn counts = %+v", helper)
	}
	if !slices.Equal(helper.Kinds, []string{string(session.ModelUsageHandover), string(session.ModelUsageSubagent)}) {
		t.Fatalf("helper kinds = %v, want [handover subagent]", helper.Kinds)
	}
}

func TestAppendDurableModelStatsMergesKindsAndEstimatesCost(t *testing.T) {
	out := &serveStatsResponse{
		Scope:       "durable_history",
		Models:      []serveStatsModel{},
		Unavailable: []string{"runtime_metrics", "models", "historical_timing", "helper_usage"},
	}
	entries := []session.ModelUsage{
		{Model: "opus", Kind: session.ModelUsageMain, InputTokens: 5720, OutputTokens: 149063, CachedInputTokens: 35795091, CacheWriteTokens: 465243, LLMTurns: 1, ToolCalls: 12},
		{Model: "opus", Kind: session.ModelUsageGuardian, InputTokens: 40, OutputTokens: 12, LLMTurns: 1, ToolCalls: 1},
		{Model: "gpt-5.6-sol-high", Kind: session.ModelUsageSubagent, InputTokens: 50868, OutputTokens: 12791, CachedInputTokens: 679040, LLMTurns: 1, ToolCalls: 7},
	}
	// Totals that exactly match the attribution must leave no remainder.
	appendDurableModelStats(out, entries, webSessionMetrics{
		InputTokens: 56628, OutputTokens: 161866, CachedInputTokens: 36474131, CacheWriteTokens: 465243,
	})
	if out.Unattributed != nil {
		t.Fatalf("fully attributed totals reported a remainder: %+v", out.Unattributed)
	}

	if len(out.Models) != 2 {
		t.Fatalf("models = %+v, want one row per model", out.Models)
	}
	own := modelUsageStatsRow(t, out.Models, "opus")
	if own.InputTokens != 5760 || own.OutputTokens != 149075 || own.CachedInputTokens != 35795091 || own.CacheWriteTokens != 465243 {
		t.Fatalf("merged token split = %+v", own)
	}
	if own.LLMTurns != 2 || own.ToolCalls != 13 {
		t.Fatalf("merged turn counts = %+v", own)
	}
	if !slices.Equal(own.Kinds, []string{string(session.ModelUsageMain), string(session.ModelUsageGuardian)}) {
		t.Fatalf("merged kinds = %v, want [main guardian]", own.Kinds)
	}
	// Recorded history keeps no request boundaries, but a session that spent
	// tens of dollars must not report a dash: the rows are priced as one
	// aggregate estimate per model.
	if own.CostUSD == nil || *own.CostUSD <= 0 {
		t.Fatalf("recorded tokens left unpriced: %+v", own)
	}
	if own.Running || own.ActiveMS != nil || own.ToolMS != nil {
		t.Fatalf("recorded history has no timing: %+v", own)
	}

	child := modelUsageStatsRow(t, out.Models, "gpt-5.6-sol-high")
	if child.InputTokens != 50868 || child.OutputTokens != 12791 || child.CachedInputTokens != 679040 || child.CacheWriteTokens != 0 {
		t.Fatalf("child token split = %+v", child)
	}
	// Both rows are priced from their own recorded totals.
	if child.CostUSD == nil || *child.CostUSD <= 0 {
		t.Fatalf("delegated row left unpriced: %+v", child)
	}

	if slices.Contains(out.Unavailable, "models") {
		t.Fatalf("models must no longer be unavailable once attribution exists: %v", out.Unavailable)
	}
	if !slices.Equal(out.Unavailable, []string{"runtime_metrics", "historical_timing", "helper_usage"}) {
		t.Fatalf("unavailable = %v, want the other capabilities preserved", out.Unavailable)
	}
}

func TestAppendDurableModelStatsWithoutRecordedAttribution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []session.ModelUsage
	}{
		{name: "nil entries", entries: nil},
		{name: "empty entries", entries: []session.ModelUsage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &serveStatsResponse{
				Scope:       "durable_history",
				Models:      []serveStatsModel{},
				Unavailable: []string{"runtime_metrics", "models", "helper_usage"},
			}
			appendDurableModelStats(out, tc.entries, webSessionMetrics{InputTokens: 1234, OutputTokens: 56})

			if len(out.Models) != 0 {
				t.Fatalf("models = %+v, want no rows without recorded attribution", out.Models)
			}
			if !slices.Equal(out.Unavailable, []string{"runtime_metrics", "models", "helper_usage"}) {
				t.Fatalf("unavailable = %v, want models still listed", out.Unavailable)
			}
			// A session recorded before attribution existed must still say
			// where its tokens went, rather than implying it spent nothing.
			if out.Unattributed == nil || out.Unattributed.InputTokens != 1234 || out.Unattributed.OutputTokens != 56 {
				t.Fatalf("unattributed = %+v, want the whole recorded bucket", out.Unattributed)
			}
		})
	}
}

// Drift in either direction must surface rather than cancel out.
func TestUnattributedTotals(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		totals, attributed    webSessionMetrics
		wantFound             bool
		wantInput, wantOutput int
	}{
		{name: "fully attributed", totals: webSessionMetrics{InputTokens: 10, OutputTokens: 5}, attributed: webSessionMetrics{InputTokens: 10, OutputTokens: 5}},
		{name: "partly attributed", totals: webSessionMetrics{InputTokens: 10, OutputTokens: 5}, attributed: webSessionMetrics{InputTokens: 4}, wantFound: true, wantInput: 6, wantOutput: 5},
		{name: "over attributed clamps", totals: webSessionMetrics{InputTokens: 2, OutputTokens: 9}, attributed: webSessionMetrics{InputTokens: 7}, wantFound: true, wantOutput: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remainder, found := unattributedTotals(tc.totals, tc.attributed)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if remainder.InputTokens != tc.wantInput || remainder.OutputTokens != tc.wantOutput {
				t.Fatalf("remainder = %+v", remainder)
			}
		})
	}
}

func TestRemoveUnavailableDropsOnlyNamedKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{name: "drops repeats of the named key", in: []string{"models", "cost_usd", "models"}, want: []string{"cost_usd"}},
		{name: "absent key preserves the list", in: []string{"runtime_metrics", "cost_usd"}, want: []string{"runtime_metrics", "cost_usd"}},
		{name: "empty list stays empty", in: nil, want: nil},
		{name: "only the named key leaves nothing", in: []string{"models", "models"}, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := removeUnavailable(append([]string(nil), tc.in...), "models")
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("removeUnavailable(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Sections with nothing in them are noise: session 8207's stats modal was five
// consecutive all-zero or "unavailable" blocks between the models table and the
// counters that mattered.
func TestStatsOmitsSectionsWithNoRecordedActivity(t *testing.T) {
	sectionTitles := func(out *serveStatsResponse) []string {
		var titles []string
		for _, section := range out.Sections {
			titles = append(titles, section.Title)
		}
		return titles
	}

	t.Run("idle runtime omits every helper section", func(t *testing.T) {
		rt := &serveRuntime{}
		rt.stats.init()
		got := rt.statsSnapshot()
		if titles := sectionTitles(got); len(titles) != 0 {
			t.Fatalf("idle runtime emitted sections %v, want none", titles)
		}
	})

	t.Run("sections appear once their helper runs", func(t *testing.T) {
		rt := &serveRuntime{}
		rt.recordHelperStats("guardian", "haiku", llm.Usage{InputTokens: 7238, OutputTokens: 12})
		titles := sectionTitles(rt.statsSnapshot())
		if !slices.Contains(titles, "Guardian Usage") {
			t.Fatalf("guardian section missing after a guardian review: %v", titles)
		}
		for _, absent := range []string{"Private Side-Question Usage", "Compaction Usage", "Handover / Path-Note Usage"} {
			if slices.Contains(titles, absent) {
				t.Fatalf("%q emitted with no activity: %v", absent, titles)
			}
		}
	})

	t.Run("recorded history drops the unavailable placeholders", func(t *testing.T) {
		out := statsResponseForRuntime(nil)
		for _, absent := range []string{"Private Side-Question Usage", "Guardian Usage", "Compaction Usage", "Handover / Path-Note Usage", "Runtime Activity"} {
			if slices.Contains(sectionTitles(out), absent) {
				t.Fatalf("%q placeholder still emitted: %v", absent, sectionTitles(out))
			}
		}
		// The machine-readable list still reports what is missing.
		if !slices.Contains(out.Unavailable, "helper_usage") {
			t.Fatalf("unavailable = %v, want helper_usage still declared", out.Unavailable)
		}
	})
}

// Timing is recorded per turn, so recorded history can report how long a
// session's own work took even after every runtime that ran it is gone.
func TestDurableModelStatsReportRecordedTiming(t *testing.T) {
	out := &serveStatsResponse{
		Scope:       "durable_history",
		Models:      []serveStatsModel{},
		Unavailable: []string{"active_ms", "model_ms", "tool_ms", "historical_timing"},
	}
	entries := []session.ModelUsage{
		{Model: "opus", Kind: session.ModelUsageMain, InputTokens: 10, OutputTokens: 2, LLMMS: 30_000, ToolMS: 5_000},
		{Model: "opus", Kind: session.ModelUsageGuardian, InputTokens: 4, OutputTokens: 1, LLMMS: 1_000},
		{Model: "haiku", Kind: session.ModelUsageSubagent, InputTokens: 3, OutputTokens: 1},
	}
	appendDurableModelStats(out, entries, webSessionMetrics{InputTokens: 17, OutputTokens: 4})

	own := modelUsageStatsRow(t, out.Models, "opus")
	if own.ActiveMS == nil || *own.ActiveMS != 36_000 {
		t.Fatalf("opus active time = %v, want model+tool time", own.ActiveMS)
	}
	if own.ToolMS == nil || *own.ToolMS != 5_000 {
		t.Fatalf("opus tool time = %v, want 5000", own.ToolMS)
	}
	// A row that recorded no clock must stay blank rather than claim zero.
	if child := modelUsageStatsRow(t, out.Models, "haiku"); child.ActiveMS != nil || child.ToolMS != nil {
		t.Fatalf("untimed row invented timing: %+v", child)
	}

	llmMS, toolMS := sessionTimingFromModels(entries)
	if llmMS != 31_000 || toolMS != 5_000 {
		t.Fatalf("session timing = %d/%d, want 31000/5000", llmMS, toolMS)
	}
}

// Each persisted turn must carry only the clock that turn added, or a long
// session would re-record its whole history on every turn.
func TestTurnTimingRecordsOnlyTheNewClock(t *testing.T) {
	rt := &serveRuntime{}
	rt.startStats("opus")
	rt.finishStats()

	rt.stats.stats.LLMTime, rt.stats.stats.ToolTime = 3*time.Second, time.Second
	first := rt.pendingTurnTiming()
	if first.LLMMS != 3_000 || first.ToolMS != 1_000 {
		t.Fatalf("first turn = %+v, want 3000/1000", first)
	}
	rt.commitTurnTiming(first)

	rt.stats.stats.LLMTime += 2 * time.Second
	second := rt.pendingTurnTiming()
	if second.LLMMS != 2_000 || second.ToolMS != 0 {
		t.Fatalf("second turn = %+v, want only the new 2000ms", second)
	}
	rt.commitTurnTiming(second)

	if idle := rt.pendingTurnTiming(); idle.LLMMS != 0 || idle.ToolMS != 0 {
		t.Fatalf("turn with no new work = %+v", idle)
	}
}

// A turn that never reaches the store must not consume the clock, and a clock
// that rewound (a discarded attempt) must not make the next turn re-record time
// that is already stored.
func TestTurnTimingSurvivesUnrecordedTurns(t *testing.T) {
	rt := &serveRuntime{}
	rt.startStats("opus")
	rt.finishStats()

	rt.stats.stats.LLMTime = 4 * time.Second
	// Measured but never committed: the write was dropped or failed.
	rt.pendingTurnTiming()
	rt.stats.stats.LLMTime += time.Second
	recorded := rt.pendingTurnTiming()
	if recorded.LLMMS != 5_000 {
		t.Fatalf("dropped turn lost its clock: %+v, want the full 5000ms", recorded)
	}
	rt.commitTurnTiming(recorded)

	rt.stats.stats.LLMTime = time.Second // rewound by a discarded attempt
	rewound := rt.pendingTurnTiming()
	if rewound.LLMMS != 0 {
		t.Fatalf("rewound clock reported work: %+v", rewound)
	}
	rt.commitTurnTiming(rewound)
	rt.stats.stats.LLMTime = 6 * time.Second
	if after := rt.pendingTurnTiming(); after.LLMMS != 1_000 {
		t.Fatalf("watermark moved backwards: %+v, want only the new 1000ms", after)
	}
}

// The headline answers what the whole session spent and how long it worked,
// across every runtime it ever had — without hiding what the rows cannot
// account for, and without freezing while a turn is still running.
func TestApplyDurableSessionTotalsReportsWholeSession(t *testing.T) {
	srv := &serveServer{}
	entries := []session.ModelUsage{
		{Model: "opus", Kind: session.ModelUsageMain, InputTokens: 1_000, OutputTokens: 100, LLMMS: 20_000, ToolMS: 4_000},
	}
	// Recorded totals exceed the attribution: the session spent more than the
	// rows can explain.
	totals := webSessionMetrics{InputTokens: 5_000, OutputTokens: 100}

	history := &serveStatsResponse{
		Scope: "durable_history", Models: []serveStatsModel{},
		Unavailable: []string{"cost_usd", "historical_cost", "active_ms", "model_ms", "tool_ms", "historical_model_usage"},
	}
	srv.applyDurableSessionTotals(history, nil, entries, sessionDelegatedCost{}, totals)
	if history.CostUSD == nil || *history.CostUSD <= 0 {
		t.Fatalf("session cost = %v, want the recorded rows priced", history.CostUSD)
	}
	if !history.CostPartial {
		t.Fatal("unattributed spend must mark the session cost as a lower bound")
	}
	if history.ActiveMS == nil || *history.ActiveMS != 24_000 || *history.ModelMS != 20_000 || *history.ToolMS != 4_000 {
		t.Fatalf("session timing = %v/%v/%v", history.ActiveMS, history.ModelMS, history.ToolMS)
	}
	if len(history.Models) != 1 {
		t.Fatalf("recorded history lost its breakdown: %+v", history.Models)
	}
	if len(history.Unavailable) != 0 {
		t.Fatalf("unavailable = %v, want every reported capability dropped", history.Unavailable)
	}

	rt := &serveRuntime{}
	rt.startStats("opus")
	rt.finishStats()
	rt.stats.stats.LLMTime = 3 * time.Second
	observed := 99.0
	live := &serveStatsResponse{
		Scope: "runtime_local", Models: []serveStatsModel{{Model: "observed-by-runtime"}},
		CostUSD: &observed, ActiveMS: msPointer(1), ModelMS: msPointer(1), ToolMS: msPointer(0),
	}
	srv.applyDurableSessionTotals(live, rt, entries, sessionDelegatedCost{CostUSD: 4, Partial: true}, totals)
	if len(live.Models) != 1 || live.Models[0].Model != "observed-by-runtime" {
		t.Fatalf("live breakdown replaced by recorded rows: %+v", live.Models)
	}
	// Recorded rows are the whole session and the runtime only ever holds the
	// slice it ran, so the runtime figure does not replace them; delegated runs
	// bill their own sessions and are added on top.
	recorded := 0.0
	for _, row := range history.Models {
		if row.CostUSD != nil {
			recorded += *row.CostUSD
		}
	}
	if live.CostUSD == nil || math.Abs(*live.CostUSD-(recorded+4)) > 1e-9 || !live.CostPartial {
		t.Fatalf("live cost = %v (partial=%v), want recorded %v plus delegated 4", live.CostUSD, live.CostPartial, recorded)
	}
	// Recorded turns plus the clock of the turn still running.
	if live.ActiveMS == nil || *live.ActiveMS != 27_000 || *live.ModelMS != 23_000 {
		t.Fatalf("live timing = %v/%v, want recorded plus the uncommitted 3000ms", live.ActiveMS, live.ModelMS)
	}
}

// Durable attribution must name the model that ran the turn. A request may
// carry its own model and a run may switch models part way through; recording
// the session's configured model instead would price the wrong rates and
// disagree with the observed breakdown in the very same response.
func TestUsageModelPrefersTheModelThatRanTheTurn(t *testing.T) {
	rt := &serveRuntime{defaultModel: "runtime-default"}
	rt.sessionMeta = &session.Session{ID: "s", Model: "session-model"}

	if got := rt.usageModel(); got != "session-model" {
		t.Fatalf("usageModel without an observed model = %q, want the session's", got)
	}
	rt.startStats("requested-model")
	if got := rt.usageModel(); got != "requested-model" {
		t.Fatalf("usageModel = %q, want the model the turn ran on", got)
	}
	rt.stats.stats.SetModel("switched-model")
	if got := rt.usageModel(); got != "switched-model" {
		t.Fatalf("usageModel after a mid-run switch = %q", got)
	}
}

// Every count in the report goes through one formatter: abbreviated to read at
// a glance, exact on hover, and only when digits are actually hidden.
func TestStatsRowFormatsCounts(t *testing.T) {
	for _, tc := range []struct {
		value       any
		want, exact string
	}{
		{value: 0, want: "0"},
		{value: 999, want: "999"},
		{value: 12_100, want: "12.1K", exact: "12,100"},
		{value: 173_217, want: "173.2K", exact: "173,217"},
		// The unit is chosen after rounding, so neither side ever prints 1000K.
		{value: 955_470, want: "955.5K", exact: "955,470"},
		{value: 999_950, want: "1M", exact: "999,950"},
		{value: 80_510_668, want: "80.5M", exact: "80,510,668"},
		{value: int64(1_250_000_000), want: "1.3B", exact: "1,250,000,000"},
		{value: -4_500, want: "-4.5K", exact: "-4,500"},
		// Values that are not counts are printed as given.
		{value: "unavailable", want: "unavailable"},
		{value: "99.5%", want: "99.5%"},
	} {
		row := statsRow("Tokens", tc.value)
		if row.Value != tc.want || row.Exact != tc.exact {
			t.Fatalf("statsRow(%v) = %q/%q, want %q/%q", tc.value, row.Value, row.Exact, tc.want, tc.exact)
		}
	}
}

// Delegated work is recorded on the parent (TUI folds it into its own bucket and
// attributes it as subagent) or on the child sessions (serve leaves it there).
// Both describe the same spend, so the session total takes the larger rather
// than adding them and doubling every delegated turn.
func TestDelegatedCostIsCountedOnceAcrossBothRecordings(t *testing.T) {
	srv := &serveServer{}
	own := session.ModelUsage{Model: "opus", Kind: session.ModelUsageMain, InputTokens: 1_000, OutputTokens: 100}
	billedByParent := session.ModelUsage{Model: "haiku", Kind: session.ModelUsageSubagent, InputTokens: 500_000, OutputTokens: 50_000}
	ownCost := costOf(t, own)
	parentDelegated := costOf(t, billedByParent)

	// Serve: nothing delegated on the parent, so the child sessions supply it.
	serveSide := &serveStatsResponse{Scope: "durable_history", Models: []serveStatsModel{}}
	srv.applyDurableSessionTotals(serveSide, nil, []session.ModelUsage{own}, sessionDelegatedCost{CostUSD: 7.5}, webSessionMetrics{InputTokens: 1_000, OutputTokens: 100})
	if serveSide.CostUSD == nil || math.Abs(*serveSide.CostUSD-(ownCost+7.5)) > 1e-9 {
		t.Fatalf("serve session cost = %v, want own %v plus delegated 7.5", serveSide.CostUSD, ownCost)
	}

	// TUI: the parent already billed the delegated work, and the child sessions
	// report the same spend. It must be counted once.
	tuiSide := &serveStatsResponse{Scope: "durable_history", Models: []serveStatsModel{}}
	srv.applyDurableSessionTotals(tuiSide, nil, []session.ModelUsage{own, billedByParent}, sessionDelegatedCost{CostUSD: parentDelegated}, webSessionMetrics{InputTokens: 501_000, OutputTokens: 50_100})
	if tuiSide.CostUSD == nil || math.Abs(*tuiSide.CostUSD-(ownCost+parentDelegated)) > 1e-9 {
		t.Fatalf("tui session cost = %v, want own %v plus delegated %v counted once", tuiSide.CostUSD, ownCost, parentDelegated)
	}

	// A child that outspent what the parent attributed still reports in full.
	mixed := &serveStatsResponse{Scope: "durable_history", Models: []serveStatsModel{}}
	srv.applyDurableSessionTotals(mixed, nil, []session.ModelUsage{own, billedByParent}, sessionDelegatedCost{CostUSD: parentDelegated + 5}, webSessionMetrics{InputTokens: 501_000, OutputTokens: 50_100})
	if mixed.CostUSD == nil || math.Abs(*mixed.CostUSD-(ownCost+parentDelegated+5)) > 1e-9 {
		t.Fatalf("mixed session cost = %v, want the larger delegated figure", mixed.CostUSD)
	}
}

// costOf prices one recorded entry the way the report does.
func costOf(t *testing.T, entry session.ModelUsage) float64 {
	t.Helper()
	cost, _ := aggregateCost(entry.Model, webSessionMetrics{
		InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens,
		CachedInputTokens: entry.CachedInputTokens, CacheWriteTokens: entry.CacheWriteTokens,
	})
	if cost == nil {
		t.Fatalf("no price for %s", entry.Model)
	}
	return *cost
}

// A model that both ran the session's own turns and appeared as a delegated run
// carries one clock before the other is initialised. Accumulating into the
// missing one used to panic the whole stats request.
func TestRuntimeModelStatsTimesSharedModelWithoutPanicking(t *testing.T) {
	out := &serveStatsResponse{Models: []serveStatsModel{}}
	calls := []ui.UsageCall{{Model: "opus", InputTokens: 10, OutputTokens: 2, GenerationTime: 2 * time.Second, ObservedOutput: true}}
	runs := []ui.SubagentProgress{{ToolCallID: "child", ResolvedModel: "opus", ToolCalls: 1, Done: true}}
	appendRuntimeModelStats(out, calls, runs, map[string]bool{"child": true}, time.Now(), 0)

	row := modelUsageStatsRow(t, out.Models, "opus")
	if row.ActiveMS == nil || row.ToolMS == nil {
		t.Fatalf("shared model lost a clock: %+v", row)
	}
	if *row.ActiveMS < 2_000 {
		t.Fatalf("observed generation time missing: %+v", row.ActiveMS)
	}
}
