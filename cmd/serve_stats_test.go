package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

func TestServeStatsRuntimeStreamAndIdleSnapshot(t *testing.T) {
	p := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Text: "answer", Usage: llm.Usage{InputTokens: 11, OutputTokens: 3, CachedInputTokens: 5, CacheWriteTokens: 7}})
	rt := &serveRuntime{provider: p, engine: llm.NewEngine(p, nil), defaultModel: "gpt-5.6-luna"}
	// Observe through the actual web runtime consumer, not a parallel test-only accounting path.
	_, err := rt.RunWithEvents(context.Background(), false, false, []llm.Message{llm.UserText("hi")}, llm.Request{Model: "gpt-5.6-luna", MaxTurns: 2}, func(e llm.Event) error {
		if e.Type == llm.EventUsage {
			if got := rt.statsSnapshot(); got == nil || got.Metrics.InputTokens != 11 {
				t.Fatalf("accounting must precede delivery: %+v", got)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := rt.statsSnapshot()
	if first.Metrics != (webSessionMetrics{InputTokens: 11, OutputTokens: 3, CachedInputTokens: 5, CacheWriteTokens: 7, LLMTurns: 1}) {
		t.Fatalf("metrics %+v", first.Metrics)
	}
	if first.CostUSD == nil || first.CostPartial {
		t.Fatalf("price = %+v", first)
	}
	rt.stats.mu.Lock()
	before := *rt.stats.stats
	future := rt.stats.stats.SnapshotAt(time.Now().Add(24 * time.Hour))
	rt.stats.mu.Unlock()
	if future.LLMTime != before.LLMTime || future.ToolTime != before.ToolTime {
		t.Fatal("idle counted as activity")
	}
	for range 5 {
		_ = rt.statsSnapshot()
	}
	rt.stats.mu.Lock()
	after := *rt.stats.stats
	rt.stats.mu.Unlock()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("snapshot mutated live stats")
	}
	if first.TTFTMS == nil || first.OutputTokensPerSecond == nil {
		t.Fatal("observed output timing unavailable")
	}
}

func TestServeStatsRetryDiscardHelpersAndParallelTools(t *testing.T) {
	rt := &serveRuntime{}
	rt.startStats("gpt-5.6-luna")
	rt.recordStatsEvent(llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 10}})
	rt.recordHelperStats("compaction", "gpt-5.6-sol", llm.Usage{InputTokens: 20})
	rt.recordStatsEvent(llm.Event{Type: llm.EventAttemptDiscard})
	rt.recordStatsEvent(llm.Event{Type: llm.EventRetry, RetryWaitSecs: 3600})
	rt.stats.mu.Lock()
	before := rt.stats.stats.LLMTime
	waiting := rt.stats.stats.SnapshotAt(time.Now().Add(time.Minute))
	rt.stats.mu.Unlock()
	if waiting.LLMTime != before {
		t.Fatal("retry backoff counted")
	}
	rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecStart, ToolCallID: "a"})
	rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecStart, ToolCallID: "b"})
	rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecStart, ToolCallID: "b"})
	rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecEnd, ToolCallID: "a"})
	rt.stats.mu.Lock()
	beforeTool := rt.stats.stats.ToolTime
	during := rt.stats.stats.SnapshotAt(time.Now().Add(time.Second))
	rt.stats.mu.Unlock()
	if during.ToolTime < beforeTool+time.Second {
		t.Fatal("first parallel tool end closed whole interval")
	}
	rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecEnd, ToolCallID: "b"})
	rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecEnd, ToolCallID: "b"})
	rt.emitGuardianReview(tools.GuardianEvent{Model: "gpt-5.6-sol", Usage: llm.Usage{InputTokens: 7}}) // empty message still bills
	rt.recordHelperStats("side_question", "unpriced-model-xyz", llm.Usage{OutputTokens: 9})
	rt.finishStats()
	got := rt.statsSnapshot()
	if got.Metrics.InputTokens != 27 || got.Metrics.OutputTokens != 9 || got.Metrics.ToolCalls != 2 || got.Metrics.LLMTurns != 3 {
		t.Fatalf("metrics %+v", got.Metrics)
	}
	if got.CostUSD == nil || !got.CostPartial {
		t.Fatal("expected lower-bound price")
	}
}

func TestServeStatsSubagentBillingAndTiming(t *testing.T) {
	rt := &serveRuntime{}
	start := time.Now().Add(-10 * time.Second)
	send := func(id string, e tools.SubagentEvent) { rt.recordSubagentStats(id, e) }
	for _, id := range []string{"one", "two"} {
		send(id, tools.SubagentEvent{Type: tools.SubagentEventInit, Model: "gpt-5.6-luna", Timestamp: start})
		send(id, tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "t", Timestamp: start.Add(time.Second)})
		send(id, tools.SubagentEvent{Type: tools.SubagentEventToolEnd, ToolCallID: "t", Timestamp: start.Add(3 * time.Second)})
		send(id, tools.SubagentEvent{Type: tools.SubagentEventUsage, InputTokens: 10, CachedInputTokens: 20, CacheWriteTokens: 30, OutputTokens: 40})
		send(id, tools.SubagentEvent{Type: tools.SubagentEventGuardian, Guardian: &tools.GuardianEvent{Model: "gpt-5.6-sol", Usage: llm.Usage{InputTokens: 5, OutputTokens: 2}}})
		send(id, tools.SubagentEvent{Type: tools.SubagentEventDone, Timestamp: start.Add(5 * time.Second)})
	}
	got := rt.statsSnapshot()
	if len(got.Models) != 2 {
		t.Fatalf("models %+v", got.Models)
	}
	main, guardian := got.Models[0], got.Models[1]
	if main.Model != "gpt-5.6-luna" || main.InputTokens != 20 || main.CachedInputTokens != 40 || main.CacheWriteTokens != 60 || main.OutputTokens != 80 || main.ToolCalls != 2 || main.LLMTurns != 2 || *main.ActiveMS != 10000 || *main.ToolMS != 4000 {
		t.Fatalf("child aggregation %+v", main)
	}
	if guardian.Model != "gpt-5.6-sol" || guardian.InputTokens != 10 || guardian.ActiveMS != nil || guardian.CostUSD == nil {
		t.Fatalf("guardian attribution %+v", guardian)
	}
	if got.Metrics.InputTokens != 30 || got.Metrics.OutputTokens != 84 {
		t.Fatalf("parent totals %+v", got.Metrics)
	}
	if got.CostUSD == nil || math.Abs(*got.CostUSD-(*main.CostUSD+*guardian.CostUSD)) > 1e-12 {
		t.Fatal("per-model pricing mismatch")
	}
}

func TestServeStatsEndpointHistoricAndBusyNonmutating(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "stats.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := &session.Session{ID: "stats-history", Model: "gpt-5.6-luna", ProviderKey: "mock", InputTokens: 100, OutputTokens: 20, LLMTurns: 2, CompactionSeq: 999, CompactionCount: 1}
	if err := store.Create(ctx, meta); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "UPDATE sessions SET compaction_seq=999, compaction_count=1 WHERE id=?", meta.ID); err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{}
	rt.Touch()
	lastUsed := rt.LastUsed()
	srv := &serveServer{store: store, sessionMgr: &serveSessionManager{sessions: map[string]*serveRuntime{meta.ID: rt}}}
	read := func() serveStatsResponse {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, httptest.NewRequest("GET", "/v1/sessions/"+meta.ID+"/stats", nil))
		if rr.Code != 200 {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		var got serveStatsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	history := read()
	if history.Scope != "durable_history" || history.Metrics.InputTokens != 100 || history.ActiveMS != nil || history.CostUSD != nil || len(history.Models) != 0 || len(history.Unavailable) == 0 {
		t.Fatalf("history %+v", history)
	}
	rt.engine = llm.NewEngine(nil, nil)
	rt.engine.SetCompaction(1000, llm.CompactionConfig{SoftThresholdRatio: 0.7, HardThresholdRatio: 0.9})
	rt.engine.SetContextEstimateBaseline(123, 1)
	rt.startStats("gpt-5.6-luna")
	rt.recordStatsEvent(llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 3}})
	rt.finishStats()
	rt.mu.Lock()
	done := make(chan serveStatsResponse, 1)
	go func() { done <- read() }()
	select {
	case got := <-done:
		values := map[string]string{}
		for _, section := range got.Sections {
			if section.Title == "Current Context / Window Pressure" {
				for _, row := range section.Rows {
					values[row.Label] = row.Value
				}
			}
		}
		if values["Current context"] != "123" || values["Soft compact at"] != "700" || values["Hard compact at"] != "900" || values["Free space"] != "877" {
			t.Fatalf("live engine context sections %v", values)
		}
		if got.Scope != "runtime_local" || got.Metrics.InputTokens != 3 || got.DurableMetrics == nil || got.DurableMetrics.InputTokens != 100 {
			t.Fatalf("scope %+v", got)
		}
	case <-time.After(2 * time.Second):
		rt.mu.Unlock()
		t.Fatal("GET blocked on run mutex")
	}
	rt.mu.Unlock()
	if rt.LastUsed() != lastUsed {
		t.Fatal("GET renewed runtime TTL")
	}
	if len(srv.sessionMgr.sessions) != 1 {
		t.Fatal("GET created runtime")
	}
	stored, _ := store.Get(ctx, meta.ID)
	if stored.CompactionSeq != 999 || stored.CompactionCount != 1 {
		t.Fatal("GET repaired stale compaction metadata")
	}
	delete(srv.sessionMgr.sessions, meta.ID)
	_ = read()
	if len(srv.sessionMgr.sessions) != 0 {
		t.Fatal("historic GET created runtime")
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"POST", meta.ID, 405}, {"GET", "missing", 404}} {
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, httptest.NewRequest(tc.method, "/v1/sessions/"+tc.path+"/stats", nil))
		if rr.Code != tc.code {
			t.Fatalf("status %d expected %d", rr.Code, tc.code)
		}
	}
}

func TestServeStatsConcurrentCallbacksAndSnapshots(t *testing.T) {
	rt := &serveRuntime{}
	var wg sync.WaitGroup
	for n := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + n))
			for range 40 {
				rt.recordHelperStats("guardian", "unknown", llm.Usage{InputTokens: 1})
				rt.recordSubagentStats(id, tools.SubagentEvent{Type: tools.SubagentEventUsage, Model: "unknown", OutputTokens: 1})
				_ = rt.statsSnapshot()
			}
		}()
	}
	wg.Wait()
	if got := rt.statsSnapshot(); got.Metrics.InputTokens != 160 || got.Metrics.OutputTokens != 160 {
		t.Fatalf("lost accounting %+v", got.Metrics)
	}
}

func TestServeStatsActualWebRunnerSpawnWiring(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	dir := filepath.Join(root, "stats-child")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: stats-child\nmodel: debug:fast\nskills: none\nmax_turns: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "fast", FastModel: "fast"}}, Agents: config.AgentsConfig{SearchPaths: []string{root}}}
	oldTools := serveTools
	serveTools = "spawn_agent"
	t.Cleanup(func() { serveTools = oldTools })
	command := &cobra.Command{}
	command.Flags().String("tools", "spawn_agent", "")
	if err := command.Flags().Set("tools", "spawn_agent"); err != nil {
		t.Fatal(err)
	}
	rt, err := newServeAgentRuntime(context.Background(), serveRuntimeRequest{SessionID: "stats-parent", Provider: "debug", Model: "fast", RuntimeDir: t.TempDir()}, serveAgentRuntimeOptions{cfg: cfg, cmd: command, approval: resolvedApprovalMode{Mode: tools.ModePrompt}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	spawn := rt.toolMgr.GetSpawnAgentTool()
	if spawn == nil {
		t.Fatal("spawn not enabled")
	}
	ctx := llm.ContextWithCallID(llm.ContextWithSessionID(context.Background(), "stats-parent"), "child")
	output, err := spawn.Execute(ctx, json.RawMessage(`{"agent_name":"stats-child","prompt":"Say hello","model":"debug:fast"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.Content, `"error":`) {
		t.Fatal(output.Content)
	}
	got := rt.statsSnapshot()
	if got == nil || got.Metrics.OutputTokens == 0 || len(got.Models) != 1 || got.Models[0].Model != "fast" || got.Models[0].ActiveMS == nil {
		t.Fatalf("web spawn callback not accounted: %+v, output %s", got, output.Content)
	}
}

func TestServeStatsChildSinkDiscardAndActualModels(t *testing.T) {
	rt := &serveRuntime{}
	sink := &spawnRunSink{callID: "child", cb: rt.recordSubagentStats, model: "preview"}
	sink.Start()
	sink.SetResolvedModel("mock", "gpt-5.6-luna")
	sink.Event(llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100}})
	sink.Event(llm.Event{Type: llm.EventAttemptDiscard})
	sink.Event(llm.Event{Type: llm.EventModelSwitch, Model: "gpt-5.6-sol"})
	sink.Event(llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 5}})
	sink.CompactionUsage(&llm.CompactionResult{Model: "gpt-5.6-luna", Usage: llm.Usage{InputTokens: 2}})
	sink.Done()
	sink.Done()
	got := rt.statsSnapshot()
	if got.Metrics.InputTokens != 7 || got.Metrics.LLMTurns != 2 {
		t.Fatalf("provisional/duplicate child accounting %+v", got.Metrics)
	}
	// Pricing is independent of the preview or containing run's model.
	want := ui.NewSessionStats()
	want.AddSubagentUsageForModel("gpt-5.6-sol", 5, 0, 0, 0)
	want.AddSubagentUsageForModel("gpt-5.6-luna", 2, 0, 0, 0)
	price, err := ui.EstimateSessionStatsCost(want, "")
	if err != nil || got.CostUSD == nil || *got.CostUSD != price {
		t.Fatalf("child price %+v want %v err %v", got.CostUSD, price, err)
	}
}

func TestServeStatsHelperWiringAndRollback(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Text: "answer", Usage: llm.Usage{InputTokens: 11, OutputTokens: 3}})
	rt := &serveRuntime{providerKey: "mock", defaultModel: "gpt-5.6-luna"}
	rt.configureSideQuestionContext()
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
	events, err := rt.startSideQuestion(sideQuestionStart{Question: "question"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	rt.sideQuestion.clearHistory()
	if got := rt.statsSnapshot(); got.Metrics.InputTokens != 11 || got.Metrics.OutputTokens != 3 {
		t.Fatalf("side question accounting %+v", got)
	}
	// Helper work remains billable even if applying the compaction fails.
	rt.compactionCB = func(context.Context, *llm.CompactionResult) error { return context.Canceled }
	persistence := &serveRunPersistence{rt: rt}
	err = persistence.applyCompaction(context.Background(), &llm.CompactionResult{Model: "gpt-5.6-sol", Usage: llm.Usage{InputTokens: 20}})
	if err != context.Canceled {
		t.Fatalf("compaction error %v", err)
	}
	rt.startStats("gpt-5.6-luna")
	rt.recordStatsEvent(llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100}})
	rt.recordHelperStats("handover", "gpt-5.6-sol", llm.Usage{InputTokens: 7})
	rt.recordStatsEvent(llm.Event{Type: llm.EventAttemptDiscard})
	rt.finishStats()
	got := rt.statsSnapshot()
	if got.Metrics.InputTokens != 38 || got.Metrics.LLMTurns != 3 {
		t.Fatalf("helper rollback %+v", got.Metrics)
	}
	calls, _ := rt.stats.stats.UsageCalls()
	for _, c := range calls {
		if c.InputTokens == 100 {
			t.Fatal("discard removed a helper instead of the provisional main call")
		}
	}
}

func TestServeStatsManualCompactionWiring(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "manual.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := &session.Session{ID: "manual-stats", Provider: "mock", ProviderKey: "mock", Model: "gpt-5.6-luna"}
	if err := store.Create(ctx, meta); err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{llm.UserText("first question"), llm.AssistantText("first answer"), llm.UserText("second question"), llm.AssistantText("second answer")}
	var rows []session.Message
	for i, m := range messages {
		rows = append(rows, *session.NewMessage(meta.ID, m, i))
	}
	if err := store.ReplaceMessages(ctx, meta.ID, rows); err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Text: "Continue from the second answer.", Usage: llm.Usage{InputTokens: 20, OutputTokens: 5}})
	rt := &serveRuntime{provider: provider, providerKey: "mock", defaultModel: meta.Model, engine: llm.NewEngine(provider, nil), store: store, sessionMeta: meta, history: messages, historyPersisted: true}
	if _, err := rt.compactSession(ctx, meta.ID); err != nil {
		t.Fatal(err)
	}
	got := rt.statsSnapshot()
	if got == nil || got.Metrics.InputTokens != 20 || got.Metrics.OutputTokens != 5 || got.CostUSD == nil {
		t.Fatalf("manual compaction accounting %+v", got)
	}
}

func TestServeStatsPricesRequestsBeforeModelAggregation(t *testing.T) {
	rt := &serveRuntime{}
	for range 2 {
		rt.recordSubagentStats("child", tools.SubagentEvent{Type: tools.SubagentEventUsage, Model: "gpt-5.6-sol", InputTokens: 200000, OutputTokens: 10000})
	}
	got := rt.statsSnapshot()
	if got.CostUSD == nil || math.Abs(*got.CostUSD-2.6) > 1e-9 {
		t.Fatalf("per-request tier pricing %+v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ActiveMS != nil {
		t.Fatalf("usage without init must not fabricate timing: %+v", got.Models)
	}
	// An unknown Guardian model is never silently priced as the parent/child.
	rt.recordSubagentStats("child", tools.SubagentEvent{Type: tools.SubagentEventGuardian, Guardian: &tools.GuardianEvent{Usage: llm.Usage{InputTokens: 1}}})
	if got := rt.statsSnapshot(); !got.CostPartial || got.CostUSD == nil || math.Abs(*got.CostUSD-2.6) > 1e-9 {
		t.Fatalf("unknown Guardian fallback %+v", got)
	}
}

func TestServeStatsDiscardRetainsTimeAndRestartsAttempt(t *testing.T) {
	rt := &serveRuntime{}
	rt.startStats("gpt-5.6-luna")
	rt.recordStatsEvent(llm.Event{Type: llm.EventTextDelta, Text: "discard me"})
	before := rt.stats.stats.SnapshotAt(time.Now()).LLMTime
	rt.recordStatsEvent(llm.Event{Type: llm.EventAttemptDiscard})
	if got := rt.stats.stats.LLMTime; got < before {
		t.Fatalf("discard lost failed-attempt time: %v < %v", got, before)
	}
	// A native fallback can discard without emitting a separate retry event.
	before = rt.stats.stats.LLMTime
	if got := rt.stats.stats.SnapshotAt(time.Now().Add(time.Second)).LLMTime; got < before+time.Second {
		t.Fatalf("replacement attempt not timed: %v, baseline %v", got, before)
	}
	rt.recordStatsEvent(llm.Event{Type: llm.EventRetry, RetryWaitSecs: 3600})
	before = rt.stats.stats.LLMTime
	if got := rt.stats.stats.SnapshotAt(time.Now().Add(time.Minute)).LLMTime; got != before {
		t.Fatalf("backoff counted as model time: %v != %v", got, before)
	}
	rt.finishStats()
	if got := rt.statsSnapshot(); got.TTFTMS != nil || got.OutputTokensPerSecond != nil || got.Metrics.LLMTurns != 0 {
		t.Fatalf("discarded output contributed performance or usage: %+v", got)
	}
}

func TestServeStatsToolBoundariesDoNotInventActivity(t *testing.T) {
	for _, id := range []string{"tool", ""} {
		t.Run("id="+id, func(t *testing.T) {
			rt := &serveRuntime{}
			rt.startStats("unknown")
			rt.finishStats()
			before := *rt.stats.stats
			rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecEnd, ToolCallID: id})
			if !reflect.DeepEqual(before, *rt.stats.stats) {
				t.Fatal("unmatched end changed idle stats")
			}
			rt.startStats("unknown")
			rt.recordStatsEvent(llm.Event{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: id}})
			rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecStart, ToolCallID: id})
			rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecEnd, ToolCallID: id})
			rt.recordStatsEvent(llm.Event{Type: llm.EventDone})
			before = *rt.stats.stats
			rt.recordStatsEvent(llm.Event{Type: llm.EventToolExecEnd, ToolCallID: id})
			if !reflect.DeepEqual(before, *rt.stats.stats) || rt.stats.running {
				t.Fatal("duplicate end changed terminal stats or run stayed active")
			}
			if got := rt.statsSnapshot().Metrics.ToolCalls; got != 1 {
				t.Fatalf("duplicate start counted: %d", got)
			}
		})
	}
}

func TestServeStatsCompactionContextContract(t *testing.T) {
	for _, tc := range []struct {
		name, enabled          string
		engine                 bool
		limit                  int
		config                 *llm.CompactionConfig
		soft, hard             string
		softBuffer, hardBuffer string
	}{
		{name: "unavailable", enabled: "unavailable"},
		{name: "disabled", enabled: "false", engine: true},
		{name: "unknown window", enabled: "false", engine: true, config: &llm.CompactionConfig{}},
		{name: "enabled", enabled: "true", engine: true, limit: 1000, config: &llm.CompactionConfig{SoftThresholdRatio: 0.7, HardThresholdRatio: 0.9}, soft: "700", hard: "900", softBuffer: "300", hardBuffer: "100"},
		{name: "equal thresholds", enabled: "true", engine: true, limit: 1000, config: &llm.CompactionConfig{SoftThresholdRatio: 0.8, HardThresholdRatio: 0.8}, soft: "800", hard: "800", softBuffer: "200", hardBuffer: "200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &serveRuntime{sessionMeta: &session.Session{ID: "context"}}
			if tc.engine {
				rt.engine = llm.NewEngine(nil, nil)
				if tc.config != nil {
					rt.engine.SetCompaction(tc.limit, *tc.config)
				}
			}
			srv := &serveServer{sessionMgr: &serveSessionManager{sessions: map[string]*serveRuntime{"context": rt}}}
			rr := httptest.NewRecorder()
			srv.handleSessionStats(rr, httptest.NewRequest("GET", "/v1/sessions/context/stats", nil), "context")
			if rr.Code != 200 {
				t.Fatalf("%d: %s", rr.Code, rr.Body.String())
			}
			var got serveStatsResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			rows := map[string]string{}
			for _, section := range got.Sections {
				if section.Title == "Current Context / Window Pressure" {
					for _, row := range section.Rows {
						rows[row.Label] = row.Value
					}
				}
			}
			for label, want := range map[string]string{"Compaction enabled": tc.enabled, "Soft compact at": tc.soft, "Hard compact at": tc.hard, "Soft window buffer": tc.softBuffer, "Hard window buffer": tc.hardBuffer} {
				if rows[label] != want {
					t.Errorf("%s = %q, want %q", label, rows[label], want)
				}
			}
		})
	}
}

func TestServeStatsConcurrentDiscardAndSnapshotIsolation(t *testing.T) {
	rt := &serveRuntime{}
	rt.recordSubagentStats("child", tools.SubagentEvent{Type: tools.SubagentEventUsage, Model: "unknown", InputTokens: 7})
	retained := rt.statsSnapshot()
	want, err := json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			rt.startStats("unknown")
			rt.recordStatsEvent(llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100}})
			rt.recordHelperStats("guardian", "unknown", llm.Usage{InputTokens: 1})
			rt.recordStatsEvent(llm.Event{Type: llm.EventAttemptDiscard})
			rt.finishStats()
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			got := rt.statsSnapshot()
			got.Models[0].InputTokens = -1
			got.Sections[0].Rows[0].Value = "modified"
		}
	}()
	wg.Wait()
	if got := rt.statsSnapshot(); got.Metrics.InputTokens != 107 || got.Models[0].InputTokens != 7 {
		t.Fatalf("snapshot or rollback corrupted ledger: %+v", got)
	}
	if got, err := json.Marshal(retained); err != nil || string(got) != string(want) {
		t.Fatalf("retained snapshot changed: %s, err %v", got, err)
	}
}

func TestServeStatsConcurrentRuntimeHistorySnapshot(t *testing.T) {
	rt := &serveRuntime{sessionMeta: &session.Session{ID: "live"}, engine: llm.NewEngine(nil, nil)}
	for range 100 {
		rt.history = append(rt.history, llm.UserText("original"))
	}
	srv := &serveServer{sessionMgr: &serveSessionManager{sessions: map[string]*serveRuntime{"live": rt}}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := range 200 {
			rt.mu.Lock()
			for i := range rt.history {
				rt.history[i].Parts[0].Text = strings.Repeat("updated", n%4+1)
			}
			rt.mu.Unlock()
			rt.engine.SetContextEstimateBaseline(n+1, 1)
			rt.engine.SetCompaction(1000, llm.CompactionConfig{SoftThresholdRatio: 0.7, HardThresholdRatio: 0.9})
		}
	}()
	for range 100 {
		rr := httptest.NewRecorder()
		srv.handleSessionStats(rr, httptest.NewRequest("GET", "/v1/sessions/live/stats", nil), "live")
		if rr.Code != 200 {
			t.Errorf("%d: %s", rr.Code, rr.Body.String())
		}
	}
	wg.Wait()
}

func TestServeStatsRunningModel(t *testing.T) {
	rt := &serveRuntime{}
	rt.recordSubagentStats("live", tools.SubagentEvent{Type: tools.SubagentEventInit, Model: "test", Timestamp: time.Now()})
	if !rt.statsSnapshot().Models[0].Running {
		t.Fatal("missing live model state")
	}
	rt.recordSubagentStats("live", tools.SubagentEvent{Type: tools.SubagentEventDone, Timestamp: time.Now()})
	if rt.statsSnapshot().Models[0].Running {
		t.Fatal("completed model still running")
	}
}
