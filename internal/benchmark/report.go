package benchmark

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

func DefaultProfile(mode string) ([]Scenario, int, int, error) {
	switch mode {
	case "balanced":
		return []Scenario{
			{InputTokens: 2_000, OutputTokens: 128, Workload: "decode"},
			{InputTokens: 16_000, OutputTokens: 16, Workload: "prefill"},
			{InputTokens: 64_000, OutputTokens: 16, Workload: "prefill"},
		}, 3, 1, nil
	case "quick":
		return []Scenario{{InputTokens: 2_000, OutputTokens: 128, Workload: "decode"}}, 3, 1, nil
	case "decode":
		return []Scenario{{InputTokens: 2_000, OutputTokens: 256, Workload: "decode"}}, 5, 1, nil
	case "prefill":
		return scenariosForInputs([]int{1_000, 4_000, 16_000, 32_000, 64_000, 96_000, 128_000}, 16, "prefill"), 3, 1, nil
	case "long-context":
		return scenariosForInputs([]int{32_000, 64_000, 96_000, 128_000, 256_000, 512_000}, 16, "prefill"), 3, 1, nil
	default:
		return nil, 0, 0, fmt.Errorf("invalid benchmark mode %q (want balanced, quick, decode, prefill, or long-context)", mode)
	}
}

func ScenariosForOverride(mode string, inputs []int, output int) []Scenario {
	workload := "prefill"
	if mode == "quick" || mode == "decode" {
		workload = "decode"
	}
	return scenariosForInputs(inputs, output, workload)
}

func scenariosForInputs(inputs []int, output int, workload string) []Scenario {
	out := make([]Scenario, 0, len(inputs))
	for _, input := range inputs {
		out = append(out, Scenario{InputTokens: input, OutputTokens: output, Workload: workload})
	}
	return out
}

func ComputeBudget(targetCount int, scenarios []Scenario, runs, warmups int, outputNotGuaranteed bool) Budget {
	return computeBudget(targetCount, scenarios, runs, warmups, outputNotGuaranteed, true)
}

func ComputeBudgetForTarget(target Target, mode string, scenarios []Scenario, runs, warmups int, outputNotGuaranteed bool) Budget {
	return computeBudget(1, scenarios, runs, warmups, outputNotGuaranteed, !UsesGeneratedPayloadTarget(target, mode))
}

func computeBudget(targetCount int, scenarios []Scenario, runs, warmups int, outputNotGuaranteed, includesCalibration bool) Budget {
	budget := Budget{
		IncludesCalibration:     includesCalibration,
		IncludesWarmups:         warmups > 0,
		IncludesRetryAllowance:  true,
		CacheWriteBillingRisk:   true,
		OutputBudgetIsRequested: outputNotGuaranteed,
		Notes: []string{
			"Input totals are maximum requested payload/provider tokens, not billed-token predictions.",
			"Each measured cold request reserves one fresh-payload retry for detected cache contamination.",
			"Cache-write tokens are computed input and may be billed differently by the provider.",
		},
	}
	calibrationRequests := 0
	if includesCalibration {
		calibrationRequests = 2
		budget.Notes = append(budget.Notes, "Each scenario uses a fresh calibration request plus a fresh validation request before warmups.")
	}
	requestsPerScenario := calibrationRequests + warmups + runs*2
	for _, scenario := range scenarios {
		budget.MaximumRequests += requestsPerScenario * targetCount
		budget.MaximumTotalInput += int64(scenario.InputTokens * requestsPerScenario * targetCount)
		budget.MaximumTotalOutput += int64(scenario.OutputTokens * requestsPerScenario * targetCount)
		budget.MaximumRequestInput = max(budget.MaximumRequestInput, scenario.InputTokens)
		budget.MaximumRequestOutput = max(budget.MaximumRequestOutput, scenario.OutputTokens)
	}
	if outputNotGuaranteed {
		budget.Notes = append(budget.Notes, "At least one adapter cannot enforce MaxOutputTokens; its output total is a disclosed requested ceiling only.")
	}
	return budget
}

// AssumedContextLimitLimitation describes the safety boundary of the expert
// Ollama context eligibility override.
func AssumedContextLimitLimitation(limit int) string {
	return fmt.Sprintf("EXPERT OVERRIDE: assumed Ollama context limit %d is used only for benchmark safety eligibility. term-llm did not discover, verify, or configure server num_ctx; provider-reported input target matching remains mandatory, and truncated requests are invalid.", limit)
}

func NewReport(opts Options, budget Budget, targets []Target, records []RunRecord) Report {
	metadata := make([]TargetMetadata, 0, len(targets))
	limitations := []string{
		"Timings are client-observed at the llm.Provider/llm.Stream boundary, not server-side phase telemetry.",
		"Transport chunks are not treated as token boundaries; throughput uses provider-reported token counts only.",
		"Cold, sequential measurements only; warm-cache and concurrent workloads are intentionally not implemented in schema version 1.",
	}
	if opts.AssumedContextLimit > 0 {
		limitations = append(limitations, AssumedContextLimitLimitation(opts.AssumedContextLimit))
	}
	for _, target := range targets {
		providerName := target.ProviderKey
		credentialType := "unavailable"
		if target.Provider != nil {
			providerName = target.Provider.Name()
			credentialType = target.Provider.Credential()
		}
		entry := TargetMetadata{
			ProviderKey:         target.ProviderKey,
			ProviderType:        target.ProviderType,
			ProviderName:        providerName,
			CredentialType:      credentialType,
			RequestedModel:      target.RequestedModel,
			InputTargetMeaning:  inputTargetMeaning(target, opts.Mode),
			InputLimit:          target.InputLimit,
			ConfiguredNumCtx:    target.ConfiguredNumCtx,
			AssumedContextLimit: target.AssumedContextLimit,
			ServiceTier:         target.ServiceTier,
			Capabilities:        target.Capabilities,
			ReasoningEffort:     opts.ReasoningEffort,
			ReasoningExpected:   target.ReasoningExpected || opts.ReasoningEffort != "",
			OutputLimitMeaning:  "enforced_by_adapter",
			Error:               target.Error,
		}
		if opts.TemperatureSet && target.Capabilities.SupportsTemperature {
			value := opts.Temperature
			entry.Temperature = &value
		}
		if !target.Capabilities.SupportsOutputLimit {
			entry.OutputLimitMeaning = "requested_but_adapter_does_not_enforce"
		}
		if !target.Capabilities.ReportsCacheReads {
			limitations = append(limitations, fmt.Sprintf("%s:%s does not expose trustworthy cache-read telemetry; cache state is unknown.", target.ProviderKey, target.RequestedModel))
		}
		if target.Capabilities.ManagedContext {
			limitations = append(limitations, fmt.Sprintf("%s:%s timing is subprocess-inclusive and not directly comparable to HTTP/API adapters.", target.ProviderKey, target.RequestedModel))
		}
		if !target.Capabilities.IncrementalStream {
			limitations = append(limitations, fmt.Sprintf("%s:%s output is delivered through a non-incremental or bursty adapter boundary; provider decode throughput and TPOT are unavailable. Provider-reported output usage and end-to-end timing remain recorded for separate analysis.", target.ProviderKey, target.RequestedModel))
		}
		if UsesGeneratedPayloadTarget(target, opts.Mode) {
			limitations = append(limitations, fmt.Sprintf("%s:%s --input-tokens targets the generated user payload only; provider-reported total input includes fixed CLI/system overhead and can vary as cache buckets shift, so provider-total calibration and target matching are disabled while actual provider totals remain recorded.", target.ProviderKey, target.RequestedModel))
		}
		metadata = append(metadata, entry)
	}
	return Report{
		SchemaVersion: SchemaVersion,
		Benchmark: BenchmarkMetadata{
			Mode:                opts.Mode,
			Cache:               opts.Cache,
			Seed:                opts.Seed,
			Runs:                opts.Runs,
			Warmups:             opts.Warmups,
			Concurrency:         opts.Concurrency,
			TimeoutSeconds:      opts.Timeout.Seconds(),
			CacheTolerance:      opts.CacheTolerance,
			TargetTolerance:     opts.TargetTolerance,
			AllowUnknownCache:   opts.AllowUnknownCache,
			AssumedContextLimit: opts.AssumedContextLimit,
			RetryPolicy:         "provider retries disabled; one benchmark-level fresh-payload retry only after detected cache contamination",
			TimingBoundary:      "client-observed at llm.Provider.Stream boundary",
		},
		Budget:         budget,
		TermLLMVersion: opts.TermLLMVersion,
		Targets:        metadata,
		Runs:           records,
		Aggregates:     AggregateRuns(records),
		Fits:           ComputeFits(records),
		Limitations:    deduplicate(limitations),
	}
}

func WriteBudget(w io.Writer, mode string, budget Budget) {
	fmt.Fprintf(w, "Benchmark token budget (%s):\n", mode)
	fmt.Fprintf(w, "  maximum requests: %d\n", budget.MaximumRequests)
	fmt.Fprintf(w, "  maximum request: %d input + %d requested output tokens\n", budget.MaximumRequestInput, budget.MaximumRequestOutput)
	fmt.Fprintf(w, "  maximum total: %d input + %d requested output tokens\n", budget.MaximumTotalInput, budget.MaximumTotalOutput)
	if budget.IncludesCalibration {
		fmt.Fprintln(w, "  includes calibration plus validation, warmups, and one fresh-payload cache-contamination retry per measured request")
	} else {
		fmt.Fprintln(w, "  includes warmups and one fresh-payload cache-contamination retry per measured request; provider-total calibration is disabled for generated-payload targets")
	}
	fmt.Fprintln(w, "  cache writes count as computed input and may have provider-specific billing")
	if budget.OutputBudgetIsRequested {
		fmt.Fprintln(w, "  warning: at least one adapter cannot enforce the requested output ceiling")
	}
}

func WriteHuman(w io.Writer, report Report) {
	fmt.Fprintf(w, "\nMode: %s | cache: %s | concurrency: %d\n", report.Benchmark.Mode, report.Benchmark.Cache, report.Benchmark.Concurrency)
	fmt.Fprintf(w, "Runs: %d warmup + %d measured per scenario | seed: %d\n", report.Benchmark.Warmups, report.Benchmark.Runs, report.Benchmark.Seed)
	fmt.Fprintln(w, "Timing: client-observed at llm.Provider.Stream boundary")
	if report.Benchmark.AssumedContextLimit > 0 {
		fmt.Fprintf(w, "WARNING: %s\n", AssumedContextLimitLimitation(report.Benchmark.AssumedContextLimit))
	}

	for _, target := range report.Targets {
		fmt.Fprintf(w, "\nProvider: %s (%s)\nModel: %s\n", target.ProviderName, target.ProviderKey, target.RequestedModel)
		if target.Error != "" {
			fmt.Fprintf(w, "Status: failed — %s\n", target.Error)
			continue
		}
		if target.AssumedContextLimit > 0 {
			fmt.Fprintf(w, "Assumed context limit: %d tokens (eligibility only; server num_ctx unchanged)\n", target.AssumedContextLimit)
		}
		fmt.Fprintf(w, "Cache telemetry: %s", yesNo(target.Capabilities.ReportsCacheReads))
		if target.Capabilities.CacheTelemetryNote != "" {
			fmt.Fprintf(w, " — %s", target.Capabilities.CacheTelemetryNote)
		}
		fmt.Fprintln(w)
		if target.Capabilities.ManagedContext {
			fmt.Fprintln(w, "Measurement scope: subprocess-inclusive (managed provider opt-in)")
		}
		if !target.Capabilities.IncrementalStream {
			fmt.Fprintln(w, "Decode throughput: unavailable (adapter delivery is non-incremental or bursty; use provider output usage and E2E timing for separate analysis)")
		}
		if target.InputTargetMeaning == "generated_user_payload" {
			fmt.Fprintln(w, "Input target: generated user payload; provider total is reported but not calibrated or target-gated")
		}
		if !target.Capabilities.SupportsOutputLimit {
			fmt.Fprintln(w, "Output limit: unsupported by adapter; actual provider output tokens are reported")
		}
		successes, failures, timeouts, retryEvents, discarded := targetRunCounts(report.Runs, target.ProviderKey, target.RequestedModel)
		fmt.Fprintf(w, "Raw measured attempts: %d success, %d failure (%d timeout), %d provider retry events, %d attempt discards\n", successes, failures, timeouts, retryEvents, discarded)
		fmt.Fprintln(w, "Workload  Input target  Provider input  Computed  Cache read/write  Output  Activity TTFT min/med/max  Visible TTFT min/med/max  Effective input tok/s  Decode tok/s  TPOT ms  E2E min/med/max  Latency/Fit")
		for _, aggregate := range report.Aggregates {
			if aggregate.Provider != target.ProviderKey || aggregate.RequestedModel != target.RequestedModel {
				continue
			}
			decode := formatMedian(aggregate.DecodeTokensPerSecond)
			if aggregate.DecodeTokensPerSecond.Median == nil {
				decode = formatMedian(aggregate.VisibleTokensPerSecond)
				if aggregate.VisibleTokensPerSecond.Median != nil {
					decode += " visible"
				}
			}
			fmt.Fprintf(w, "%-8s  %11d  %14s  %8s  %8s/%-8s  %6s  %25s  %24s  %21s  %12s  %7s  %15s  %d/%d | %d/%d\n",
				aggregate.Workload,
				aggregate.RequestedInput,
				formatNumber(aggregate.TotalInputTokens),
				formatNumber(aggregate.ComputedInputTokens),
				formatNumber(aggregate.CachedInputTokens),
				formatNumber(aggregate.CacheWriteTokens),
				formatNumber(aggregate.OutputTokens),
				formatDurationDistribution(aggregate.ActivityTTFTMS),
				formatDurationDistribution(aggregate.VisibleTTFTMS),
				formatMedian(aggregate.EffectiveInputTokensRate),
				decode,
				formatMedian(aggregate.TPOTMS),
				formatDurationDistribution(aggregate.EndToEndMS),
				aggregate.LatencyValidRuns,
				aggregate.MeasuredRuns,
				aggregate.FitValidRuns,
				aggregate.MeasuredRuns,
			)
			if aggregate.CacheState == "unknown" {
				fmt.Fprintln(w, "                      cache state: unknown (zero cached tokens is not treated as a cache miss)")
			}
		}
	}

	if len(report.Fits) > 0 {
		fmt.Fprintln(w, "\nLocal client-observed effective prefill slopes:")
		for _, fit := range report.Fits {
			fmt.Fprintf(w, "  %s:%s %s: %.0f tok/s (%d lengths, %d valid runs)\n", fit.Provider, fit.RequestedModel, fit.Range, fit.EffectiveTokensPerSec, fit.DistinctLengths, fit.ValidRuns)
		}
	} else {
		fmt.Fprintln(w, "\nLocal client-observed effective prefill slopes: unavailable (requires at least three distinct valid lengths in a range; unknown cache telemetry also requires --allow-unknown-cache)")
	}
	fmt.Fprintln(w, "\nThese are client-observed measurements, not server phase timings.")
}

func targetRunCounts(records []RunRecord, provider, model string) (successes, failures, timeouts, retryEvents, discards int) {
	for _, record := range records {
		if record.Provider != provider || record.RequestedModel != model || record.Phase != "measured" {
			continue
		}
		if record.Success {
			successes++
		} else {
			failures++
		}
		if record.TimedOut {
			timeouts++
		}
		retryEvents += record.RetryEvents
		discards += record.AttemptDiscards
	}
	return successes, failures, timeouts, retryEvents, discards
}

func formatDurationDistribution(distribution Distribution) string {
	if distribution.Min == nil || distribution.Median == nil || distribution.Max == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f/%.2f/%.2fs", *distribution.Min/1000, *distribution.Median/1000, *distribution.Max/1000)
}

func formatMedian(distribution Distribution) string {
	if distribution.Median == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *distribution.Median)
}

func formatNumber(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f", *value)
}

func yesNo(value bool) string {
	if value {
		return "reported"
	}
	return "not reported"
}

func deduplicate(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
