package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/benchmark"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestResolveBenchmarkTargetsAllActualOllamaModelsFromOpenAICompatibleConfig(t *testing.T) {
	actualModels := []string{"qwen:7b", "gemma:latest", "local/third:Q4"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		models := make([]map[string]string, 0, len(actualModels))
		for _, model := range actualModels {
			models = append(models, map[string]string{"id": model})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": models})
	}))
	defer server.Close()

	cfg := &config.Config{
		DefaultProvider: "ollama",
		Providers: map[string]config.ProviderConfig{
			"ollama": {
				Type:    config.ProviderTypeOpenAICompat,
				BaseURL: server.URL + "/v1",
				Model:   "configured-but-not-exclusive",
			},
		},
	}
	scenarios := []benchmark.Scenario{{InputTokens: 2_000, OutputTokens: 128, Workload: "decode"}}
	plans, skipped, err := resolveBenchmarkTargets(context.Background(), cfg, "ollama", scenarios, "quick", false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || len(plans) != len(actualModels) {
		t.Fatalf("plans=%#v skipped=%v", plans, skipped)
	}
	for i, model := range actualModels {
		if plans[i].target.RequestedModel != model {
			t.Fatalf("model %d = %q, want %q", i, plans[i].target.RequestedModel, model)
		}
		if plans[i].target.ProviderType != string(config.ProviderTypeOpenAICompat) || plans[i].target.Capabilities.ReportsCacheReads || plans[i].target.Capabilities.MeasurementScope != "direct_http" {
			t.Fatalf("Ollama target = %#v", plans[i].target)
		}
	}
}

func TestResolveBenchmarkTargetsInfersExplicitOllamaModelContextFromShow(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tags":
			fmt.Fprintln(w, `{"models":[{"name":"qwen38:latest"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/show":
			fmt.Fprintln(w, `{"capabilities":["completion","thinking","tools"],"parameters":"num_ctx 32768\n","model_info":{"qwen35.context_length":262144}}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {
			Type:  config.ProviderTypeOllama,
			Model: "qwen38:latest",
		},
	}}
	scenarios := []benchmark.Scenario{
		{InputTokens: 4_000, OutputTokens: 128, Workload: "prefill"},
		{InputTokens: 16_000, OutputTokens: 128, Workload: "prefill"},
		{InputTokens: 64_000, OutputTokens: 128, Workload: "prefill"},
	}
	plans, warnings, err := resolveBenchmarkTargets(context.Background(), cfg, "ollama:qwen38:latest", scenarios, "balanced", false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].scenarios) != 2 {
		t.Fatalf("plans = %#v", plans)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "64000") || !strings.Contains(warnings[0], "num_ctx 32768") {
		t.Fatalf("warnings = %v, want 64K target rejected by runtime num_ctx", warnings)
	}
	if plans[0].target.InputLimit != 262144 || plans[0].target.ConfiguredNumCtx != 32768 {
		t.Fatalf("target context metadata = %#v", plans[0].target)
	}
}

func TestResolveBenchmarkTargetsAliasesAndManagedOptIn(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	scenarios := []benchmark.Scenario{{InputTokens: 2_000, OutputTokens: 128, Workload: "decode"}}
	plans, _, err := resolveBenchmarkTargets(context.Background(), cfg, "chatgpt:luna", scenarios, "quick", false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].target.RequestedModel != "gpt-5.6-luna" || !plans[0].target.Capabilities.ReportsCacheReads || plans[0].target.Capabilities.SupportsOutputLimit {
		t.Fatalf("ChatGPT plans = %#v", plans)
	}
	if _, _, err := resolveBenchmarkTargets(context.Background(), cfg, "claude-bin:haiku", scenarios, "quick", false, false, 0); err == nil || !strings.Contains(err.Error(), "--include-managed-provider") {
		t.Fatalf("managed provider error = %v", err)
	}
	plans, _, err = resolveBenchmarkTargets(context.Background(), cfg, "claude-bin:haiku", scenarios, "quick", false, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !plans[0].target.Capabilities.ManagedContext || plans[0].target.Capabilities.IncrementalStream || plans[0].target.Capabilities.SupportsOutputLimit || plans[0].target.Capabilities.SupportsReasoningEffort || plans[0].target.ReasoningExpected {
		t.Fatalf("Claude Haiku target = %#v", plans[0].target)
	}
	plans, _, err = resolveBenchmarkTargets(context.Background(), cfg, "claude-bin:sonnet-high", scenarios, "quick", false, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !plans[0].target.Capabilities.SupportsReasoningEffort || !plans[0].target.ReasoningExpected {
		t.Fatalf("Claude reasoning-effort target = %#v", plans)
	}
}

func TestResolveBenchmarkTargetsSupportsConfiguredVLLM(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "cdck_deepseek",
		Providers: map[string]config.ProviderConfig{
			"cdck_deepseek": {
				Type:          config.ProviderTypeVLLM,
				BaseURL:       "https://vllm.example.test/v1",
				Model:         "deepseek-ai/DeepSeek-V4-Flash",
				ContextWindow: 128_000,
			},
		},
	}
	scenarios := []benchmark.Scenario{{InputTokens: 2_000, OutputTokens: 128, Workload: "decode"}}
	plans, skipped, err := resolveBenchmarkTargets(context.Background(), cfg, "cdck_deepseek", scenarios, "quick", false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || len(plans) != 1 {
		t.Fatalf("plans=%#v skipped=%v", plans, skipped)
	}
	target := plans[0].target
	if target.ProviderType != string(config.ProviderTypeVLLM) || target.RequestedModel != "deepseek-ai/DeepSeek-V4-Flash" {
		t.Fatalf("vLLM target = %#v", target)
	}
	capabilities := target.Capabilities
	if !capabilities.SupportsOutputLimit || !capabilities.SupportsTemperature || !capabilities.SupportsReasoningEffort || !capabilities.IncrementalStream {
		t.Fatalf("vLLM capabilities = %#v", capabilities)
	}
	if !capabilities.ReportsCacheReads || capabilities.ManagedContext || capabilities.MeasurementScope != "direct_http" {
		t.Fatalf("vLLM capabilities = %#v", capabilities)
	}
}

func TestResolveBenchmarkTargetsRequiresOllamaLongContextNumCtx(t *testing.T) {
	numCtx := 64_000
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {Type: config.ProviderTypeOllama, Model: "local-model", NumCtx: &numCtx},
	}}
	scenarios := []benchmark.Scenario{{InputTokens: 96_000, OutputTokens: 16, Workload: "prefill"}}
	plans, _, err := resolveBenchmarkTargets(context.Background(), cfg, "ollama:local-model", scenarios, "long-context", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].target.Error == "" || !strings.Contains(plans[0].target.Error, "context") {
		t.Fatalf("long-context plans = %#v", plans)
	}
}

func TestResolveBenchmarkTargetsUsesAssumedOllamaContextLimit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		providerKey  string
		providerType config.ProviderType
	}{
		{name: "native", providerKey: "local", providerType: config.ProviderTypeOllama},
		{name: "openai compatible", providerKey: "ollama", providerType: config.ProviderTypeOpenAICompat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Providers: map[string]config.ProviderConfig{
				tc.providerKey: {Type: tc.providerType, Model: "local-model"},
			}}
			const assumedLimit = 64_528
			scenarios := []benchmark.Scenario{
				{InputTokens: 16_000, OutputTokens: 16, Workload: "prefill"},
				{InputTokens: 64_000, OutputTokens: 16, Workload: "prefill"},
				{InputTokens: 64_001, OutputTokens: 16, Workload: "prefill"},
			}
			plans, warnings, err := resolveBenchmarkTargets(context.Background(), cfg, tc.providerKey+":local-model", scenarios, "long-context", false, false, assumedLimit)
			if err != nil {
				t.Fatal(err)
			}
			if len(plans) != 1 || plans[0].target.Error != "" || len(plans[0].scenarios) != 2 {
				t.Fatalf("plans = %#v", plans)
			}
			if plans[0].scenarios[1].InputTokens != 64_000 {
				t.Fatalf("boundary scenario was not retained: %#v", plans[0].scenarios)
			}
			if plans[0].target.AssumedContextLimit != assumedLimit || plans[0].target.ConfiguredNumCtx != 0 {
				t.Fatalf("target context metadata = %#v", plans[0].target)
			}
			warningText := strings.Join(warnings, " ")
			if !strings.Contains(warningText, "EXPERT OVERRIDE") || !strings.Contains(warningText, "num_ctx") || !strings.Contains(warningText, "64001") {
				t.Fatalf("warnings = %v", warnings)
			}
		})
	}
}

func TestResolveBenchmarkTargetsRejectsAssumedLimitForNonOllama(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	scenarios := []benchmark.Scenario{{InputTokens: 2_000, OutputTokens: 16, Workload: "prefill"}}
	_, _, err := resolveBenchmarkTargets(context.Background(), cfg, "chatgpt:luna", scenarios, "prefill", true, false, 64_000)
	if err == nil || !strings.Contains(err.Error(), "only supported for local Ollama") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseBenchmarkTokenList(t *testing.T) {
	got, err := parseBenchmarkTokenList("1K, 4_000, 0.096M, 4K")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1_000, 4_000, 96_000}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestParseBenchmarkContextLimit(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{value: "64K", want: 64_000},
		{value: "0.5M", want: 500_000},
		{value: "1_000_000", want: 1_000_000},
	} {
		got, err := parseBenchmarkContextLimit(tc.value)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.value, err)
		}
		if got != tc.want {
			t.Fatalf("parse %q = %d, want %d", tc.value, got, tc.want)
		}
	}
	for _, value := range []string{"", "0", "64G", "64K,128K"} {
		if _, err := parseBenchmarkContextLimit(value); err == nil {
			t.Fatalf("parse %q unexpectedly succeeded", value)
		}
	}
}

func TestBenchmarkCommandHasNoApprovalFlag(t *testing.T) {
	cmd := newBenchmarkCommand()
	if flag := cmd.Flags().Lookup("yes"); flag != nil {
		t.Fatalf("obsolete approval flag is still registered: %s", flag.Name)
	}
}

func TestBenchmarkBudgetDisclosesCalibrationValidationWarmupAndRetry(t *testing.T) {
	budget := benchmark.ComputeBudget(2, []benchmark.Scenario{{InputTokens: 2_000, OutputTokens: 128}}, 3, 1, false)
	// Per target: 2 calibration/validation + 1 warmup + (3 measured * 2 attempts) = 9.
	if budget.MaximumRequests != 18 || budget.MaximumTotalInput != 36_000 || budget.MaximumTotalOutput != 2_304 {
		t.Fatalf("budget = %#v", budget)
	}
}

func TestResolveBenchmarkTargetsMarksChatGPTOutputLimitUnsupported(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	scenarios := []benchmark.Scenario{{InputTokens: 2_000, OutputTokens: 16, Workload: "prefill"}}
	plans, warnings, err := resolveBenchmarkTargets(context.Background(), cfg, "chatgpt:luna", scenarios, "prefill", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].scenarios[0].OutputTokens != 16 || plans[0].target.Capabilities.SupportsOutputLimit {
		t.Fatalf("plans = %#v", plans)
	}
	if strings.Contains(strings.Join(warnings, " "), "provider-safe minimum") {
		t.Fatalf("unsupported output limit unexpectedly raised the requested output: %v", warnings)
	}
}

func TestResolveBenchmarkTargetsUsesConfiguredUpstreamModelAlias(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"custom": {
			Type:  config.ProviderTypeOllama,
			Model: "friendly-high",
			ModelConfigs: []config.ProviderModelConfig{{
				ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"high"},
			}},
		},
	}}
	scenarios := []benchmark.Scenario{{InputTokens: 1_000, OutputTokens: 16, Workload: "prefill"}}
	plans, _, err := resolveBenchmarkTargets(context.Background(), cfg, "custom:friendly-high", scenarios, "prefill", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].target.RequestedModel != "upstream/model-high" {
		t.Fatalf("plans = %#v", plans)
	}
}

func TestExecuteBenchmarkPlansContinuesAndRendersPerModelFailure(t *testing.T) {
	originalFactory := newBenchmarkProvider
	defer func() { newBenchmarkProvider = originalFactory }()
	newBenchmarkProvider = func(_ *config.Config, _ string, model string) (llm.Provider, error) {
		return &benchmarkCommandProvider{model: model, fail: model == "broken"}, nil
	}
	capabilities := benchmark.AdapterCapabilities{ReportsCacheReads: true, SupportsOutputLimit: true}
	plans := []benchmarkTargetPlan{
		{target: benchmark.Target{ProviderKey: "p", ProviderType: "test", RequestedModel: "broken", Capabilities: capabilities}, scenarios: []benchmark.Scenario{{InputTokens: 100, OutputTokens: 1, Workload: "prefill"}}},
		{target: benchmark.Target{ProviderKey: "p", ProviderType: "test", RequestedModel: "working", Capabilities: capabilities}, scenarios: []benchmark.Scenario{{InputTokens: 100, OutputTokens: 1, Workload: "prefill"}}},
	}
	var progress []benchmark.Progress
	opts := benchmark.Options{
		Mode: "prefill", Cache: "cold", Runs: 1, Timeout: time.Second, CacheTolerance: 0.01, TargetTolerance: 0.15,
		OnProgress: func(update benchmark.Progress) { progress = append(progress, update) },
	}
	targets, records, err := executeBenchmarkPlans(context.Background(), &config.Config{}, plans, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].Error == "" || targets[1].Error != "" {
		t.Fatalf("targets = %#v", targets)
	}
	if len(records) != 4 {
		t.Fatalf("records = %#v", records)
	}
	if len(progress) != len(records) {
		t.Fatalf("progress updates = %#v, want one per request", progress)
	}
	if progress[0].ProviderKey != "p" || progress[0].RequestedModel != "broken" || progress[0].Phase != "calibration" || progress[0].Attempt != 1 {
		t.Fatalf("first progress update = %#v", progress[0])
	}
	report := benchmark.NewReport(opts, benchmark.Budget{}, targets, records)
	var out bytes.Buffer
	benchmark.WriteHuman(&out, report)
	if !strings.Contains(out.String(), "Status") || !strings.Contains(out.String(), "working") || !strings.Contains(out.String(), "prefill  100") {
		t.Fatalf("human report = %s", out.String())
	}
}

func TestBenchmarkProgressUpdateIsCompact(t *testing.T) {
	update := benchmarkProgressUpdate(benchmark.Progress{
		ProviderKey: "cdck_deepseek", RequestedModel: "deepseek-ai/DeepSeek-V4-Flash",
		Phase: "measured", Workload: "decode", Run: 2, Attempt: 2, InputTokens: 2_000, OutputTokens: 128,
	})
	if update.Phase != "Benchmarking" {
		t.Fatalf("progress phase = %q", update.Phase)
	}
	for _, want := range []string{"run 2 retry 1", "decode", "2.0k in / 128 out"} {
		if !strings.Contains(update.Status, want) {
			t.Fatalf("progress status %q missing %q", update.Status, want)
		}
	}
	if strings.Contains(update.Status, "cdck_deepseek") || strings.Contains(update.Status, "DeepSeek") || len(update.Status) > 60 {
		t.Fatalf("progress status is too noisy: %q", update.Status)
	}
}

func TestWriteBenchmarkDryRunJSONIsSingleCleanDocument(t *testing.T) {
	plans := []benchmarkTargetPlan{{target: benchmark.Target{ProviderKey: "ollama", RequestedModel: "model", AssumedContextLimit: 64_000}, scenarios: []benchmark.Scenario{{InputTokens: 100, OutputTokens: 1}}}}
	var out bytes.Buffer
	if err := writeBenchmarkDryRun(&out, true, plans, benchmark.Budget{MaximumRequests: 3}); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	if decoded["dry_run"] != true || decoded["assumed_context_limit"] != float64(64_000) {
		t.Fatalf("decoded = %#v", decoded)
	}
	limitations, ok := decoded["limitations"].([]any)
	if !ok || len(limitations) != 1 || !strings.Contains(limitations[0].(string), "num_ctx") {
		t.Fatalf("limitations = %#v", decoded["limitations"])
	}
}

func TestOpenBenchmarkJSONLAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openBenchmarkJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "second\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\nsecond\n" {
		t.Fatalf("JSONL = %q", got)
	}
}

type benchmarkCommandProvider struct {
	model string
	fail  bool
}

func (p *benchmarkCommandProvider) Name() string                   { return p.model }
func (p *benchmarkCommandProvider) Credential() string             { return "mock" }
func (p *benchmarkCommandProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *benchmarkCommandProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	if p.fail {
		return nil, fmt.Errorf("model unavailable")
	}
	return &benchmarkCommandStream{events: []llm.Event{
		{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 100, OutputTokens: 1}},
		{Type: llm.EventDone},
	}}, nil
}

type benchmarkCommandStream struct {
	events []llm.Event
	next   int
}

func (s *benchmarkCommandStream) Recv() (llm.Event, error) {
	if s.next >= len(s.events) {
		return llm.Event{}, io.EOF
	}
	event := s.events[s.next]
	s.next++
	return event, nil
}
func (*benchmarkCommandStream) Close() error { return nil }
