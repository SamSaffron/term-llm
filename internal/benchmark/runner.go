package benchmark

import (
	"context"
	"fmt"
	"math"
	"math/rand"
)

type scheduledRun struct {
	scenario int
	run      int
}

type RecordCallbackError struct {
	Err error
}

func (e *RecordCallbackError) Error() string { return "write benchmark record: " + e.Err.Error() }
func (e *RecordCallbackError) Unwrap() error { return e.Err }

func (r Runner) runTarget(ctx context.Context, target Target, opts Options) ([]RunRecord, error) {
	if target.Provider == nil {
		return nil, fmt.Errorf("benchmark target %s:%s has no provider", target.ProviderKey, target.RequestedModel)
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("benchmark timeout must be positive")
	}
	if len(opts.Scenarios) == 0 {
		return nil, fmt.Errorf("benchmark has no scenarios")
	}

	records := make([]RunRecord, 0, len(opts.Scenarios)*(opts.Runs+opts.Warmups+1))
	calibratedEstimates := make([]int, len(opts.Scenarios))
	order := 0
	emit := func(record RunRecord) error {
		records = append(records, record)
		if opts.OnRecord != nil {
			if err := opts.OnRecord(record); err != nil {
				return &RecordCallbackError{Err: err}
			}
		}
		return nil
	}

	// Calibrate each distinct scenario with provider-reported input usage, then
	// validate the adjusted payload before any warmup or measured request. The
	// second fresh request distinguishes tokenizer-estimate error from context
	// truncation (notably Ollama silently capping prompt_eval_count). Managed CLI
	// providers in quick/decode mode intentionally skip this: their fixed system
	// overhead and shifting cache buckets are not part of the generated payload
	// target and make provider-total calibration unstable.
	generatedPayloadTarget := UsesGeneratedPayloadTarget(target, opts.Mode)
	for i, scenario := range opts.Scenarios {
		calibratedEstimates[i] = scenario.InputTokens
		if !generatedPayloadTarget {
			order++
			seed := DeriveSeed(opts.Seed, target.ProviderKey, target.RequestedModel, "calibration", i, 1, order)
			record := r.measure(ctx, target, opts, measureSpec{
				phase:          "calibration",
				workload:       scenario.Workload,
				attempt:        1,
				order:          order,
				seed:           seed,
				inputTarget:    scenario.InputTokens,
				outputTarget:   scenario.OutputTokens,
				estimateTarget: scenario.InputTokens,
			})
			if err := emit(record); err != nil {
				return records, err
			}
			if ctx.Err() != nil {
				return records, ctx.Err()
			}
			if !record.Success || record.Usage.TotalInputTokens <= 0 {
				return records, fmt.Errorf("calibration for %s:%s at %d tokens did not return usable provider input tokens: %s", target.ProviderKey, target.RequestedModel, scenario.InputTokens, record.Error)
			}
			ratio := float64(scenario.InputTokens) / float64(record.Usage.TotalInputTokens)
			if ratio < 0.25 || ratio > 4 {
				return records, fmt.Errorf("calibration for %s:%s at %d tokens was implausible (provider reported %d); possible truncation or cache reuse", target.ProviderKey, target.RequestedModel, scenario.InputTokens, record.Usage.TotalInputTokens)
			}
			calibratedEstimates[i] = max(32, int(math.Round(float64(record.LocalEstimate)*ratio)))

			order++
			seed = DeriveSeed(opts.Seed, target.ProviderKey, target.RequestedModel, "calibration", i, 2, order)
			record = r.measure(ctx, target, opts, measureSpec{
				phase:          "calibration",
				workload:       scenario.Workload,
				attempt:        2,
				order:          order,
				seed:           seed,
				inputTarget:    scenario.InputTokens,
				outputTarget:   scenario.OutputTokens,
				estimateTarget: calibratedEstimates[i],
			})
			if err := emit(record); err != nil {
				return records, err
			}
			if ctx.Err() != nil {
				return records, ctx.Err()
			}
			if !record.Success || record.Usage.TotalInputTokens <= 0 || record.TargetMatched == nil || !*record.TargetMatched {
				return records, fmt.Errorf("calibration validation for %s:%s at %d tokens did not match the target (provider reported %d): possible context truncation or cache reuse", target.ProviderKey, target.RequestedModel, scenario.InputTokens, record.Usage.TotalInputTokens)
			}
		}

		for warmup := 0; warmup < opts.Warmups; warmup++ {
			order++
			seed := DeriveSeed(opts.Seed, target.ProviderKey, target.RequestedModel, "warmup", i, warmup, order)
			record := r.measure(ctx, target, opts, measureSpec{
				phase:          "warmup",
				workload:       scenario.Workload,
				run:            warmup + 1,
				attempt:        1,
				order:          order,
				seed:           seed,
				inputTarget:    scenario.InputTokens,
				outputTarget:   scenario.OutputTokens,
				estimateTarget: calibratedEstimates[i],
			})
			if err := emit(record); err != nil {
				return records, err
			}
			if ctx.Err() != nil {
				return records, ctx.Err()
			}
		}
	}

	// Interleave repetitions and randomize target order within each repetition so
	// context length is not confounded with run order.
	schedule := make([]scheduledRun, 0, len(opts.Scenarios)*opts.Runs)
	rng := rand.New(rand.NewSource(DeriveSeed(opts.Seed, target.ProviderKey, target.RequestedModel, "schedule")))
	for run := 1; run <= opts.Runs; run++ {
		for _, scenarioIndex := range rng.Perm(len(opts.Scenarios)) {
			schedule = append(schedule, scheduledRun{scenario: scenarioIndex, run: run})
		}
	}

	for _, scheduled := range schedule {
		scenario := opts.Scenarios[scheduled.scenario]
		retryReason := ""
		for attempt := 1; attempt <= 2; attempt++ {
			order++
			seed := DeriveSeed(opts.Seed, target.ProviderKey, target.RequestedModel, "measured", scheduled.scenario, scheduled.run, attempt, order)
			record := r.measure(ctx, target, opts, measureSpec{
				phase:          "measured",
				workload:       scenario.Workload,
				run:            scheduled.run,
				attempt:        attempt,
				order:          order,
				seed:           seed,
				inputTarget:    scenario.InputTokens,
				outputTarget:   scenario.OutputTokens,
				estimateTarget: calibratedEstimates[scheduled.scenario],
				retryReason:    retryReason,
			})
			if err := emit(record); err != nil {
				return records, err
			}
			if ctx.Err() != nil {
				return records, ctx.Err()
			}
			cacheContaminated := target.Capabilities.ReportsCacheReads && record.CacheRatio != nil && *record.CacheRatio > opts.CacheTolerance
			if !cacheContaminated || attempt == 2 {
				break
			}
			retryReason = "cold_cache_contaminated"
		}
	}

	return records, nil
}
