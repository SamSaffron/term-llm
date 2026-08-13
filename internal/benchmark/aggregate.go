package benchmark

import (
	"fmt"
	"math"
	"sort"
)

type aggregateKey struct {
	provider string
	model    string
	workload string
	input    int
	output   int
}

type runKey struct {
	aggregateKey
	run int
}

func AggregateRuns(records []RunRecord) []Aggregate {
	latest := latestMeasuredAttempts(records)
	groups := make(map[aggregateKey][]RunRecord)
	for _, record := range latest {
		key := aggregateKey{record.Provider, record.RequestedModel, record.Workload, record.RequestedInput, record.RequestedOutput}
		groups[key] = append(groups[key], record)
	}

	out := make([]Aggregate, 0, len(groups))
	for key, runs := range groups {
		aggregate := Aggregate{
			Provider:        key.provider,
			RequestedModel:  key.model,
			Workload:        key.workload,
			RequestedInput:  key.input,
			RequestedOutput: key.output,
			MeasuredRuns:    len(runs),
			CacheState:      aggregateCacheState(runs),
		}
		var (
			total, computed, cached, writes, outputs []float64
			activity, visible, e2e                   []float64
			decode, visibleDecode, tpot, rate        []float64
		)
		for _, run := range runs {
			if run.Success {
				aggregate.SuccessfulRuns++
			} else {
				aggregate.ErrorRuns++
			}
			if run.LatencyEligible {
				aggregate.LatencyValidRuns++
				if run.UsageReceived {
					total = append(total, float64(run.Usage.TotalInputTokens))
					outputs = append(outputs, float64(run.Usage.OutputTokens))
				}
				appendMetric(&activity, run.Timing.ActivityTTFTMS)
				appendMetric(&visible, run.Timing.VisibleTTFTMS)
				appendMetric(&e2e, run.Timing.EndToEndMS)
				if run.Timing.DecodeTokensPerSec != nil || run.Timing.VisibleTokensPerSec != nil {
					aggregate.DecodeValidRuns++
				}
				appendMetric(&decode, run.Timing.DecodeTokensPerSec)
				appendMetric(&visibleDecode, run.Timing.VisibleTokensPerSec)
				appendMetric(&tpot, run.Timing.TPOTMS)
			}
			if run.FitEligible {
				aggregate.FitValidRuns++
				computed = append(computed, float64(run.Usage.ComputedInputTokens))
				if run.Usage.CachedInputTokens != nil {
					cached = append(cached, float64(*run.Usage.CachedInputTokens))
				}
				if run.Usage.CacheWriteTokens != nil {
					writes = append(writes, float64(*run.Usage.CacheWriteTokens))
				}
				if run.Timing.ActivityTTFTMS != nil && *run.Timing.ActivityTTFTMS > 0 {
					rate = append(rate, float64(run.Usage.ComputedInputTokens)/(*run.Timing.ActivityTTFTMS/1000))
				}
			}
		}
		aggregate.TotalInputTokens = medianPtr(total)
		aggregate.ComputedInputTokens = medianPtr(computed)
		aggregate.CachedInputTokens = medianPtr(cached)
		aggregate.CacheWriteTokens = medianPtr(writes)
		aggregate.OutputTokens = medianPtr(outputs)
		aggregate.ActivityTTFTMS = summarize(activity)
		aggregate.VisibleTTFTMS = summarize(visible)
		aggregate.EndToEndMS = summarize(e2e)
		aggregate.DecodeTokensPerSecond = summarize(decode)
		aggregate.VisibleTokensPerSecond = summarize(visibleDecode)
		aggregate.TPOTMS = summarize(tpot)
		aggregate.EffectiveInputTokensRate = summarize(rate)
		out = append(out, aggregate)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].RequestedModel != out[j].RequestedModel {
			return out[i].RequestedModel < out[j].RequestedModel
		}
		if out[i].RequestedInput != out[j].RequestedInput {
			return out[i].RequestedInput < out[j].RequestedInput
		}
		return out[i].Workload < out[j].Workload
	})
	return out
}

func latestMeasuredAttempts(records []RunRecord) []RunRecord {
	latest := make(map[runKey]RunRecord)
	for _, record := range records {
		if record.Phase != "measured" {
			continue
		}
		key := runKey{
			aggregateKey: aggregateKey{record.Provider, record.RequestedModel, record.Workload, record.RequestedInput, record.RequestedOutput},
			run:          record.Run,
		}
		if previous, ok := latest[key]; !ok || record.Attempt > previous.Attempt || (record.Attempt == previous.Attempt && record.Order > previous.Order) {
			latest[key] = record
		}
	}
	out := make([]RunRecord, 0, len(latest))
	for _, record := range latest {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].RequestedModel != out[j].RequestedModel {
			return out[i].RequestedModel < out[j].RequestedModel
		}
		if out[i].RequestedInput != out[j].RequestedInput {
			return out[i].RequestedInput < out[j].RequestedInput
		}
		if out[i].Workload != out[j].Workload {
			return out[i].Workload < out[j].Workload
		}
		return out[i].Run < out[j].Run
	})
	return out
}

func ComputeFits(records []RunRecord) []Fit {
	type point struct {
		input     float64
		ttftSecs  float64
		validRuns int
	}
	type modelKey struct{ provider, model string }
	byTarget := make(map[aggregateKey][]RunRecord)
	for _, record := range latestMeasuredAttempts(records) {
		if record.Workload != "prefill" || !record.FitEligible || record.Timing.ActivityTTFTMS == nil {
			continue
		}
		key := aggregateKey{record.Provider, record.RequestedModel, record.Workload, record.RequestedInput, record.RequestedOutput}
		byTarget[key] = append(byTarget[key], record)
	}
	models := make(map[modelKey][]point)
	for key, runs := range byTarget {
		var inputs, ttfts []float64
		for _, run := range runs {
			inputs = append(inputs, float64(run.Usage.ComputedInputTokens))
			ttfts = append(ttfts, *run.Timing.ActivityTTFTMS/1000)
		}
		models[modelKey{key.provider, key.model}] = append(models[modelKey{key.provider, key.model}], point{
			input:     median(inputs),
			ttftSecs:  median(ttfts),
			validRuns: len(runs),
		})
	}

	ranges := []struct {
		label    string
		min, max int
	}{
		{"1K-16K", 1_000, 16_000},
		{"16K-64K", 16_000, 64_000},
		{"64K-128K", 64_000, 128_000},
		{"128K-512K", 128_000, 512_000},
		{"512K-max", 512_000, math.MaxInt},
	}

	var fits []Fit
	for model, points := range models {
		appendFit := func(label string, selected []point) bool {
			if len(selected) < 3 {
				return false
			}
			sort.Slice(selected, func(i, j int) bool { return selected[i].input < selected[j].input })
			var slopes []float64
			validRuns := 0
			for i := range selected {
				validRuns += selected[i].validRuns
				for j := i + 1; j < len(selected); j++ {
					dx := selected[j].input - selected[i].input
					if dx > 0 {
						slopes = append(slopes, (selected[j].ttftSecs-selected[i].ttftSecs)/dx)
					}
				}
			}
			if len(slopes) == 0 {
				return false
			}
			slope := median(slopes)
			if slope <= 0 {
				return false
			}
			intercepts := make([]float64, 0, len(selected))
			for _, candidate := range selected {
				intercepts = append(intercepts, candidate.ttftSecs-slope*candidate.input)
			}
			intercept := median(intercepts)
			residuals := make([]float64, 0, len(selected))
			for _, candidate := range selected {
				predicted := intercept + slope*candidate.input
				residuals = append(residuals, math.Abs(candidate.ttftSecs-predicted))
			}
			fits = append(fits, Fit{
				Provider:                model.provider,
				RequestedModel:          model.model,
				Range:                   label,
				MinimumInputTokens:      int(math.Round(selected[0].input)),
				MaximumInputTokens:      int(math.Round(selected[len(selected)-1].input)),
				DistinctLengths:         len(selected),
				ValidRuns:               validRuns,
				SlopeSecondsPerToken:    slope,
				EffectiveTokensPerSec:   1 / slope,
				InterceptSeconds:        intercept,
				MedianAbsoluteErrorSecs: median(residuals),
				Method:                  "median_pairwise_slopes",
			})
			return true
		}

		modelHasFit := false
		for rangeIndex, fitRange := range ranges {
			var selected []point
			for _, candidate := range points {
				inRange := candidate.input >= float64(fitRange.min)
				if rangeIndex == len(ranges)-1 {
					inRange = inRange && candidate.input <= float64(fitRange.max)
				} else {
					inRange = inRange && candidate.input < float64(fitRange.max)
				}
				if inRange {
					selected = append(selected, candidate)
				}
			}
			modelHasFit = appendFit(fitRange.label, selected) || modelHasFit
		}
		if !modelHasFit {
			appendFit("observed span", append([]point(nil), points...))
		}
	}
	sort.Slice(fits, func(i, j int) bool {
		left := fmt.Sprintf("%s\x00%s\x00%09d", fits[i].Provider, fits[i].RequestedModel, fits[i].MinimumInputTokens)
		right := fmt.Sprintf("%s\x00%s\x00%09d", fits[j].Provider, fits[j].RequestedModel, fits[j].MinimumInputTokens)
		return left < right
	})
	return fits
}

func summarize(values []float64) Distribution {
	result := Distribution{Count: len(values), Label: "unavailable"}
	if len(values) == 0 {
		return result
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	result.Label = "min_median_max"
	result.Min = floatPtr(sorted[0])
	result.Median = floatPtr(medianSorted(sorted))
	result.Max = floatPtr(sorted[len(sorted)-1])
	if len(sorted) >= 10 {
		result.Label = "min_median_max_p95"
		index := int(math.Ceil(0.95*float64(len(sorted)))) - 1
		result.P95 = floatPtr(sorted[index])
	}
	return result
}

func medianPtr(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	return floatPtr(median(values))
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return medianSorted(sorted)
}

func medianSorted(sorted []float64) float64 {
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func appendMetric(values *[]float64, value *float64) {
	if value != nil {
		*values = append(*values, *value)
	}
}

func aggregateCacheState(runs []RunRecord) string {
	state := ""
	for _, run := range runs {
		if run.CacheState == "unknown" {
			return "unknown"
		}
		if state == "" {
			state = run.CacheState
		} else if state != run.CacheState {
			return "mixed"
		}
	}
	if state == "" {
		return "unknown"
	}
	return state
}
