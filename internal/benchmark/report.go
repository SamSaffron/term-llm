package benchmark

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
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
	fmt.Fprintf(w, "  %s mode · %s cache · %d warmup + %d measured · seed %d\n",
		report.Benchmark.Mode, report.Benchmark.Cache, report.Benchmark.Warmups, report.Benchmark.Runs, report.Benchmark.Seed)
	fmt.Fprintln(w, "  Client-observed timing at the llm.Provider.Stream boundary")
	if report.Benchmark.AssumedContextLimit > 0 {
		writeWrappedLine(w, "WARNING: "+AssumedContextLimitLimitation(report.Benchmark.AssumedContextLimit), "", 78)
	}

	for _, target := range report.Targets {
		fmt.Fprintln(w)
		writeWrappedLine(w, "  "+target.ProviderKey, "  ", 78)
		writeWrappedLine(w, "  Model     "+target.RequestedModel, "            ", 78)
		fmt.Fprintf(w, "  Adapter   %s · credential %s\n", target.ProviderType, target.CredentialType)
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
		writeHumanPrefixedParts(w, "  Attempts  ", []string{
			fmt.Sprintf("%d succeeded", successes),
			fmt.Sprintf("%d failed (%d timeout)", failures, timeouts),
			fmt.Sprintf("%d retries", retryEvents),
			fmt.Sprintf("%d discards", discarded),
		}, 78)
		measurementScope := target.Capabilities.MeasurementScope
		if measurementScope == "" {
			measurementScope = "unspecified scope"
		}
		writeHumanPrefixedParts(w, "  Features  ", []string{
			"cache reads " + reportedLabel(target.Capabilities.ReportsCacheReads),
			"output limit " + supportedLabel(target.Capabilities.SupportsOutputLimit),
			measurementScope,
		}, 78)

		aggregates := aggregatesForTarget(report.Aggregates, target)
		cacheState := sharedCacheState(aggregates)
		cacheLine := "  Cache     "
		if cacheState != "" {
			cacheLine += cacheState
		}
		if target.Capabilities.CacheTelemetryNote != "" {
			if cacheState != "" {
				cacheLine += " · "
			}
			cacheLine += target.Capabilities.CacheTelemetryNote
		}
		if cacheLine != "  Cache     " {
			writeWrappedLine(w, cacheLine, "            ", 78)
		}

		wroteTable := writeHumanComparison(w, aggregates)
		writeHumanFits(w, report.Fits, target, aggregates, wroteTable)
		for _, aggregate := range aggregates {
			writeHumanAggregate(w, target, aggregate, cacheState)
		}
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

func writeHumanComparison(w io.Writer, aggregates []Aggregate) bool {
	if len(aggregates) < 2 {
		return false
	}
	rows := [][]string{{"Workload", "Target", "TTFT", "Decode", "Prefill"}}
	for _, aggregate := range aggregates {
		decode := "n/a"
		if highlightDecode(aggregate) {
			decode = formatCompactThroughput(decodeDistribution(aggregate))
		}
		rows = append(rows, []string{
			aggregate.Workload,
			fmt.Sprintf("%s→%s", formatInteger(int64(aggregate.RequestedInput)), formatInteger(int64(aggregate.RequestedOutput))),
			formatDurationMedian(aggregate.ActivityTTFTMS),
			decode,
			formatCompactThroughput(aggregate.EffectiveInputTokensRate),
		})
	}
	fmt.Fprintln(w)
	writeAlignedRows(w, "  ", rows, []bool{false, true, true, true, true})
	return true
}

func writeHumanAggregate(w io.Writer, target TargetMetadata, aggregate Aggregate, sharedCache string) {
	fmt.Fprintln(w)
	writeWrappedLine(w, humanAggregateHeading(target, aggregate), "    ", 78)

	if durationHasExtra(aggregate.ActivityTTFTMS) {
		writeHumanMetric(w, "Activity TTFT", formatDurationSummary(aggregate.ActivityTTFTMS))
	}
	if aggregate.VisibleTTFTMS.Median != nil {
		writeHumanMetric(w, "Visible TTFT", formatDurationSummary(aggregate.VisibleTTFTMS))
	}
	writeHumanMetric(w, "End to end", formatDurationSummary(aggregate.EndToEndMS))

	if highlightDecode(aggregate) {
		decodeLabel, decode := "Decode rate", formatRateSummary(aggregate.DecodeTokensPerSecond, "tok/s")
		if aggregate.DecodeTokensPerSecond.Median == nil && aggregate.VisibleTokensPerSecond.Median != nil {
			decodeLabel, decode = "Visible rate", formatRateSummary(aggregate.VisibleTokensPerSecond, "tok/s")
		}
		if aggregate.TPOTMS.Median != nil {
			decode += " · TPOT " + formatRateSummary(aggregate.TPOTMS, "ms")
		}
		writeHumanMetric(w, decodeLabel, decode)
	}
	writeHumanMetric(w, "Effective input", formatRateSummary(aggregate.EffectiveInputTokensRate, "tok/s"))
	writeHumanMetric(w, "Input tokens", formatInputTokens(target, aggregate))
	writeHumanMetric(w, "Cache tokens", formatCacheTokens(aggregate, sharedCache == ""))
	writeHumanMetric(w, "Output tokens", formatOutputTokens(aggregate))
	if !aggregateFullyValid(aggregate) {
		writeHumanMetric(w, "Valid runs", fmt.Sprintf("latency %d/%d · fit %d/%d · decode %d/%d",
			aggregate.LatencyValidRuns, aggregate.MeasuredRuns,
			aggregate.FitValidRuns, aggregate.MeasuredRuns,
			aggregate.DecodeValidRuns, aggregate.MeasuredRuns))
	}
	if aggregate.ErrorRuns > 0 {
		writeHumanMetric(w, "Errors", fmt.Sprintf("%d measured run(s) failed", aggregate.ErrorRuns))
	}
}

func writeHumanFits(w io.Writer, fits []Fit, target TargetMetadata, aggregates []Aggregate, wroteTable bool) {
	var targetFits []Fit
	for _, fit := range fits {
		if fit.Provider == target.ProviderKey && fit.RequestedModel == target.RequestedModel {
			targetFits = append(targetFits, fit)
		}
	}
	if len(targetFits) == 0 && !hasPrefillAggregate(aggregates) {
		return
	}
	if !wroteTable {
		fmt.Fprintln(w)
	}
	if len(targetFits) == 0 {
		fmt.Fprintln(w, "  Prefill fit  unavailable · needs 3 distinct fit-valid input lengths")
		return
	}
	for _, fit := range targetFits {
		fmt.Fprintf(w, "  Prefill fit  %s tok/s  %s · %d lengths · %d runs\n",
			formatInteger(int64(fit.EffectiveTokensPerSec+0.5)), fit.Range, fit.DistinctLengths, fit.ValidRuns)
	}
}

func humanAggregateHeading(target TargetMetadata, aggregate Aggregate) string {
	parts := []string{formatDurationMedian(aggregate.ActivityTTFTMS) + " TTFT"}
	if highlightDecode(aggregate) {
		parts = append(parts, formatCompactThroughput(decodeDistribution(aggregate)))
		if aggregate.TPOTMS.Median != nil {
			parts = append(parts, fmt.Sprintf("%.1fms TPOT", *aggregate.TPOTMS.Median))
		}
	} else if aggregate.EffectiveInputTokensRate.Median != nil {
		parts = append(parts, formatCompactThroughput(aggregate.EffectiveInputTokensRate)+" input")
	}
	heading := fmt.Sprintf("  %s  %s → %s    %s",
		aggregate.Workload,
		formatInteger(int64(aggregate.RequestedInput)),
		formatInteger(int64(aggregate.RequestedOutput)),
		strings.Join(parts, " · "))
	if target.InputTargetMeaning == "generated_user_payload" {
		heading += " (payload target)"
	}
	return heading
}

func formatInputTokens(target TargetMetadata, aggregate Aggregate) string {
	provider := formatNumber(aggregate.TotalInputTokens)
	computed := formatNumber(aggregate.ComputedInputTokens)
	if target.InputTargetMeaning == "generated_user_payload" {
		return "provider " + provider + " (reported, not target-gated) · computed " + computed
	}
	if provider == computed {
		return provider
	}
	return "provider " + provider + " · computed " + computed
}

func formatCacheTokens(aggregate Aggregate, includeState bool) string {
	text := "read " + formatNumber(aggregate.CachedInputTokens) + " · write " + formatNumber(aggregate.CacheWriteTokens)
	if includeState && aggregate.CacheState != "" {
		text += " · " + aggregate.CacheState
	}
	return text
}

func formatOutputTokens(aggregate Aggregate) string {
	actual := formatNumber(aggregate.OutputTokens)
	requested := formatInteger(int64(aggregate.RequestedOutput))
	if actual == requested {
		return actual
	}
	return actual + " actual · " + requested + " requested"
}

const humanLabelWidth = 16

func writeHumanMetric(w io.Writer, label, value string) {
	prefix := fmt.Sprintf("    %-*s", humanLabelWidth, label)
	writeHumanPrefixedParts(w, prefix, strings.Split(value, " · "), 78)
}

func aggregatesForTarget(aggregates []Aggregate, target TargetMetadata) []Aggregate {
	out := make([]Aggregate, 0, len(aggregates))
	for _, aggregate := range aggregates {
		if aggregate.Provider == target.ProviderKey && aggregate.RequestedModel == target.RequestedModel {
			out = append(out, aggregate)
		}
	}
	return out
}

func sharedCacheState(aggregates []Aggregate) string {
	if len(aggregates) == 0 {
		return ""
	}
	state := aggregates[0].CacheState
	if state == "" || state == "mixed" {
		return ""
	}
	for _, aggregate := range aggregates[1:] {
		if aggregate.CacheState != state {
			return ""
		}
	}
	return state
}

func hasPrefillAggregate(aggregates []Aggregate) bool {
	for _, aggregate := range aggregates {
		if aggregate.Workload == "prefill" {
			return true
		}
	}
	return false
}

func highlightDecode(aggregate Aggregate) bool {
	return aggregate.RequestedOutput >= 64
}

func decodeDistribution(aggregate Aggregate) Distribution {
	if aggregate.DecodeTokensPerSecond.Median != nil {
		return aggregate.DecodeTokensPerSecond
	}
	return aggregate.VisibleTokensPerSecond
}

func aggregateFullyValid(aggregate Aggregate) bool {
	if aggregate.MeasuredRuns <= 0 {
		return true
	}
	if aggregate.LatencyValidRuns != aggregate.MeasuredRuns || aggregate.FitValidRuns != aggregate.MeasuredRuns {
		return false
	}
	if highlightDecode(aggregate) && aggregate.DecodeValidRuns != aggregate.MeasuredRuns {
		return false
	}
	return true
}

func durationHasExtra(distribution Distribution) bool {
	if distribution.Median == nil {
		return false
	}
	if distribution.P95 != nil {
		return true
	}
	return distribution.Min != nil && distribution.Max != nil && *distribution.Min != *distribution.Max
}

func formatDurationMedian(distribution Distribution) string {
	if distribution.Median == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2fs", *distribution.Median/1000)
}

func formatCompactThroughput(distribution Distribution) string {
	if distribution.Median == nil {
		return "n/a"
	}
	value := *distribution.Median
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.1fM/s", value/1_000_000)
	case value >= 1000:
		return fmt.Sprintf("%.1fk/s", value/1000)
	case value >= 10:
		return fmt.Sprintf("%.0f tok/s", value)
	default:
		return fmt.Sprintf("%.1f tok/s", value)
	}
}

func writeAlignedRows(w io.Writer, indent string, rows [][]string, alignRight []bool) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				continue
			}
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	for _, row := range rows {
		line := indent
		for i, cell := range row {
			if i > 0 {
				line += "  "
			}
			width := 0
			if i < len(widths) {
				width = widths[i]
			}
			if i < len(alignRight) && alignRight[i] {
				line += padLeft(cell, width)
			} else {
				line += padRight(cell, width)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

func padLeft(value string, width int) string {
	n := utf8.RuneCountInString(value)
	if n >= width {
		return value
	}
	return strings.Repeat(" ", width-n) + value
}

func padRight(value string, width int) string {
	n := utf8.RuneCountInString(value)
	if n >= width {
		return value
	}
	return value + strings.Repeat(" ", width-n)
}

func formatDurationSummary(distribution Distribution) string {
	if distribution.Median == nil {
		return "n/a"
	}
	result := fmt.Sprintf("%.2fs", *distribution.Median/1000)
	if distribution.Min != nil && distribution.Max != nil && *distribution.Min != *distribution.Max {
		result += fmt.Sprintf(" (%.2f-%.2fs", *distribution.Min/1000, *distribution.Max/1000)
		if distribution.P95 != nil {
			result += fmt.Sprintf(", p95 %.2fs", *distribution.P95/1000)
		}
		result += ")"
	} else if distribution.P95 != nil {
		result += fmt.Sprintf(" (p95 %.2fs)", *distribution.P95/1000)
	}
	return result
}

func formatRateSummary(distribution Distribution, unit string) string {
	if distribution.Median == nil {
		return "n/a"
	}
	result := formatFixedRate(*distribution.Median) + " " + unit
	if distribution.Min != nil && distribution.Max != nil && *distribution.Min != *distribution.Max {
		result += fmt.Sprintf(" (%s-%s)", formatFixedRate(*distribution.Min), formatFixedRate(*distribution.Max))
	}
	return result
}

func formatFixedRate(value float64) string {
	formatted := fmt.Sprintf("%.1f", value)
	dot := strings.LastIndex(formatted, ".")
	intPart, frac := formatted, ""
	if dot >= 0 {
		intPart, frac = formatted[:dot], formatted[dot:]
	}
	sign := ""
	if strings.HasPrefix(intPart, "-") {
		sign = "-"
		intPart = intPart[1:]
	}
	for i := len(intPart) - 3; i > 0; i -= 3 {
		intPart = intPart[:i] + "," + intPart[i:]
	}
	return sign + intPart + frac
}

func writeHumanPrefixedParts(w io.Writer, prefix string, parts []string, width int) {
	if len(parts) == 0 {
		return
	}
	pad := strings.Repeat(" ", utf8.RuneCountInString(prefix))
	flush := func(line string) {
		if utf8.RuneCountInString(line) <= width {
			fmt.Fprintln(w, line)
			return
		}
		writeWrappedLine(w, line, pad, width)
	}
	line := prefix + parts[0]
	for _, part := range parts[1:] {
		trial := line + " · " + part
		if utf8.RuneCountInString(trial) <= width {
			line = trial
			continue
		}
		flush(line)
		line = pad + part
	}
	flush(line)
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
	text = strings.TrimRight(text, " ")
	if text == "" {
		fmt.Fprintln(w)
		return
	}
	if utf8.RuneCountInString(text) <= width {
		fmt.Fprintln(w, text)
		return
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		fmt.Fprintln(w)
		return
	}
	first := words[0]
	idx := strings.Index(text, first)
	prefixEnd := idx + len(first)
	for prefixEnd < len(text) && text[prefixEnd] == ' ' {
		prefixEnd++
	}
	var line string
	if len(words) == 1 {
		fmt.Fprintln(w, text)
		return
	}
	line = text[:prefixEnd] + words[1]
	for _, word := range words[2:] {
		trial := line + " " + word
		if utf8.RuneCountInString(trial) <= width {
			line = trial
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
