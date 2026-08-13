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
			{InputTokens: 4_000, OutputTokens: 16, Workload: "prefill"},
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
	fmt.Fprintf(w, "Token budget (%s) - requested ceilings, not billing estimates\n", mode)
	fmt.Fprintf(w, "  Requests     up to %s\n", formatInteger(int64(budget.MaximumRequests)))
	fmt.Fprintf(w, "  Per request  %s input + %s output tokens\n",
		formatInteger(int64(budget.MaximumRequestInput)), formatInteger(int64(budget.MaximumRequestOutput)))
	fmt.Fprintf(w, "  Total        %s input + %s output tokens\n",
		formatInteger(budget.MaximumTotalInput), formatInteger(budget.MaximumTotalOutput))
	if budget.IncludesCalibration {
		fmt.Fprintln(w, "  Includes     calibration, validation, warmups, and retry allowance")
	} else {
		fmt.Fprintln(w, "  Includes     warmups and retry allowance; calibration disabled")
	}
	if budget.OutputBudgetIsRequested {
		writeWrappedBullet(w, "At least one adapter cannot enforce the requested output ceiling.", 78)
	}
}

func WriteHuman(w io.Writer, report Report) {
	fmt.Fprintln(w, "\nBenchmark results")
	fmt.Fprintf(w, "  %s mode | %s cache | %d warmup + %d measured | seed %d\n",
		report.Benchmark.Mode, report.Benchmark.Cache, report.Benchmark.Warmups, report.Benchmark.Runs, report.Benchmark.Seed)
	fmt.Fprintln(w, "  Client-observed timing at the llm.Provider.Stream boundary")
	if report.Benchmark.AssumedContextLimit > 0 {
		writeWrappedLine(w, "WARNING: "+AssumedContextLimitLimitation(report.Benchmark.AssumedContextLimit), "", 78)
	}

	for _, target := range report.Targets {
		fmt.Fprintln(w)
		writeWrappedLine(w, target.ProviderKey, "  ", 78)
		writeWrappedLine(w, "  Model     "+target.RequestedModel, "            ", 78)
		fmt.Fprintf(w, "  Adapter   %s | credential %s\n", target.ProviderType, target.CredentialType)
		if target.Error != "" {
			writeWrappedLine(w, "  Status    failed - "+target.Error, "            ", 78)
			continue
		}
		if target.AssumedContextLimit > 0 {
			writeWrappedLine(w,
				fmt.Sprintf("  Assumed context limit: %d tokens (eligibility only; server num_ctx unchanged)", target.AssumedContextLimit),
				"    ", 78)
		}
		successes, failures, timeouts, retryEvents, discarded := targetRunCounts(report.Runs, target.ProviderKey, target.RequestedModel)
		fmt.Fprintf(w, "  Attempts  %d succeeded | %d failed (%d timeout) | %d retries | %d discards\n",
			successes, failures, timeouts, retryEvents, discarded)
		measurementScope := target.Capabilities.MeasurementScope
		if measurementScope == "" {
			measurementScope = "unspecified scope"
		}
		fmt.Fprintf(w, "  Features  cache reads %s | output limit %s | %s\n",
			reportedLabel(target.Capabilities.ReportsCacheReads), supportedLabel(target.Capabilities.SupportsOutputLimit), measurementScope)
		if target.Capabilities.CacheTelemetryNote != "" {
			writeWrappedLine(w, "  Cache     "+target.Capabilities.CacheTelemetryNote, "            ", 78)
		}

		for _, aggregate := range report.Aggregates {
			if aggregate.Provider != target.ProviderKey || aggregate.RequestedModel != target.RequestedModel {
				continue
			}
			writeHumanAggregate(w, target, aggregate)
		}
		writeHumanFits(w, report.Fits, target)
	}
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

func writeHumanAggregate(w io.Writer, target TargetMetadata, aggregate Aggregate) {
	heading := fmt.Sprintf("  %s | %s input -> %s requested output",
		aggregate.Workload, formatInteger(int64(aggregate.RequestedInput)), formatInteger(int64(aggregate.RequestedOutput)))
	if target.InputTargetMeaning == "generated_user_payload" {
		heading += " (payload target)"
	}
	fmt.Fprintf(w, "\n%s\n", heading)
	fmt.Fprintf(w, "    Activity TTFT  %s\n", formatDurationSummary(aggregate.ActivityTTFTMS))
	fmt.Fprintf(w, "    Visible TTFT   %s\n", formatDurationSummary(aggregate.VisibleTTFTMS))
	fmt.Fprintf(w, "    End to end     %s\n", formatDurationSummary(aggregate.EndToEndMS))

	decodeLabel, decode := "Decode rate", formatRateSummary(aggregate.DecodeTokensPerSecond, "tok/s")
	if aggregate.DecodeTokensPerSecond.Median == nil && aggregate.VisibleTokensPerSecond.Median != nil {
		decodeLabel, decode = "Visible rate", formatRateSummary(aggregate.VisibleTokensPerSecond, "tok/s")
	}
	fmt.Fprintf(w, "    %-15s%s", decodeLabel, decode)
	if aggregate.TPOTMS.Median != nil {
		fmt.Fprintf(w, " | TPOT %s", formatRateSummary(aggregate.TPOTMS, "ms"))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "    Effective input %s\n", formatRateSummary(aggregate.EffectiveInputTokensRate, "tok/s"))

	providerInput := formatNumber(aggregate.TotalInputTokens)
	if target.InputTargetMeaning == "generated_user_payload" {
		providerInput += " (reported, not target-gated)"
	}
	fmt.Fprintf(w, "    Input tokens    provider %s | computed %s\n", providerInput, formatNumber(aggregate.ComputedInputTokens))
	fmt.Fprintf(w, "    Cache tokens    read %s | write %s | state %s\n",
		formatNumber(aggregate.CachedInputTokens), formatNumber(aggregate.CacheWriteTokens), aggregate.CacheState)
	fmt.Fprintf(w, "    Output tokens   %s actual | %s requested\n",
		formatNumber(aggregate.OutputTokens), formatInteger(int64(aggregate.RequestedOutput)))
	fmt.Fprintf(w, "    Valid runs      latency %d/%d | fit %d/%d | decode %d/%d\n",
		aggregate.LatencyValidRuns, aggregate.MeasuredRuns,
		aggregate.FitValidRuns, aggregate.MeasuredRuns,
		aggregate.DecodeValidRuns, aggregate.MeasuredRuns)
	if aggregate.ErrorRuns > 0 {
		fmt.Fprintf(w, "    Errors          %d measured run(s) failed\n", aggregate.ErrorRuns)
	}
}

func writeHumanFits(w io.Writer, fits []Fit, target TargetMetadata) {
	var targetFits []Fit
	for _, fit := range fits {
		if fit.Provider == target.ProviderKey && fit.RequestedModel == target.RequestedModel {
			targetFits = append(targetFits, fit)
		}
	}
	fmt.Fprintln(w, "\n  Effective prefill fit")
	if len(targetFits) == 0 {
		fmt.Fprintln(w, "    unavailable - needs at least 3 distinct fit-valid input lengths")
		return
	}
	for _, fit := range targetFits {
		fmt.Fprintf(w, "    %s  %.0f tok/s | %d lengths | %d valid runs\n",
			fit.Range, fit.EffectiveTokensPerSec, fit.DistinctLengths, fit.ValidRuns)
	}
}

func formatDurationSummary(distribution Distribution) string {
	if distribution.Median == nil {
		return "n/a"
	}
	result := fmt.Sprintf("%.2fs", *distribution.Median/1000)
	if distribution.Min != nil && distribution.Max != nil {
		result += fmt.Sprintf(" (%.2f-%.2fs", *distribution.Min/1000, *distribution.Max/1000)
		if distribution.P95 != nil {
			result += fmt.Sprintf(", p95 %.2fs", *distribution.P95/1000)
		}
		result += ")"
	}
	return result
}

func formatRateSummary(distribution Distribution, unit string) string {
	if distribution.Median == nil {
		return "n/a"
	}
	result := fmt.Sprintf("%.1f %s", *distribution.Median, unit)
	if distribution.Min != nil && distribution.Max != nil && *distribution.Min != *distribution.Max {
		result += fmt.Sprintf(" (%.1f-%.1f)", *distribution.Min, *distribution.Max)
	}
	return result
}

func formatNumber(value *float64) string {
	if value == nil {
		return "n/a"
	}
	return formatInteger(int64(*value + 0.5))
}

func formatInteger(value int64) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	digits := fmt.Sprintf("%d", value)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return sign + digits
}

func reportedLabel(value bool) string {
	if value {
		return "reported"
	}
	return "not reported"
}

func supportedLabel(value bool) string {
	if value {
		return "enforced"
	}
	return "not enforced"
}

func writeWrappedBullet(w io.Writer, text string, width int) {
	writeWrappedLine(w, "  - "+text, "    ", width)
}

func writeWrappedLine(w io.Writer, text, continuation string, width int) {
	words := strings.Fields(text)
	if len(words) == 0 {
		fmt.Fprintln(w)
		return
	}
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) <= width {
			line += " " + word
			continue
		}
		fmt.Fprintln(w, line)
		line = continuation + word
	}
	fmt.Fprintln(w, line)
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
