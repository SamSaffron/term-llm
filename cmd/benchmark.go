package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/benchmark"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

type benchmarkFlags struct {
	provider               string
	mode                   string
	cache                  string
	allowUnknownCache      bool
	inputTokens            string
	assumeContextLimit     string
	outputTokens           int
	runs                   int
	warmups                int
	concurrency            int
	seed                   int64
	reasoningEffort        string
	temperature            float32
	timeout                time.Duration
	cacheTolerance         float64
	targetTolerance        float64
	json                   bool
	jsonl                  string
	dryRun                 bool
	includeManagedProvider bool
}

type benchmarkTargetPlan struct {
	target    benchmark.Target
	scenarios []benchmark.Scenario
}

func init() {
	rootCmd.AddCommand(newBenchmarkCommand())
}

func newBenchmarkCommand() *cobra.Command {
	flags := benchmarkFlags{}
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Measure client-observed LLM streaming and input-processing performance",
		Long: `Benchmark provider inference without the agent engine, tools, sessions, or search.

Timings are measured at the llm.Provider/llm.Stream boundary. They include client,
network, queueing, tokenization, and transport overhead and are not server telemetry.
Cold-prefix token fits use provider-reported counts; unsupported cache or decode
metrics remain explicitly unavailable rather than being inferred from stream chunks.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBenchmark(cmd, flags)
		},
	}
	cmd.Flags().StringVarP(&flags.provider, "provider", "p", "", "Provider, optionally with model; Ollama without a model benchmarks all configured models")
	if err := cmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic("failed to register benchmark provider completion: " + err.Error())
	}
	cmd.Flags().StringVar(&flags.mode, "mode", "balanced", "Workload mode: balanced, quick, decode, prefill, or long-context")
	cmd.Flags().StringVar(&flags.cache, "cache", "cold", "Cache workload: cold (warm is reserved for a future separate workload)")
	cmd.Flags().BoolVar(&flags.allowUnknownCache, "allow-unknown-cache", false, "Allow token fits when the adapter cannot report cache reads (results stay labeled unknown)")
	cmd.Flags().StringVar(&flags.inputTokens, "input-tokens", "", "Comma-separated input-token targets (supports K/M suffixes)")
	cmd.Flags().StringVar(&flags.assumeContextLimit, "assume-context-limit", "", "Expert safety override for an unknown local Ollama context limit (supports K/M suffixes; does not configure num_ctx)")
	cmd.Flags().IntVar(&flags.outputTokens, "output-tokens", 0, "Requested output-token ceiling (0 uses the mode profile)")
	cmd.Flags().IntVar(&flags.runs, "runs", -1, "Measured runs per scenario (-1 uses the mode profile)")
	cmd.Flags().IntVar(&flags.warmups, "warmup", -1, "Warmup runs per scenario (-1 uses the mode profile)")
	cmd.Flags().IntVar(&flags.concurrency, "concurrency", 1, "Concurrent requests (schema version 1 supports only 1)")
	cmd.Flags().Int64Var(&flags.seed, "seed", 42, "Deterministic workload seed")
	cmd.Flags().StringVar(&flags.reasoningEffort, "reasoning-effort", "", "Explicit provider reasoning effort")
	cmd.Flags().Float32Var(&flags.temperature, "temperature", 0, "Explicit sampling temperature (only sent when the adapter supports it)")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", 10*time.Minute, "Hard timeout per request")
	cmd.Flags().Float64Var(&flags.cacheTolerance, "cache-tolerance", 0.01, "Maximum cache-read ratio accepted as cold")
	cmd.Flags().Float64Var(&flags.targetTolerance, "target-tolerance", 0.15, "Maximum relative provider-token deviation from an input target")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Emit one complete JSON report")
	cmd.Flags().StringVar(&flags.jsonl, "jsonl", "", "Append each raw request record to this JSONL file as it completes")
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Resolve and list every benchmark target without dispatching inference requests")
	cmd.Flags().BoolVar(&flags.includeManagedProvider, "include-managed-provider", false, "Allow subprocess/CLI providers and label timing subprocess-inclusive")
	return cmd
}

func runBenchmark(cmd *cobra.Command, flags benchmarkFlags) error {
	if flags.cache != "cold" {
		return fmt.Errorf("benchmark cache mode %q is not implemented; schema version 1 keeps warm-cache workloads separate", flags.cache)
	}
	if flags.concurrency != 1 {
		return fmt.Errorf("benchmark concurrency %d is not implemented; schema version 1 supports only sequential concurrency=1", flags.concurrency)
	}
	if flags.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if flags.cacheTolerance < 0 || flags.cacheTolerance >= 1 {
		return fmt.Errorf("--cache-tolerance must be in [0, 1)")
	}
	if flags.targetTolerance <= 0 || flags.targetTolerance >= 1 {
		return fmt.Errorf("--target-tolerance must be in (0, 1)")
	}

	scenarios, profileRuns, profileWarmups, err := benchmark.DefaultProfile(flags.mode)
	if err != nil {
		return err
	}
	if flags.runs < -1 || flags.warmups < -1 {
		return fmt.Errorf("--runs and --warmup must be non-negative or -1 for profile defaults")
	}
	if flags.runs == -1 {
		flags.runs = profileRuns
	}
	if flags.warmups == -1 {
		flags.warmups = profileWarmups
	}
	if flags.runs < 1 {
		return fmt.Errorf("--runs must be at least 1")
	}
	if flags.inputTokens != "" {
		inputs, err := parseBenchmarkTokenList(flags.inputTokens)
		if err != nil {
			return fmt.Errorf("--input-tokens: %w", err)
		}
		output := flags.outputTokens
		if output == 0 {
			output = defaultBenchmarkOutput(flags.mode)
		}
		scenarios = benchmark.ScenariosForOverride(flags.mode, inputs, output)
	} else if flags.outputTokens > 0 {
		for i := range scenarios {
			scenarios[i].OutputTokens = flags.outputTokens
		}
	}
	if flags.outputTokens < 0 {
		return fmt.Errorf("--output-tokens must be non-negative")
	}
	assumedContextLimit := 0
	if strings.TrimSpace(flags.assumeContextLimit) != "" {
		assumedContextLimit, err = parseBenchmarkContextLimit(flags.assumeContextLimit)
		if err != nil {
			return fmt.Errorf("--assume-context-limit: %w", err)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	plans, skipped, err := resolveBenchmarkTargets(cmd.Context(), cfg, flags.provider, scenarios, flags.mode, flags.inputTokens != "", flags.includeManagedProvider, assumedContextLimit)
	if err != nil {
		return err
	}
	for _, warning := range skipped {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	for i := range plans {
		plan := &plans[i]
		if plan.target.Error != "" {
			continue
		}
		if flags.reasoningEffort != "" && !plan.target.Capabilities.SupportsReasoningEffort {
			plan.target.Error = fmt.Sprintf("does not support --reasoning-effort through its benchmark adapter")
			continue
		}
		if cmd.Flags().Changed("temperature") && !plan.target.Capabilities.SupportsTemperature {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s:%s does not support benchmark temperature control; --temperature will not be sent and is null in results\n", plan.target.ProviderKey, plan.target.RequestedModel)
		}
	}

	outputNotGuaranteed := false
	budget := benchmark.Budget{}
	for _, plan := range plans {
		if plan.target.Error != "" {
			continue
		}
		if !plan.target.Capabilities.SupportsOutputLimit {
			outputNotGuaranteed = true
		}
		part := benchmark.ComputeBudgetForTarget(plan.target, flags.mode, plan.scenarios, flags.runs, flags.warmups, !plan.target.Capabilities.SupportsOutputLimit)
		addBenchmarkBudget(&budget, part)
	}
	budget.OutputBudgetIsRequested = outputNotGuaranteed
	budget.CacheWriteBillingRisk = true
	budget.Notes = []string{
		"Input totals are maximum requested provider tokens, not billed-token predictions.",
		"Cache-write tokens count as computed input and may have provider-specific billing.",
	}
	budgetWriter := cmd.OutOrStdout()
	if flags.json {
		budgetWriter = cmd.ErrOrStderr()
	}
	benchmark.WriteBudget(budgetWriter, flags.mode, budget)
	if flags.dryRun {
		return writeBenchmarkDryRun(cmd.OutOrStdout(), flags.json, plans, budget)
	}
	fmt.Fprintln(budgetWriter)

	var jsonlFile *os.File
	var jsonlEncoder *json.Encoder
	if flags.jsonl != "" {
		jsonlFile, err = openBenchmarkJSONL(flags.jsonl)
		if err != nil {
			return err
		}
		defer jsonlFile.Close()
		jsonlEncoder = json.NewEncoder(jsonlFile)
	}

	opts := benchmark.Options{
		Mode:                flags.mode,
		Cache:               flags.cache,
		Runs:                flags.runs,
		Warmups:             flags.warmups,
		Concurrency:         flags.concurrency,
		Seed:                flags.seed,
		ReasoningEffort:     flags.reasoningEffort,
		Temperature:         flags.temperature,
		TemperatureSet:      cmd.Flags().Changed("temperature"),
		Timeout:             flags.timeout,
		CacheTolerance:      flags.cacheTolerance,
		TargetTolerance:     flags.targetTolerance,
		AllowUnknownCache:   flags.allowUnknownCache,
		AssumedContextLimit: assumedContextLimit,
		TermLLMVersion:      versionString(),
	}
	progressCh := make(chan ui.ProgressUpdate, 1)
	progressCh <- ui.ProgressUpdate{Phase: "Benchmarking", Status: "starting"}
	opts.OnProgress = func(progress benchmark.Progress) {
		update := benchmarkProgressUpdate(progress)
		select {
		case progressCh <- update:
		case <-cmd.Context().Done():
		}
	}
	if jsonlEncoder != nil {
		opts.OnRecord = func(record benchmark.RunRecord) error {
			return jsonlEncoder.Encode(record)
		}
	}

	type executionResult struct {
		targets []benchmark.Target
		records []benchmark.RunRecord
	}
	value, err := ui.RunWithSpinnerProgress(cmd.Context(), false, progressCh, func(ctx context.Context) (any, error) {
		defer close(progressCh)
		targets, records, runErr := executeBenchmarkPlans(ctx, cfg, plans, opts, flags.includeManagedProvider)
		return executionResult{targets: targets, records: records}, runErr
	})
	if err != nil {
		return err
	}
	result, ok := value.(executionResult)
	if !ok {
		return fmt.Errorf("benchmark execution returned unexpected result %T", value)
	}
	targets, allRecords := result.targets, result.records

	report := benchmark.NewReport(opts, budget, targets, allRecords)
	if flags.json {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	benchmark.WriteHuman(cmd.OutOrStdout(), report)
	return nil
}

func benchmarkProgressUpdate(progress benchmark.Progress) ui.ProgressUpdate {
	phase := progress.Phase
	switch progress.Phase {
	case "calibration":
		if progress.Attempt == 2 {
			phase = "validation"
		}
	case "warmup":
		phase = fmt.Sprintf("warmup %d", progress.Run)
	case "measured":
		phase = fmt.Sprintf("run %d", progress.Run)
		if progress.Attempt > 1 {
			phase += fmt.Sprintf(" retry %d", progress.Attempt-1)
		}
	}
	return ui.ProgressUpdate{
		Phase: "Benchmarking",
		Status: fmt.Sprintf("%s · %s · %s in / %s out",
			phase, progress.Workload, ui.FormatTokenCount(progress.InputTokens), ui.FormatTokenCount(progress.OutputTokens)),
	}
}

func openBenchmarkJSONL(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open benchmark JSONL for append: %w", err)
	}
	return file, nil
}

var newBenchmarkProvider = llm.NewProviderByNameNoRetry

func executeBenchmarkPlans(ctx context.Context, cfg *config.Config, plans []benchmarkTargetPlan, opts benchmark.Options, includeManaged bool) ([]benchmark.Target, []benchmark.RunRecord, error) {
	allRecords := make([]benchmark.RunRecord, 0)
	targets := make([]benchmark.Target, 0, len(plans))
	runner := benchmark.Runner{}
	for _, plan := range plans {
		target := plan.target
		if target.Error != "" {
			targets = append(targets, target)
			continue
		}

		provider, err := newBenchmarkProvider(cfg, target.ProviderKey, target.RequestedModel)
		if err != nil {
			target.Error = fmt.Sprintf("create provider: %v", err)
			targets = append(targets, target)
			continue
		}
		target.Provider = provider
		cleanup := func() {
			if cleaner, ok := provider.(interface{ CleanupMCP() }); ok {
				cleaner.CleanupMCP()
			}
		}
		if provider.Capabilities().ManagesOwnContext && !includeManaged {
			target.Error = "provider manages its own context; use --include-managed-provider for subprocess-inclusive timing"
			cleanup()
			targets = append(targets, target)
			continue
		}

		targetOpts := opts
		targetOpts.Scenarios = plan.scenarios
		records, runErr := runner.RunTarget(ctx, target, targetOpts)
		allRecords = append(allRecords, records...)
		cleanup()
		if runErr != nil {
			var callbackErr *benchmark.RecordCallbackError
			if errors.As(runErr, &callbackErr) || ctx.Err() != nil {
				return targets, allRecords, runErr
			}
			target.Error = runErr.Error()
		}
		targets = append(targets, target)
	}
	return targets, allRecords, nil
}

func resolveBenchmarkTargets(ctx context.Context, cfg *config.Config, providerFlag string, scenarios []benchmark.Scenario, mode string, explicitInputs, includeManaged bool, assumedContextLimit int) ([]benchmarkTargetPlan, []string, error) {
	providerKey := cfg.DefaultProvider
	model := ""
	if providerFlag != "" {
		var err error
		providerKey, model, err = llm.ParseProviderModel(providerFlag, cfg)
		if err != nil {
			return nil, nil, err
		}
	}
	providerCfg, configured := cfg.Providers[providerKey]
	providerType := config.InferProviderType(providerKey, providerCfg.Type)
	localOllama := benchmarkUsesLocalOllama(providerKey, providerType)
	if assumedContextLimit > 0 && !localOllama {
		return nil, nil, fmt.Errorf("--assume-context-limit is only supported for local Ollama providers (native or OpenAI-compatible Ollama)")
	}
	capabilities, ok := benchmarkAdapterCapabilities(providerType, localOllama)
	if !ok {
		return nil, nil, fmt.Errorf("provider %q (type %s) is not supported by benchmark schema version 1; supported adapters: local Ollama (native or OpenAI-compatible), vLLM, chatgpt, claude-bin", providerKey, providerType)
	}
	if capabilities.ManagedContext && !includeManaged {
		return nil, nil, fmt.Errorf("provider %q manages its own context; use --include-managed-provider to opt into subprocess-inclusive timing", providerKey)
	}
	if providerType == config.ProviderTypeChatGPT && strings.EqualFold(model, "luna") {
		model = "gpt-5.6-luna"
	}

	models := []string{model}
	modelInfoByID := make(map[string]llm.ModelInfo)
	var skipped []string
	listOllamaModels := localOllama && (model == "" || providerType == config.ProviderTypeOllama)
	if listOllamaModels {
		listed, err := listBenchmarkModels(ctx, cfg, providerKey)
		if err != nil {
			if model == "" {
				return nil, nil, fmt.Errorf("list local Ollama models for %q: %w", providerKey, err)
			}
			skipped = append(skipped, fmt.Sprintf("could not inspect local Ollama model metadata for %s:%s: %v", providerKey, model, err))
		} else {
			if model == "" {
				models = models[:0]
			}
			for _, info := range listed {
				id := strings.TrimSpace(info.ID)
				if id == "" {
					continue
				}
				if model == "" {
					models = append(models, id)
				}
				modelInfoByID[id] = info
			}
			if model == "" && len(models) == 0 {
				return nil, nil, fmt.Errorf("local Ollama provider %q returned no installed models", providerKey)
			}
		}
	}
	if len(models) == 0 || (len(models) == 1 && strings.TrimSpace(models[0]) == "") {
		fallback := strings.TrimSpace(providerCfg.Model)
		if fallback == "" {
			fallback = config.DefaultProviderModel(string(providerType))
		}
		models = []string{fallback}
	}
	models = uniqueNonEmpty(models)
	if len(models) == 0 {
		if configured {
			return nil, nil, fmt.Errorf("provider %q has no configured model", providerKey)
		}
		return nil, nil, fmt.Errorf("provider %q requires an explicit model", providerKey)
	}

	var plans []benchmarkTargetPlan
	if assumedContextLimit > 0 {
		skipped = append(skipped, benchmark.AssumedContextLimitLimitation(assumedContextLimit))
	}
	for _, selectedModel := range models {
		selectedModel = config.UpstreamModelForProviderModel(cfg, providerKey, selectedModel)
		inputLimit := llm.InputLimitForProviderModel(providerKey, selectedModel)
		discoveredNumCtx := 0
		if info := modelInfoByID[selectedModel]; info.ID != "" {
			if info.InputLimit > 0 {
				inputLimit = info.InputLimit
			}
			if localOllama {
				discoveredNumCtx = info.ConfiguredContext
			}
		}
		configuredContext := providerCfg.ContextWindow
		if modelConfig, ok := config.ModelConfigForProviderModel(cfg, providerKey, selectedModel); ok && modelConfig.ContextWindow > 0 {
			configuredContext = modelConfig.ContextWindow
			inputLimit = modelConfig.ContextWindow
			if modelConfig.MaxOutputTokens > 0 && modelConfig.MaxOutputTokens < modelConfig.ContextWindow {
				inputLimit = modelConfig.ContextWindow - modelConfig.MaxOutputTokens
			}
		}
		if providerCfg.NumCtx != nil && *providerCfg.NumCtx > 0 {
			configuredContext = *providerCfg.NumCtx
		}
		effectiveOllamaContext := configuredContext
		contextLimitSource := "configured Ollama context"
		if effectiveOllamaContext == 0 && discoveredNumCtx > 0 {
			effectiveOllamaContext = discoveredNumCtx
			contextLimitSource = "Ollama model num_ctx"
		}
		if assumedContextLimit > 0 && (effectiveOllamaContext == 0 || assumedContextLimit < effectiveOllamaContext) {
			effectiveOllamaContext = assumedContextLimit
			contextLimitSource = "assumed Ollama context limit"
		}
		filtered := make([]benchmark.Scenario, 0, len(scenarios))
		for _, scenario := range scenarios {
			if capabilities.MinimumOutputTokens > 0 && scenario.OutputTokens < capabilities.MinimumOutputTokens {
				skipped = append(skipped, fmt.Sprintf("raising %s:%s output ceiling from %d to provider-safe minimum %d", providerKey, selectedModel, scenario.OutputTokens, capabilities.MinimumOutputTokens))
				scenario.OutputTokens = capabilities.MinimumOutputTokens
			}
			if inputLimit > 0 && scenario.InputTokens+512 > inputLimit {
				skipped = append(skipped, fmt.Sprintf("skipping %s:%s target %d above conservative effective input limit %d", providerKey, selectedModel, scenario.InputTokens, inputLimit))
				continue
			}
			if localOllama && effectiveOllamaContext > 0 && scenario.InputTokens+scenario.OutputTokens+512 > effectiveOllamaContext {
				skipped = append(skipped, fmt.Sprintf("skipping %s:%s target %d because input + output/template headroom exceeds %s %d", providerKey, selectedModel, scenario.InputTokens, contextLimitSource, effectiveOllamaContext))
				continue
			}
			if localOllama && effectiveOllamaContext == 0 && scenario.InputTokens+scenario.OutputTokens+512 > 4_096 {
				skipped = append(skipped, fmt.Sprintf("skipping %s:%s target %d because Ollama context is not configured; only requests fitting the conservative 4096-token floor are safe", providerKey, selectedModel, scenario.InputTokens))
				continue
			}
			filtered = append(filtered, scenario)
		}
		targetCapabilities := capabilities
		reasoningExpected := providerType == config.ProviderTypeChatGPT || strings.HasSuffix(strings.ToLower(selectedModel), "-think")
		if providerType == config.ProviderTypeClaudeBin {
			_, modelEffort := llm.BaseModelAndEffortForProvider(string(providerType), selectedModel)
			reasoningExpected = modelEffort != ""
			targetCapabilities.SupportsReasoningEffort = len(llm.ReasoningEffortsForProviderModel(string(providerType), selectedModel)) > 0
		}
		if (providerCfg.Think != nil && *providerCfg.Think) || strings.TrimSpace(providerCfg.ThinkLevel) != "" {
			reasoningExpected = true
		}
		reportedNumCtx := 0
		if localOllama {
			reportedNumCtx = configuredContext
			if reportedNumCtx == 0 {
				reportedNumCtx = discoveredNumCtx
			}
		}
		target := benchmark.Target{
			ProviderKey:         providerKey,
			ProviderType:        string(providerType),
			RequestedModel:      selectedModel,
			Capabilities:        targetCapabilities,
			InputLimit:          inputLimit,
			ConfiguredNumCtx:    reportedNumCtx,
			AssumedContextLimit: assumedContextLimit,
			ServiceTier:         providerCfg.ServiceTier,
			ReasoningExpected:   reasoningExpected,
		}
		if len(filtered) == 0 {
			target.Error = "no benchmark input targets fit the model/context limits"
		}
		if target.Error == "" && mode == "long-context" && inputLimit == 0 && effectiveOllamaContext == 0 && !explicitInputs {
			target.Error = "long-context benchmark requires a known input limit or explicit --input-tokens"
		}
		plans = append(plans, benchmarkTargetPlan{target: target, scenarios: filtered})
	}
	return plans, skipped, nil
}

func benchmarkUsesLocalOllama(providerKey string, providerType config.ProviderType) bool {
	return providerType == config.ProviderTypeOllama ||
		(strings.EqualFold(strings.TrimSpace(providerKey), "ollama") &&
			(providerType == config.ProviderTypeOpenAICompat || providerType == config.ProviderTypeVLLM))
}

func listBenchmarkModels(ctx context.Context, cfg *config.Config, providerKey string) ([]llm.ModelInfo, error) {
	provider, err := newBenchmarkProvider(cfg, providerKey, "")
	if err != nil {
		return nil, err
	}
	if cleaner, ok := provider.(interface{ CleanupMCP() }); ok {
		defer cleaner.CleanupMCP()
	}
	lister, ok := provider.(interface {
		ListModels(context.Context) ([]llm.ModelInfo, error)
	})
	if !ok {
		return nil, fmt.Errorf("adapter %T cannot list models", provider)
	}
	return lister.ListModels(ctx)
}

func writeBenchmarkDryRun(w io.Writer, jsonOutput bool, plans []benchmarkTargetPlan, budget benchmark.Budget) error {
	type dryRunTarget struct {
		Provider            string               `json:"provider"`
		Model               string               `json:"model"`
		AssumedContextLimit int                  `json:"assumed_context_limit,omitempty"`
		Scenarios           []benchmark.Scenario `json:"scenarios,omitempty"`
		Error               string               `json:"error,omitempty"`
	}
	targets := make([]dryRunTarget, 0, len(plans))
	assumedContextLimit := 0
	for _, plan := range plans {
		targets = append(targets, dryRunTarget{
			Provider:            plan.target.ProviderKey,
			Model:               plan.target.RequestedModel,
			AssumedContextLimit: plan.target.AssumedContextLimit,
			Scenarios:           plan.scenarios,
			Error:               plan.target.Error,
		})
		if plan.target.AssumedContextLimit > 0 {
			assumedContextLimit = plan.target.AssumedContextLimit
		}
	}
	limitations := []string(nil)
	if assumedContextLimit > 0 {
		limitations = append(limitations, benchmark.AssumedContextLimitLimitation(assumedContextLimit))
	}
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			DryRun              bool             `json:"dry_run"`
			AssumedContextLimit int              `json:"assumed_context_limit,omitempty"`
			Budget              benchmark.Budget `json:"budget"`
			Targets             []dryRunTarget   `json:"targets"`
			Limitations         []string         `json:"limitations,omitempty"`
		}{DryRun: true, AssumedContextLimit: assumedContextLimit, Budget: budget, Targets: targets, Limitations: limitations})
	}
	if assumedContextLimit > 0 {
		fmt.Fprintf(w, "\nWARNING: %s\n", benchmark.AssumedContextLimitLimitation(assumedContextLimit))
	}
	fmt.Fprintf(w, "\nResolved benchmark targets (%d; no inference dispatched):\n", len(targets))
	for _, target := range targets {
		if target.Error != "" {
			fmt.Fprintf(w, "  %s:%s — unavailable: %s\n", target.Provider, target.Model, target.Error)
			continue
		}
		fmt.Fprintf(w, "  %s:%s (%d scenarios)\n", target.Provider, target.Model, len(target.Scenarios))
	}
	return nil
}

func benchmarkAdapterCapabilities(providerType config.ProviderType, localOllama bool) (benchmark.AdapterCapabilities, bool) {
	if localOllama {
		return benchmark.AdapterCapabilities{
			ReportsCacheReads:       false,
			ReportsCacheWrites:      false,
			ReportsReasoningTokens:  false,
			SupportsOutputLimit:     true,
			SupportsTemperature:     true,
			SupportsReasoningEffort: false,
			MinimumOutputTokens:     1,
			IncrementalStream:       true,
			MeasurementScope:        "direct_http",
			CacheTelemetryNote:      "Ollama prompt token counts do not distinguish cache reads; state remains unknown",
			OutputLimitSupportNote:  "Ollama num_predict or OpenAI-compatible max_tokens",
		}, true
	}
	switch providerType {
	case config.ProviderTypeVLLM:
		return benchmark.AdapterCapabilities{
			ReportsCacheReads:       true,
			ReportsCacheWrites:      false,
			ReportsReasoningTokens:  false,
			SupportsOutputLimit:     true,
			SupportsTemperature:     true,
			SupportsReasoningEffort: true,
			MinimumOutputTokens:     1,
			IncrementalStream:       true,
			MeasurementScope:        "direct_http",
			CacheTelemetryNote:      "OpenAI-compatible prompt_tokens_details.cached_tokens reports prefix-cache reads",
			OutputLimitSupportNote:  "OpenAI-compatible max_tokens",
		}, true
	case config.ProviderTypeChatGPT:
		return benchmark.AdapterCapabilities{
			ReportsCacheReads:       true,
			ReportsCacheWrites:      true,
			ReportsReasoningTokens:  true,
			SupportsOutputLimit:     false,
			SupportsTemperature:     false,
			SupportsReasoningEffort: true,
			MinimumOutputTokens:     1,
			IncrementalStream:       true,
			MeasurementScope:        "direct_api",
			CacheTelemetryNote:      "Responses input_tokens_details.cached_tokens",
			OutputLimitSupportNote:  "ChatGPT backend route rejects max_output_tokens; benchmark requests one bounded natural completion with an explicit end marker",
		}, true
	case config.ProviderTypeClaudeBin:
		return benchmark.AdapterCapabilities{
			ReportsCacheReads:       false,
			ReportsCacheWrites:      true,
			ReportsReasoningTokens:  false,
			SupportsOutputLimit:     false,
			SupportsTemperature:     false,
			SupportsReasoningEffort: true,
			MinimumOutputTokens:     16,
			IncrementalStream:       false,
			ManagedContext:          true,
			MeasurementScope:        "subprocess_inclusive",
			CacheTelemetryNote:      "CLI-backed usage is not treated as trustworthy cache-state telemetry",
			OutputLimitSupportNote:  "claude-bin does not map Request.MaxOutputTokens to the CLI; benchmark requests one bounded natural completion with an explicit end marker",
		}, true
	default:
		return benchmark.AdapterCapabilities{}, false
	}
}

func parseBenchmarkContextLimit(value string) (int, error) {
	if strings.Contains(value, ",") {
		return 0, fmt.Errorf("must be one token count")
	}
	values, err := parseBenchmarkTokenList(value)
	if err != nil {
		return 0, err
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("must be one token count")
	}
	return values[0], nil
}

func parseBenchmarkTokenList(value string) ([]int, error) {
	seen := make(map[int]bool)
	var result []int
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(strings.ReplaceAll(raw, "_", ""))
		if raw == "" {
			return nil, fmt.Errorf("empty token target")
		}
		multiplier := float64(1)
		suffix := raw[len(raw)-1]
		if suffix == 'k' || suffix == 'K' {
			multiplier = 1_000
			raw = raw[:len(raw)-1]
		} else if suffix == 'm' || suffix == 'M' {
			multiplier = 1_000_000
			raw = raw[:len(raw)-1]
		}
		number, err := strconv.ParseFloat(raw, 64)
		if err != nil || number <= 0 {
			return nil, fmt.Errorf("invalid token target %q", raw)
		}
		tokens := int(number * multiplier)
		if tokens < 32 {
			return nil, fmt.Errorf("token target %d is too small (minimum 32)", tokens)
		}
		if !seen[tokens] {
			seen[tokens] = true
			result = append(result, tokens)
		}
	}
	sort.Ints(result)
	return result, nil
}

func defaultBenchmarkOutput(mode string) int {
	switch mode {
	case "quick":
		return 128
	case "decode":
		return 256
	default:
		return 16
	}
}

func uniqueNonEmpty(values []string) []string {
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
	return out
}

func addBenchmarkBudget(total *benchmark.Budget, part benchmark.Budget) {
	total.MaximumRequests += part.MaximumRequests
	total.MaximumTotalInput += part.MaximumTotalInput
	total.MaximumTotalOutput += part.MaximumTotalOutput
	total.MaximumRequestInput = max(total.MaximumRequestInput, part.MaximumRequestInput)
	total.MaximumRequestOutput = max(total.MaximumRequestOutput, part.MaximumRequestOutput)
	total.IncludesCalibration = total.IncludesCalibration || part.IncludesCalibration
	total.IncludesWarmups = total.IncludesWarmups || part.IncludesWarmups
	total.IncludesRetryAllowance = total.IncludesRetryAllowance || part.IncludesRetryAllowance
	total.CacheWriteBillingRisk = total.CacheWriteBillingRisk || part.CacheWriteBillingRisk
}
