package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type timedEvent struct {
	after time.Duration
	event llm.Event
}

type scriptedTurn struct {
	streamAfter time.Duration
	events      []timedEvent
	err         error
}

type scriptedProvider struct {
	clock    *fakeClock
	turns    []scriptedTurn
	next     int
	requests []llm.Request
	resets   int
}

func (p *scriptedProvider) Name() string                   { return "scripted" }
func (p *scriptedProvider) Credential() string             { return "mock" }
func (p *scriptedProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *scriptedProvider) ResetConversation()             { p.resets++ }
func (p *scriptedProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.requests = append(p.requests, req)
	if p.next >= len(p.turns) {
		return nil, errors.New("no scripted turn")
	}
	turn := p.turns[p.next]
	p.next++
	p.clock.advance(turn.streamAfter)
	if turn.err != nil {
		return nil, turn.err
	}
	return &scriptedStream{clock: p.clock, events: turn.events}, nil
}

type scriptedStream struct {
	clock  *fakeClock
	events []timedEvent
	next   int
}

func (s *scriptedStream) Recv() (llm.Event, error) {
	if s.next >= len(s.events) {
		return llm.Event{}, io.EOF
	}
	event := s.events[s.next]
	s.next++
	s.clock.advance(event.after)
	return event.event, nil
}
func (s *scriptedStream) Close() error { return nil }

func baseOptions() Options {
	return Options{
		Mode:            "quick",
		Cache:           "cold",
		Runs:            1,
		Concurrency:     1,
		Seed:            42,
		Timeout:         time.Second,
		CacheTolerance:  0.01,
		TargetTolerance: 0.15,
	}
}

func targetFor(provider llm.Provider, reportsCache bool) Target {
	return Target{
		ProviderKey:    "test",
		ProviderType:   "test",
		RequestedModel: "model",
		Provider:       provider,
		Capabilities: AdapterCapabilities{
			ReportsCacheReads:      reportsCache,
			ReportsCacheWrites:     reportsCache,
			ReportsReasoningTokens: reportsCache,
			SupportsOutputLimit:    true,
			IncrementalStream:      true,
		},
	}
}

func TestGeneratePayloadDeterministicDistinctAndEarlyUnique(t *testing.T) {
	seedA := DeriveSeed(42, 2_000, "measured", 1, 0)
	seedB := DeriveSeed(42, 2_000, "measured", 2, 0)
	first := GeneratePayload(seedA, 256, "prefill", 16, true)
	repeated := GeneratePayload(seedA, 256, "prefill", 16, true)
	second := GeneratePayload(seedB, 256, "prefill", 16, true)
	if first.Text != repeated.Text || first.SHA256 != repeated.SHA256 {
		t.Fatal("same seed did not reproduce payload")
	}
	if first.SHA256 == second.SHA256 {
		t.Fatal("different scenario dimensions reused payload")
	}
	prefixA := first.Text[:min(64, len(first.Text))]
	prefixB := second.Text[:min(64, len(second.Text))]
	if prefixA == prefixB {
		t.Fatalf("payload uniqueness was not early: prefix %q", prefixA)
	}
	if strings.Contains(first.Text, "http://") || strings.Contains(first.Text, "https://") {
		t.Fatal("payload unexpectedly contains URL")
	}
}

func TestGeneratePayloadUnsupportedOutputLimitUsesBoundedNaturalInstruction(t *testing.T) {
	for _, workload := range []string{"decode", "prefill"} {
		payload := GeneratePayload(0x1234, 256, workload, 128, false)
		other := GeneratePayload(0x5678, 256, workload, 128, false)
		marker := "BENCHMARK-END-0000000000001234"
		otherMarker := "BENCHMARK-END-0000000000005678"

		for _, phrase := range []string{
			"one concise paragraph",
			"approximately 128 words",
			"End the paragraph with the exact marker " + marker,
			"Do not repeat words or phrases mechanically",
			"do not repeat the marker",
			"do not write or continue anything after it",
		} {
			if !strings.Contains(payload.Text, phrase) {
				t.Errorf("%s fallback prompt missing %q: %q", workload, phrase, payload.Text)
			}
		}
		if strings.Count(payload.Text, marker) != 1 || strings.Contains(payload.Text, otherMarker) || !strings.Contains(other.Text, otherMarker) {
			t.Errorf("%s fallback markers are not unique: first=%q second=%q", workload, payload.Text, other.Text)
		}
		lower := strings.ToLower(payload.Text)
		for _, repetitive := range []string{"occurrences of", "separated by single spaces", "exactly 128"} {
			if strings.Contains(lower, repetitive) {
				t.Errorf("%s fallback prompt demands a repetitive exact run via %q: %q", workload, repetitive, payload.Text)
			}
		}
	}
}

func TestNormalizeUsageIncludesCacheWritesAsComputedInput(t *testing.T) {
	usage := normalizeUsage(llm.Usage{InputTokens: 12, CacheWriteTokens: 127_000, CachedInputTokens: 0, OutputTokens: 3}, AdapterCapabilities{ReportsCacheReads: true, ReportsCacheWrites: true})
	if usage.TotalInputTokens != 127_012 || usage.ComputedInputTokens != 127_012 {
		t.Fatalf("normalized usage = total %d computed %d, want 127012/127012", usage.TotalInputTokens, usage.ComputedInputTokens)
	}
	if usage.TokenCountSource != "normalized_components" {
		t.Fatalf("source = %q", usage.TokenCountSource)
	}
	if usage.CacheWriteTokens == nil || *usage.CacheWriteTokens != 127_000 {
		t.Fatalf("cache-write metric = %v, want 127000", usage.CacheWriteTokens)
	}
	unsupported := normalizeUsage(llm.Usage{InputTokens: 12, CachedInputTokens: 99, CacheWriteTokens: 88, ReasoningTokens: 7}, AdapterCapabilities{})
	if unsupported.CachedInputTokens != nil || unsupported.CacheWriteTokens != nil || unsupported.ReasoningTokens != nil {
		t.Fatalf("unsupported provider metrics must be null: %#v", unsupported)
	}
}

func TestMeasureActivityVisibleTTFTAndReasoningDecode(t *testing.T) {
	clock := newFakeClock()
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{{
		streamAfter: 10 * time.Millisecond,
		events: []timedEvent{
			{after: 20 * time.Millisecond, event: llm.Event{Type: llm.EventReasoningDelta, Text: "thinking"}},
			{after: 30 * time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "a"}},
			{after: 40 * time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "b"}},
			{after: 5 * time.Millisecond, event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 8, ReasoningTokens: 2}}},
			{after: 5 * time.Millisecond, event: llm.Event{Type: llm.EventDone}},
		},
	}}}
	record := (Runner{Clock: clock}).measure(context.Background(), targetFor(provider, true), baseOptions(), measureSpec{
		phase: "measured", workload: "decode", run: 1, attempt: 1, inputTarget: 100, outputTarget: 8, estimateTarget: 100,
	})
	assertFloat(t, record.Timing.ActivityTTFTMS, 30)
	assertFloat(t, record.Timing.VisibleTTFTMS, 60)
	assertFloat(t, record.Timing.ObservedDecodeMS, 70)
	assertFloat(t, record.Timing.DecodeTokensPerSec, 100)
	assertFloat(t, record.Timing.TPOTMS, 10)
	assertFloat(t, record.Timing.EndToEndMS, 110)
	if record.DecodeStatus != "observed_incremental" || !record.ReasoningObserved || !record.Success {
		t.Fatalf("record status = %#v", record)
	}
}

func TestMeasureHiddenReasoningUsesVisibleWindowOnly(t *testing.T) {
	clock := newFakeClock()
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{{events: []timedEvent{
		{after: 10 * time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "a"}},
		{after: 100 * time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "b"}},
		{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 10, ReasoningTokens: 6}}},
		{event: llm.Event{Type: llm.EventDone}},
	}}}}
	record := (Runner{Clock: clock}).measure(context.Background(), targetFor(provider, true), baseOptions(), measureSpec{
		phase: "measured", workload: "decode", inputTarget: 100, outputTarget: 10, estimateTarget: 100,
	})
	if record.Timing.DecodeTokensPerSec != nil {
		t.Fatalf("hidden reasoning produced all-output decode rate %v", *record.Timing.DecodeTokensPerSec)
	}
	assertFloat(t, record.Timing.VisibleTokensPerSec, 30)
	if record.DecodeStatus != "visible_only_hidden_reasoning_excluded" {
		t.Fatalf("decode status = %q", record.DecodeStatus)
	}
}

func TestMeasureLateReasoningDoesNotSpanObservedWindow(t *testing.T) {
	clock := newFakeClock()
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{{events: []timedEvent{
		{after: 10 * time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "a"}},
		{after: 20 * time.Millisecond, event: llm.Event{Type: llm.EventReasoningDelta, Text: "late summary"}},
		{after: 30 * time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "b"}},
		{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 10, ReasoningTokens: 6}}},
		{event: llm.Event{Type: llm.EventDone}},
	}}}}
	record := (Runner{Clock: clock}).measure(context.Background(), targetFor(provider, true), baseOptions(), measureSpec{
		phase: "measured", workload: "decode", inputTarget: 100, outputTarget: 10, estimateTarget: 100,
	})
	if !record.ReasoningObserved || record.ObservableReasoningWindow || record.Timing.DecodeTokensPerSec != nil {
		t.Fatalf("late reasoning treated as full observable window: %#v", record)
	}
	assertFloat(t, record.Timing.VisibleTokensPerSec, 60)
}

func TestMeasureNonIncrementalStreamLeavesDecodeNull(t *testing.T) {
	clock := newFakeClock()
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{{events: []timedEvent{
		{after: time.Second, event: llm.Event{Type: llm.EventTextDelta, Text: "whole response"}},
		{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 100}}},
		{event: llm.Event{Type: llm.EventDone}},
	}}}}
	record := (Runner{Clock: clock}).measure(context.Background(), targetFor(provider, true), baseOptions(), measureSpec{
		phase: "measured", workload: "decode", inputTarget: 100, outputTarget: 100, estimateTarget: 100,
	})
	if record.DecodeStatus != "non_incremental" || record.Timing.DecodeTokensPerSec != nil || record.Timing.TPOTMS != nil {
		t.Fatalf("non-incremental decode metrics were not null: %#v", record.Timing)
	}
}

func TestMeasureAcceptsIntentionalResponsesOutputLimitWithValidUsage(t *testing.T) {
	clock := newFakeClock()
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{{events: []timedEvent{
		{after: time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "a"}},
		{after: time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "b"}},
		{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 8}}},
		{event: llm.Event{Type: llm.EventError, Err: &llm.ResponsesIncompleteError{Reason: "max_output_tokens"}}},
	}}}}
	record := (Runner{Clock: clock}).measure(context.Background(), targetFor(provider, true), baseOptions(), measureSpec{
		phase: "measured", workload: "decode", inputTarget: 100, outputTarget: 8, estimateTarget: 100,
	})
	if !record.Success || !record.TerminalReceived || !record.OutputLimitReached || record.Error != "" {
		t.Fatalf("intentional output stop = %#v", record)
	}

	provider.turns = append(provider.turns, scriptedTurn{events: []timedEvent{
		{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 7}}},
		{event: llm.Event{Type: llm.EventError, Err: &llm.ResponsesIncompleteError{Reason: "max_output_tokens"}}},
	}})
	record = (Runner{Clock: clock}).measure(context.Background(), targetFor(provider, true), baseOptions(), measureSpec{
		phase: "measured", workload: "decode", inputTarget: 100, outputTarget: 8, estimateTarget: 100,
	})
	if record.Success || record.Error == "" {
		t.Fatalf("mismatched incomplete usage was accepted: %#v", record)
	}
}

func TestMeasureUnsupportedOutputLimitUsesBoundedPromptAndProviderUsage(t *testing.T) {
	clock := newFakeClock()
	events := make([]timedEvent, 0, 12)
	for range 10 {
		events = append(events, timedEvent{after: time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "natural output "}})
	}
	events = append(events,
		timedEvent{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 13}}},
		timedEvent{event: llm.Event{Type: llm.EventDone}},
	)
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{{events: events}}}
	target := targetFor(provider, true)
	target.Capabilities.SupportsOutputLimit = false
	record := (Runner{Clock: clock}).measure(context.Background(), target, baseOptions(), measureSpec{
		phase: "measured", workload: "decode", inputTarget: 100, outputTarget: 8, estimateTarget: 100, seed: 0x1234,
	})
	if !record.Success || !record.TerminalReceived || record.OutputLimitStatus != "unsupported_not_enforced" {
		t.Fatalf("record = %#v", record)
	}
	if record.ActivityEvents != 10 {
		t.Fatalf("stream stopped after %d events, want all 10 events consumed through provider completion", record.ActivityEvents)
	}
	if got := provider.requests[0].MaxOutputTokens; got != 0 {
		t.Fatalf("unsupported MaxOutputTokens sent to adapter: %d", got)
	}
	if record.Usage.OutputTokens != 13 {
		t.Fatalf("provider output usage = %d, want authoritative actual usage 13", record.Usage.OutputTokens)
	}
	assertFloat(t, record.Timing.DecodeTokensPerSec, float64(13-1)/(9*time.Millisecond).Seconds())
	prompt := provider.requests[0].Messages[0].Parts[0].Text
	if !strings.Contains(prompt, "one concise paragraph") || !strings.Contains(prompt, "approximately 8 words") || !strings.Contains(prompt, "BENCHMARK-END-0000000000001234") {
		t.Fatalf("bounded natural output instruction missing: %q", prompt)
	}
	if strings.Contains(prompt, "occurrences of") || strings.Contains(prompt, "separated by single spaces") {
		t.Fatalf("unsupported output instruction demands a repetitive run: %q", prompt)
	}
	report := NewReport(baseOptions(), Budget{}, []Target{target}, []RunRecord{record})
	if len(report.Targets) != 1 || report.Targets[0].OutputLimitMeaning != "requested_but_adapter_does_not_enforce" {
		t.Fatalf("output-limit metadata = %#v", report.Targets)
	}
}

func TestRunnerManagedQuickTargetsGeneratedPayloadNotProviderTotal(t *testing.T) {
	clock := newFakeClock()
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{{events: []timedEvent{
		{after: time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "amber"}},
		{after: time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: " amber"}},
		{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{
			InputTokens: 12_000, CachedInputTokens: 500, CacheWriteTokens: 8_000,
			ProviderRawInputTokens: 20_500, OutputTokens: 8,
		}}},
		{event: llm.Event{Type: llm.EventDone}},
	}}}}
	target := Target{
		ProviderKey: "claude-bin", ProviderType: "claude-bin", RequestedModel: "haiku", Provider: provider,
		Capabilities: AdapterCapabilities{
			ReportsCacheWrites: true, IncrementalStream: false, ManagedContext: true,
			MeasurementScope: "subprocess_inclusive",
		},
	}
	opts := baseOptions()
	opts.Scenarios = []Scenario{{InputTokens: 2_000, OutputTokens: 8, Workload: "decode"}}
	records, err := (Runner{Clock: clock}).RunTarget(context.Background(), target, opts)
	if err != nil {
		t.Fatalf("managed quick benchmark failed on provider overhead: %v", err)
	}
	if len(records) != 1 || len(provider.requests) != 1 {
		t.Fatalf("managed quick performed calibration requests: records=%d requests=%d", len(records), len(provider.requests))
	}
	record := records[0]
	if record.Phase != "measured" || record.InputTargetMeaning != "generated_user_payload" || math.Abs(float64(record.LocalEstimate-2_000)) > 2 {
		t.Fatalf("payload target record = %#v", record)
	}
	if record.Usage.TotalInputTokens != 20_500 || record.TargetMatched != nil || !record.LatencyEligible || record.FitEligible {
		t.Fatalf("provider-total usage incorrectly target-gated: %#v", record)
	}
	if record.ReasoningExpected || record.Timing.DecodeTokensPerSec != nil || record.Timing.VisibleTokensPerSec != nil || record.Timing.TPOTMS != nil || record.Timing.ObservedDecodeMS != nil || record.DecodeStatus != "non_comparable_delivery" {
		t.Fatalf("managed CLI delivery produced comparable decode metrics = %#v", record)
	}
	assertFloat(t, record.Timing.EndToEndMS, 2)
	if provider.requests[0].MaxOutputTokens != 0 {
		t.Fatalf("managed unsupported output limit sent: %d", provider.requests[0].MaxOutputTokens)
	}

	report := NewReport(opts, Budget{}, []Target{target}, records)
	if len(report.Aggregates) != 1 || report.Aggregates[0].TotalInputTokens == nil || *report.Aggregates[0].TotalInputTokens != 20_500 || report.Aggregates[0].OutputTokens == nil || *report.Aggregates[0].OutputTokens != 8 {
		t.Fatalf("actual provider usage not aggregated: %#v", report.Aggregates)
	}
	if report.Aggregates[0].DecodeValidRuns != 0 || report.Aggregates[0].DecodeTokensPerSecond.Median != nil || report.Aggregates[0].TPOTMS.Median != nil || report.Aggregates[0].EndToEndMS.Median == nil {
		t.Fatalf("managed aggregate exposed decode throughput or lost E2E timing: %#v", report.Aggregates[0])
	}
	limitations := strings.Join(report.Limitations, " ")
	if !strings.Contains(limitations, "generated user payload") || !strings.Contains(limitations, "cache buckets shift") || !strings.Contains(limitations, "actual provider totals remain recorded") || !strings.Contains(limitations, "provider decode throughput and TPOT are unavailable") || !strings.Contains(limitations, "Provider-reported output usage and end-to-end timing remain recorded") {
		t.Fatalf("managed payload limitation missing: %v", report.Limitations)
	}
	budget := ComputeBudgetForTarget(target, opts.Mode, opts.Scenarios, opts.Runs, opts.Warmups, true)
	if budget.IncludesCalibration || budget.MaximumRequests != 2 {
		t.Fatalf("managed quick budget still includes calibration: %#v", budget)
	}
}

func TestMeasureResetsConversationAndUsesUniqueSessionPerRequest(t *testing.T) {
	clock := newFakeClock()
	turn := scriptedTurn{events: []timedEvent{
		{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 1}}},
		{event: llm.Event{Type: llm.EventDone}},
	}}
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{turn, turn}}
	for i := 0; i < 2; i++ {
		record := (Runner{Clock: clock}).measure(context.Background(), targetFor(provider, true), baseOptions(), measureSpec{
			phase: "measured", workload: "prefill", inputTarget: 100, outputTarget: 1, estimateTarget: 100, seed: int64(i + 1),
		})
		if !record.Success {
			t.Fatalf("request %d failed: %#v", i, record)
		}
	}
	if provider.resets != 2 || len(provider.requests) != 2 {
		t.Fatalf("resets=%d requests=%d", provider.resets, len(provider.requests))
	}
	if provider.requests[0].SessionID == "" || provider.requests[0].SessionID == provider.requests[1].SessionID {
		t.Fatalf("session IDs = %q, %q", provider.requests[0].SessionID, provider.requests[1].SessionID)
	}
	if !provider.requests[0].Ephemeral || !provider.requests[1].Ephemeral {
		t.Fatal("benchmark requests were not ephemeral")
	}
}

func TestRunnerStopsBeforeMeasuredRequestsWhenCalibrationValidationDetectsTruncation(t *testing.T) {
	clock := newFakeClock()
	turn := func(input int) scriptedTurn {
		return scriptedTurn{events: []timedEvent{
			{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: input, OutputTokens: 1}}},
			{event: llm.Event{Type: llm.EventDone}},
		}}
	}
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{turn(100), turn(50)}}
	opts := baseOptions()
	opts.Scenarios = []Scenario{{InputTokens: 100, OutputTokens: 1, Workload: "prefill"}}
	records, err := (Runner{Clock: clock}).RunTarget(context.Background(), targetFor(provider, false), opts)
	if err == nil || !strings.Contains(err.Error(), "possible context truncation") {
		t.Fatalf("calibration error = %v", err)
	}
	if len(records) != 2 || provider.next != 2 {
		t.Fatalf("records=%d provider requests=%d", len(records), provider.next)
	}
}

func TestCacheCapabilityAndTargetMismatchControlFitEligibility(t *testing.T) {
	unknown := RunRecord{Success: true, TerminalReceived: true, UsageReceived: true, RequestedInput: 100, Usage: UsageRecord{TotalInputTokens: 100, ComputedInputTokens: 100}}
	classifyCacheAndTarget(&unknown, Target{Capabilities: AdapterCapabilities{ReportsCacheReads: false}}, baseOptions())
	finalizeEligibility(&unknown, Target{Capabilities: AdapterCapabilities{ReportsCacheReads: false}}, baseOptions(), "measured")
	if unknown.CacheState != "unknown" || unknown.FitEligible {
		t.Fatalf("unknown cache record = %#v", unknown)
	}

	known := RunRecord{Success: true, TerminalReceived: true, UsageReceived: true, RequestedInput: 100, Usage: UsageRecord{TotalInputTokens: 100, ComputedInputTokens: 100}}
	knownTarget := Target{Capabilities: AdapterCapabilities{ReportsCacheReads: true}, AssumedContextLimit: 128_000}
	classifyCacheAndTarget(&known, knownTarget, baseOptions())
	finalizeEligibility(&known, knownTarget, baseOptions(), "measured")
	if known.CacheState != "miss" || !known.FitEligible {
		t.Fatalf("known cache record = %#v", known)
	}

	mismatch := RunRecord{Success: true, TerminalReceived: true, UsageReceived: true, RequestedInput: 100, Usage: UsageRecord{TotalInputTokens: 50, ComputedInputTokens: 50}}
	classifyCacheAndTarget(&mismatch, knownTarget, baseOptions())
	finalizeEligibility(&mismatch, knownTarget, baseOptions(), "measured")
	if mismatch.TargetMatched == nil || *mismatch.TargetMatched || mismatch.LatencyEligible {
		t.Fatalf("target mismatch was accepted: %#v", mismatch)
	}
	if !strings.Contains(strings.Join(mismatch.ExclusionReasons, " "), "truncation") {
		t.Fatalf("mismatch reasons = %v", mismatch.ExclusionReasons)
	}
}

func TestRunnerRetriesCacheContaminationOnceWithFreshPayload(t *testing.T) {
	clock := newFakeClock()
	turn := func(input, cached int) scriptedTurn {
		return scriptedTurn{events: []timedEvent{
			{after: time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "a"}},
			{after: time.Millisecond, event: llm.Event{Type: llm.EventTextDelta, Text: "b"}},
			{event: llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: input - cached, CachedInputTokens: cached, ProviderRawInputTokens: input, OutputTokens: 3}}},
			{event: llm.Event{Type: llm.EventDone}},
		}}
	}
	provider := &scriptedProvider{clock: clock, turns: []scriptedTurn{turn(100, 0), turn(100, 0), turn(100, 50), turn(100, 0)}}
	opts := baseOptions()
	opts.Scenarios = []Scenario{{InputTokens: 100, OutputTokens: 3, Workload: "prefill"}}
	records, err := (Runner{Clock: clock}).RunTarget(context.Background(), targetFor(provider, true), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || records[2].Attempt != 1 || records[3].Attempt != 2 {
		t.Fatalf("records = %#v", records)
	}
	if records[2].PayloadSHA256 == records[3].PayloadSHA256 {
		t.Fatal("cache retry reused payload")
	}
	if records[3].RetryReason != "cold_cache_contaminated" {
		t.Fatalf("retry reason = %q", records[3].RetryReason)
	}
	aggregates := AggregateRuns(records)
	if len(aggregates) != 1 || aggregates[0].MeasuredRuns != 1 || aggregates[0].FitValidRuns != 1 {
		t.Fatalf("aggregates = %#v", aggregates)
	}
}

func TestSmallSampleSummaryAndFitMinimums(t *testing.T) {
	small := summarize([]float64{1, 2, 3, 100})
	if small.P95 != nil || small.Label != "min_median_max" || small.Median == nil || *small.Median != 2.5 {
		t.Fatalf("small summary = %#v", small)
	}
	large := summarize([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	if large.P95 == nil || *large.P95 != 10 {
		t.Fatalf("large summary = %#v", large)
	}

	record := func(input int, ttft float64) RunRecord {
		return RunRecord{Provider: "p", RequestedModel: "m", Phase: "measured", Workload: "prefill", Run: 1, Attempt: 1, RequestedInput: input, RequestedOutput: 16, FitEligible: true, Usage: UsageRecord{ComputedInputTokens: input}, Timing: TimingRecord{ActivityTTFTMS: floatPtr(ttft)}}
	}
	two := []RunRecord{record(1_000, 100), record(4_000, 400)}
	if fits := ComputeFits(two); len(fits) != 0 {
		t.Fatalf("fit emitted from two lengths: %#v", fits)
	}
	three := append(two, record(15_000, 1_500))
	fits := ComputeFits(three)
	if len(fits) != 1 || fits[0].Range != "1K-16K" || fits[0].DistinctLengths != 3 {
		t.Fatalf("three-length fits = %#v", fits)
	}
}

func TestAggregatesUseLatestAttemptAndEligibleTokenMedians(t *testing.T) {
	matched := true
	record := func(run, attempt, order, input int, eligible bool) RunRecord {
		return RunRecord{
			Provider: "p", RequestedModel: "m", Phase: "measured", Workload: "prefill",
			Run: run, Attempt: attempt, Order: order, RequestedInput: 1_000, RequestedOutput: 16,
			Success: true, UsageReceived: true, LatencyEligible: eligible, FitEligible: eligible, TargetMatched: &matched,
			Usage: UsageRecord{TotalInputTokens: input, ComputedInputTokens: input, OutputTokens: input / 10},
		}
	}
	records := []RunRecord{
		record(1, 1, 1, 100, true),
		record(1, 2, 2, 9_000, false),
		record(1, 2, 3, 1_000, true), // latest order wins when attempt metadata ties
		record(2, 1, 4, 3_000, false),
	}
	aggregates := AggregateRuns(records)
	if len(aggregates) != 1 {
		t.Fatalf("aggregates = %#v", aggregates)
	}
	got := aggregates[0]
	if got.MeasuredRuns != 2 || got.FitValidRuns != 1 || got.ComputedInputTokens == nil || *got.ComputedInputTokens != 1_000 {
		t.Fatalf("aggregate = %#v", got)
	}
	if got.OutputTokens == nil || *got.OutputTokens != 100 {
		t.Fatalf("eligible output median = %v", got.OutputTokens)
	}
}

func TestFitRangesAreHalfOpenAtSharedBoundaries(t *testing.T) {
	record := func(input int) RunRecord {
		return RunRecord{Provider: "p", RequestedModel: "m", Phase: "measured", Workload: "prefill", Run: input, Attempt: 1, RequestedInput: input, RequestedOutput: 16, FitEligible: true, Usage: UsageRecord{ComputedInputTokens: input}, Timing: TimingRecord{ActivityTTFTMS: floatPtr(float64(input) / 10)}}
	}
	records := []RunRecord{
		record(1_000), record(4_000), record(15_000),
		record(16_000), record(32_000), record(63_000),
	}
	fits := ComputeFits(records)
	if len(fits) != 2 || fits[0].Range != "1K-16K" || fits[0].DistinctLengths != 3 || fits[1].Range != "16K-64K" || fits[1].DistinctLengths != 3 {
		t.Fatalf("fits = %#v", fits)
	}
}

func TestWarmupsExcludedAndJSONHasNullMetricsWithoutPayload(t *testing.T) {
	records := []RunRecord{
		{Provider: "p", RequestedModel: "m", Phase: "warmup", RequestedInput: 100, RequestedOutput: 10, Success: true},
		{Provider: "p", RequestedModel: "m", Phase: "measured", Workload: "decode", Run: 1, Attempt: 1, RequestedInput: 100, RequestedOutput: 10, Success: true},
	}
	aggregates := AggregateRuns(records)
	if len(aggregates) != 1 || aggregates[0].MeasuredRuns != 1 {
		t.Fatalf("warmup entered aggregate: %#v", aggregates)
	}
	records[1].PayloadSHA256 = strings.Repeat("a", 64)
	encoded, err := json.Marshal(records[1])
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "benchmark-id") || strings.Contains(text, "authorization") {
		t.Fatalf("JSON leaked prompt or secret-shaped field: %s", text)
	}
	if !strings.Contains(text, `"decode_tokens_per_second":null`) {
		t.Fatalf("unavailable metric was not JSON null: %s", text)
	}
}

func TestReportRecordsAssumedContextLimitAndProminentWarning(t *testing.T) {
	opts := baseOptions()
	opts.AssumedContextLimit = 128_000
	target := targetFor(nil, false)
	target.ProviderKey = "ollama"
	target.ProviderType = "ollama"
	target.RequestedModel = "local-model"
	target.AssumedContextLimit = 128_000

	report := NewReport(opts, Budget{}, []Target{target}, nil)
	if report.Benchmark.AssumedContextLimit != 128_000 || len(report.Targets) != 1 || report.Targets[0].AssumedContextLimit != 128_000 {
		t.Fatalf("assumed context metadata = %#v", report)
	}
	limitations := strings.Join(report.Limitations, " ")
	if !strings.Contains(limitations, "EXPERT OVERRIDE") || !strings.Contains(limitations, "num_ctx") || !strings.Contains(limitations, "truncated requests are invalid") {
		t.Fatalf("limitations = %v", report.Limitations)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), `"assumed_context_limit":128000`) < 2 {
		t.Fatalf("JSON does not record benchmark and target assumed limits: %s", encoded)
	}
	var human strings.Builder
	WriteHuman(&human, report)
	if !strings.Contains(human.String(), "WARNING: EXPERT OVERRIDE") || !strings.Contains(human.String(), "Assumed context limit: 128000 tokens") {
		t.Fatalf("human report = %s", human.String())
	}
}

func TestMeasureHardTimeout(t *testing.T) {
	provider := timeoutProvider{}
	opts := baseOptions()
	opts.Timeout = time.Nanosecond
	record := (Runner{}).measure(context.Background(), targetFor(provider, false), opts, measureSpec{
		phase: "measured", workload: "prefill", inputTarget: 100, outputTarget: 1, estimateTarget: 100,
	})
	if !record.TimedOut || record.Error != context.DeadlineExceeded.Error() || record.Success {
		t.Fatalf("timeout record = %#v", record)
	}
}

type timeoutProvider struct{}

func (timeoutProvider) Name() string                   { return "timeout" }
func (timeoutProvider) Credential() string             { return "mock" }
func (timeoutProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (timeoutProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func assertFloat(t *testing.T, actual *float64, expected float64) {
	t.Helper()
	if actual == nil || math.Abs(*actual-expected) > 1e-9 {
		if actual == nil {
			t.Fatalf("value = nil, want %v", expected)
		}
		t.Fatalf("value = %v, want %v", *actual, expected)
	}
}
