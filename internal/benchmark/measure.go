package benchmark

import (
	"context"
	"errors"
	"io"
	"math"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

type measureSpec struct {
	phase          string
	workload       string
	run            int
	attempt        int
	order          int
	seed           int64
	inputTarget    int
	outputTarget   int
	estimateTarget int
	retryReason    string
}

func (r Runner) measure(ctx context.Context, target Target, opts Options, spec measureSpec) RunRecord {
	clock := r.clock()
	payload := GeneratePayload(spec.seed, spec.estimateTarget, spec.workload, spec.outputTarget, target.Capabilities.SupportsOutputLimit)
	record := RunRecord{
		Provider:           target.ProviderKey,
		ProviderType:       target.ProviderType,
		RequestedModel:     target.RequestedModel,
		Phase:              spec.phase,
		Workload:           spec.workload,
		Run:                spec.run,
		Attempt:            spec.attempt,
		Order:              spec.order,
		Seed:               spec.seed,
		PayloadSHA256:      payload.SHA256,
		PayloadBytes:       payload.Bytes,
		PayloadWords:       payload.Words,
		RequestedInput:     spec.inputTarget,
		InputTargetMeaning: inputTargetMeaning(target, opts.Mode),
		LocalEstimate:      payload.Estimated,
		RequestedOutput:    spec.outputTarget,
		OutputLimitStatus:  "enforced_by_adapter",
		ReasoningEffort:    opts.ReasoningEffort,
		ReasoningExpected:  target.ReasoningExpected || opts.ReasoningEffort != "",
		ServiceTier:        target.ServiceTier,
		ReportsCacheReads:  target.Capabilities.ReportsCacheReads,
		MeasurementScope:   target.Capabilities.MeasurementScope,
		RetryPolicy:        "provider retries disabled; benchmark may retry once with a fresh payload after cache contamination",
		RetryReason:        spec.retryReason,
		CacheState:         "unknown",
		DecodeStatus:       "unavailable",
	}
	if !target.Capabilities.SupportsOutputLimit {
		record.OutputLimitStatus = "unsupported_not_enforced"
	}
	if opts.TemperatureSet && target.Capabilities.SupportsTemperature {
		value := opts.Temperature
		record.Temperature = &value
	}

	req := llm.Request{
		Model:           target.RequestedModel,
		SessionID:       "benchmark-" + payload.SHA256,
		Ephemeral:       true,
		Messages:        []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: payload.Text}}}},
		ReasoningEffort: opts.ReasoningEffort,
		ServiceTier:     target.ServiceTier,
		ServiceTierSet:  target.ServiceTier != "",
		Temperature:     opts.Temperature,
		TemperatureSet:  opts.TemperatureSet && target.Capabilities.SupportsTemperature,
	}
	if target.Capabilities.SupportsOutputLimit {
		req.MaxOutputTokens = spec.outputTarget
	}

	requestCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Benchmarks are independent requests, never turns in a provider-managed
	// conversation. Reset any connection-local Responses continuation before the
	// timed boundary; SessionID is also unique so server routing cannot join runs.
	if resetter, ok := target.Provider.(interface{ ResetConversation() }); ok {
		resetter.ResetConversation()
	}

	tStart := clock.Now()
	record.StartedAt = tStart
	stream, err := target.Provider.Stream(requestCtx, req)
	if err != nil {
		tEnd := clock.Now()
		setEndToEnd(&record, tStart, tEnd)
		classifyRunError(&record, requestCtx, err)
		finalizeEligibility(&record, target, opts, spec.phase)
		return record
	}
	defer stream.Close()

	var (
		firstActivity        *time.Time
		firstText            *time.Time
		lastActivity         *time.Time
		lastText             *time.Time
		terminalTime         *time.Time
		firstActivityWasText bool
	)

	for {
		event, recvErr := stream.Recv()
		now := clock.Now()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				if !record.TerminalReceived {
					record.Error = "stream ended without terminal completion"
				}
			} else {
				classifyRunError(&record, requestCtx, recvErr)
			}
			terminalTime = &now
			break
		}

		switch event.Type {
		case llm.EventTextDelta:
			if event.Text != "" {
				record.ActivityEvents++
				record.VisibleTextEvents++
				if firstActivity == nil {
					firstActivityWasText = true
				}
				setFirst(&firstActivity, now)
				setFirst(&firstText, now)
				lastActivity = timePtr(now)
				lastText = timePtr(now)
			}
		case llm.EventReasoningDelta:
			if event.Text != "" || event.ReasoningEncryptedContent != "" {
				record.ActivityEvents++
				record.ReasoningObserved = true
				setFirst(&firstActivity, now)
				lastActivity = timePtr(now)
			}
		case llm.EventUsage:
			if event.Use != nil {
				record.UsageReceived = true
				record.Usage = normalizeUsage(*event.Use, target.Capabilities)
			}
		case llm.EventRetry:
			record.RetryEvents++
			record.RetryWaitSeconds += event.RetryWaitSecs
		case llm.EventAttemptDiscard:
			record.AttemptDiscards++
		case llm.EventModelSwitch:
			record.ObservedModels = append(record.ObservedModels, ModelSwitch{Model: event.Model, ReasoningEffort: event.ReasoningEffort})
		case llm.EventError:
			if intentionalOutputLimitStop(event.Err, record) {
				record.OutputLimitReached = true
				record.OutputLimitStatus = "reached_intentionally"
				record.TerminalReceived = true
			} else if event.Err != nil {
				classifyRunError(&record, requestCtx, event.Err)
			} else {
				record.Error = "provider stream error"
			}
			terminalTime = &now
		case llm.EventDone:
			record.TerminalReceived = true
			terminalTime = &now
		}
		if terminalTime != nil {
			break
		}
	}

	if terminalTime == nil {
		now := clock.Now()
		terminalTime = &now
	}
	setEndToEnd(&record, tStart, *terminalTime)
	if firstActivity != nil {
		record.Timing.ActivityTTFTMS = durationMS(*firstActivity, tStart)
	}
	if firstText != nil {
		record.Timing.VisibleTTFTMS = durationMS(*firstText, tStart)
	}
	record.ObservableReasoningWindow = record.ReasoningObserved && !firstActivityWasText
	if target.Capabilities.IncrementalStream {
		deriveDecodeMetrics(&record, firstActivity, lastActivity, firstText, lastText, record.ReasoningExpected)
	} else {
		record.DecodeStatus = "non_comparable_delivery"
	}
	classifyCacheAndTarget(&record, target, opts)
	record.Success = record.Error == "" && record.TerminalReceived
	finalizeEligibility(&record, target, opts, spec.phase)
	return record
}

func normalizeUsage(usage llm.Usage, capabilities AdapterCapabilities) UsageRecord {
	total := usage.ProviderRawInputTokens
	source := "provider_raw"
	if total <= 0 {
		total = usage.InputTokens + usage.CachedInputTokens + usage.CacheWriteTokens
		source = "normalized_components"
	}
	if total <= 0 {
		source = "unavailable"
	}
	record := UsageRecord{
		DirectInputTokens:   usage.InputTokens,
		TotalInputTokens:    total,
		ComputedInputTokens: usage.InputTokens + usage.CacheWriteTokens,
		OutputTokens:        usage.OutputTokens,
		TokenCountSource:    source,
	}
	if capabilities.ReportsCacheReads {
		record.CachedInputTokens = intPtr(usage.CachedInputTokens)
	}
	if capabilities.ReportsCacheWrites {
		record.CacheWriteTokens = intPtr(usage.CacheWriteTokens)
	}
	if capabilities.ReportsReasoningTokens {
		record.ReasoningTokens = intPtr(usage.ReasoningTokens)
	}
	if usage.ProviderRawInputTokens > 0 {
		record.ProviderRawInputTokens = intPtr(usage.ProviderRawInputTokens)
	}
	return record
}

func classifyCacheAndTarget(record *RunRecord, target Target, opts Options) {
	if record.Usage.TotalInputTokens > 0 && !UsesGeneratedPayloadTarget(target, opts.Mode) {
		matched := math.Abs(float64(record.Usage.TotalInputTokens-record.RequestedInput))/float64(record.RequestedInput) <= opts.TargetTolerance
		record.TargetMatched = &matched
	}
	if !target.Capabilities.ReportsCacheReads || record.Usage.TotalInputTokens <= 0 {
		record.CacheState = "unknown"
		return
	}
	ratio := float64(intValue(record.Usage.CachedInputTokens)) / float64(record.Usage.TotalInputTokens)
	record.CacheRatio = &ratio
	switch {
	case ratio <= opts.CacheTolerance:
		record.CacheState = "miss"
	case ratio >= 1-opts.CacheTolerance:
		record.CacheState = "hit"
	default:
		record.CacheState = "partial"
	}
}

func deriveDecodeMetrics(record *RunRecord, firstActivity, lastActivity, firstText, lastText *time.Time, reasoningExpected bool) {
	if record.ActivityEvents < 2 || firstActivity == nil || lastActivity == nil || !lastActivity.After(*firstActivity) {
		if record.ActivityEvents == 1 {
			record.DecodeStatus = "non_incremental"
		} else {
			record.DecodeStatus = "insufficient_activity"
		}
		return
	}

	observed := lastActivity.Sub(*firstActivity)
	record.Timing.ObservedDecodeMS = floatPtr(float64(observed) / float64(time.Millisecond))
	output := record.Usage.OutputTokens
	reasoningTokens := intValue(record.Usage.ReasoningTokens)
	if reasoningTokens > 0 && !record.ObservableReasoningWindow {
		if record.VisibleTextEvents < 2 || firstText == nil || lastText == nil || !lastText.After(*firstText) {
			record.DecodeStatus = "hidden_reasoning_unobservable"
			return
		}
		visibleOutputAfterFirst := output - reasoningTokens - 1
		if visibleOutputAfterFirst <= 0 {
			record.DecodeStatus = "hidden_reasoning_unobservable"
			return
		}
		visibleDuration := lastText.Sub(*firstText)
		record.Timing.VisibleDecodeMS = floatPtr(float64(visibleDuration) / float64(time.Millisecond))
		record.Timing.VisibleTokensPerSec = floatPtr(float64(visibleOutputAfterFirst) / visibleDuration.Seconds())
		record.DecodeStatus = "visible_only_hidden_reasoning_excluded"
		return
	}
	if reasoningExpected && !record.ObservableReasoningWindow && record.Usage.ReasoningTokens == nil {
		record.DecodeStatus = "reasoning_detail_unavailable"
		return
	}
	if output < 2 {
		record.DecodeStatus = "insufficient_output_tokens"
		return
	}

	countAfterFirst := output - 1
	record.Timing.DecodeTokensPerSec = floatPtr(float64(countAfterFirst) / observed.Seconds())
	record.Timing.TPOTMS = floatPtr(float64(observed) / float64(time.Millisecond) / float64(countAfterFirst))
	record.DecodeStatus = "observed_incremental"
}

func finalizeEligibility(record *RunRecord, target Target, opts Options, phase string) {
	if phase != "measured" {
		record.ExclusionReasons = append(record.ExclusionReasons, "not_measured")
		return
	}
	if !record.Success {
		record.ExclusionReasons = append(record.ExclusionReasons, "failed_or_incomplete")
	}
	if record.RetryEvents > 0 || record.AttemptDiscards > 0 {
		record.ExclusionReasons = append(record.ExclusionReasons, "provider_retry_or_discard")
	}
	if record.TargetMatched != nil && !*record.TargetMatched {
		record.ExclusionReasons = append(record.ExclusionReasons, "input_target_mismatch_possible_truncation_or_prefix_reuse")
	}
	if target.Capabilities.ReportsCacheReads && record.CacheRatio != nil && *record.CacheRatio > opts.CacheTolerance {
		record.ExclusionReasons = append(record.ExclusionReasons, "cold_cache_contaminated")
	}

	record.LatencyEligible = record.Success && record.RetryEvents == 0 && record.AttemptDiscards == 0 &&
		(record.TargetMatched == nil || *record.TargetMatched) &&
		!(target.Capabilities.ReportsCacheReads && record.CacheRatio != nil && *record.CacheRatio > opts.CacheTolerance)
	record.FitEligible = record.LatencyEligible && record.Usage.TotalInputTokens > 0
	if record.Usage.TotalInputTokens <= 0 {
		record.ExclusionReasons = append(record.ExclusionReasons, "provider_input_tokens_unavailable")
	}
	if UsesGeneratedPayloadTarget(target, opts.Mode) {
		record.FitEligible = false
		record.ExclusionReasons = append(record.ExclusionReasons, "generated_payload_target_not_comparable_to_provider_total_input")
	}
	if !target.Capabilities.ReportsCacheReads && !opts.AllowUnknownCache {
		record.FitEligible = false
		record.ExclusionReasons = append(record.ExclusionReasons, "cache_state_unknown_use_allow_unknown_cache_for_token_fits")
	}
}

func intentionalOutputLimitStop(err error, record RunRecord) bool {
	var incomplete *llm.ResponsesIncompleteError
	return errors.As(err, &incomplete) && incomplete.Reason == "max_output_tokens" &&
		record.UsageReceived && record.RequestedOutput > 0 &&
		record.Usage.OutputTokens == record.RequestedOutput
}

func classifyRunError(record *RunRecord, ctx context.Context, err error) {
	if err == nil {
		return
	}
	record.Error = err.Error()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		record.TimedOut = true
		record.Error = context.DeadlineExceeded.Error()
	} else if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		record.Cancelled = true
		record.Error = context.Canceled.Error()
	}
}

func setFirst(dst **time.Time, value time.Time) {
	if *dst == nil {
		*dst = timePtr(value)
	}
}

func setEndToEnd(record *RunRecord, start, end time.Time) {
	record.Timing.EndToEndMS = durationMS(end, start)
}

func durationMS(end, start time.Time) *float64 {
	return floatPtr(float64(end.Sub(start)) / float64(time.Millisecond))
}

func timePtr(value time.Time) *time.Time { return &value }
func floatPtr(value float64) *float64    { return &value }
func intPtr(value int) *int              { return &value }
func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
