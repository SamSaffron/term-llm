package benchmark

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

const SchemaVersion = 1

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type AdapterCapabilities struct {
	ReportsCacheReads       bool   `json:"reports_cache_reads"`
	ReportsCacheWrites      bool   `json:"reports_cache_writes"`
	ReportsReasoningTokens  bool   `json:"reports_reasoning_tokens"`
	SupportsOutputLimit     bool   `json:"supports_output_limit"`
	SupportsTemperature     bool   `json:"supports_temperature"`
	SupportsReasoningEffort bool   `json:"supports_reasoning_effort"`
	MinimumOutputTokens     int    `json:"minimum_safe_output_tokens,omitempty"`
	IncrementalStream       bool   `json:"incremental_stream"`
	ManagedContext          bool   `json:"managed_context"`
	MeasurementScope        string `json:"measurement_scope"`
	CacheTelemetryNote      string `json:"cache_telemetry_note"`
	OutputLimitSupportNote  string `json:"output_limit_support_note,omitempty"`
}

type Target struct {
	ProviderKey         string
	ProviderType        string
	RequestedModel      string
	Provider            llm.Provider
	Capabilities        AdapterCapabilities
	InputLimit          int
	ConfiguredNumCtx    int
	AssumedContextLimit int
	ServiceTier         string
	ReasoningExpected   bool
	Error               string
}

type Scenario struct {
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Workload     string `json:"workload"`
}

// UsesGeneratedPayloadTarget reports whether input targets describe the locally
// generated user payload instead of provider-total input usage. Managed CLI
// adapters add variable system/context overhead, so provider-total calibration
// is not meaningful for their quick/decode latency workloads.
func UsesGeneratedPayloadTarget(target Target, mode string) bool {
	return target.Capabilities.ManagedContext && (mode == "quick" || mode == "decode")
}

func inputTargetMeaning(target Target, mode string) string {
	if UsesGeneratedPayloadTarget(target, mode) {
		return "generated_user_payload"
	}
	return "provider_total_calibrated"
}

type Options struct {
	Mode                string
	Cache               string
	Scenarios           []Scenario
	Runs                int
	Warmups             int
	Concurrency         int
	Seed                int64
	ReasoningEffort     string
	Temperature         float32
	TemperatureSet      bool
	Timeout             time.Duration
	CacheTolerance      float64
	TargetTolerance     float64
	AllowUnknownCache   bool
	AssumedContextLimit int
	TermLLMVersion      string
	OnRecord            func(RunRecord) error
}

type Budget struct {
	MaximumRequests         int      `json:"maximum_requests"`
	MaximumRequestInput     int      `json:"maximum_request_input_tokens"`
	MaximumRequestOutput    int      `json:"maximum_request_output_tokens"`
	MaximumTotalInput       int64    `json:"maximum_total_input_tokens"`
	MaximumTotalOutput      int64    `json:"maximum_total_output_tokens"`
	IncludesCalibration     bool     `json:"includes_calibration"`
	IncludesWarmups         bool     `json:"includes_warmups"`
	IncludesRetryAllowance  bool     `json:"includes_cache_contamination_retry_allowance"`
	CacheWriteBillingRisk   bool     `json:"cache_write_billing_risk"`
	OutputBudgetIsRequested bool     `json:"output_budget_is_requested_not_guaranteed"`
	Notes                   []string `json:"notes"`
}

type BenchmarkMetadata struct {
	Mode                string  `json:"mode"`
	Cache               string  `json:"cache"`
	Seed                int64   `json:"seed"`
	Runs                int     `json:"runs"`
	Warmups             int     `json:"warmups"`
	Concurrency         int     `json:"concurrency"`
	TimeoutSeconds      float64 `json:"timeout_seconds"`
	CacheTolerance      float64 `json:"cache_tolerance"`
	TargetTolerance     float64 `json:"target_tolerance"`
	AllowUnknownCache   bool    `json:"allow_unknown_cache"`
	AssumedContextLimit int     `json:"assumed_context_limit,omitempty"`
	RetryPolicy         string  `json:"retry_policy"`
	TimingBoundary      string  `json:"timing_boundary"`
}

type TargetMetadata struct {
	ProviderKey         string              `json:"provider"`
	ProviderType        string              `json:"provider_type"`
	ProviderName        string              `json:"provider_name"`
	CredentialType      string              `json:"credential_type"`
	RequestedModel      string              `json:"requested_model"`
	InputTargetMeaning  string              `json:"input_target_meaning"`
	InputLimit          int                 `json:"input_limit,omitempty"`
	ConfiguredNumCtx    int                 `json:"configured_num_ctx,omitempty"`
	AssumedContextLimit int                 `json:"assumed_context_limit,omitempty"`
	ServiceTier         string              `json:"service_tier,omitempty"`
	Capabilities        AdapterCapabilities `json:"capabilities"`
	ReasoningEffort     string              `json:"reasoning_effort,omitempty"`
	ReasoningExpected   bool                `json:"reasoning_expected"`
	Temperature         *float32            `json:"temperature"`
	OutputLimitMeaning  string              `json:"output_limit_meaning"`
	Error               string              `json:"error,omitempty"`
}

type UsageRecord struct {
	DirectInputTokens      int    `json:"direct_input_tokens"`
	CachedInputTokens      *int   `json:"cached_input_tokens"`
	CacheWriteTokens       *int   `json:"cache_write_tokens"`
	ProviderRawInputTokens *int   `json:"provider_raw_input_tokens"`
	TotalInputTokens       int    `json:"total_input_tokens"`
	ComputedInputTokens    int    `json:"computed_input_tokens"`
	OutputTokens           int    `json:"output_tokens"`
	ReasoningTokens        *int   `json:"reasoning_tokens"`
	TokenCountSource       string `json:"token_count_source"`
}

type TimingRecord struct {
	ActivityTTFTMS      *float64 `json:"activity_ttft_ms"`
	VisibleTTFTMS       *float64 `json:"visible_ttft_ms"`
	EndToEndMS          *float64 `json:"end_to_end_ms"`
	ObservedDecodeMS    *float64 `json:"observed_decode_ms"`
	VisibleDecodeMS     *float64 `json:"visible_decode_ms"`
	DecodeTokensPerSec  *float64 `json:"decode_tokens_per_second"`
	VisibleTokensPerSec *float64 `json:"visible_decode_tokens_per_second"`
	TPOTMS              *float64 `json:"tpot_ms"`
}

type ModelSwitch struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type RunRecord struct {
	Provider                  string        `json:"provider"`
	ProviderType              string        `json:"provider_type"`
	RequestedModel            string        `json:"requested_model"`
	ObservedModels            []ModelSwitch `json:"observed_model_switches,omitempty"`
	Phase                     string        `json:"phase"`
	Workload                  string        `json:"workload"`
	Run                       int           `json:"run"`
	Attempt                   int           `json:"attempt"`
	Order                     int           `json:"order"`
	StartedAt                 time.Time     `json:"started_at"`
	Seed                      int64         `json:"seed"`
	PayloadSHA256             string        `json:"payload_sha256"`
	PayloadBytes              int           `json:"payload_bytes"`
	PayloadWords              int           `json:"payload_words"`
	RequestedInput            int           `json:"requested_input_tokens"`
	InputTargetMeaning        string        `json:"input_target_meaning"`
	LocalEstimate             int           `json:"local_estimated_input_tokens"`
	RequestedOutput           int           `json:"requested_output_tokens"`
	OutputLimitStatus         string        `json:"output_limit_status"`
	ReasoningEffort           string        `json:"reasoning_effort,omitempty"`
	ReasoningExpected         bool          `json:"reasoning_expected"`
	ServiceTier               string        `json:"service_tier,omitempty"`
	Temperature               *float32      `json:"temperature"`
	ReportsCacheReads         bool          `json:"reports_cache_reads"`
	MeasurementScope          string        `json:"measurement_scope"`
	RetryPolicy               string        `json:"retry_policy"`
	UsageReceived             bool          `json:"usage_received"`
	Usage                     UsageRecord   `json:"usage"`
	CacheState                string        `json:"cache_state"`
	CacheRatio                *float64      `json:"cache_ratio"`
	TargetMatched             *bool         `json:"target_matched"`
	ActivityEvents            int           `json:"activity_events"`
	VisibleTextEvents         int           `json:"visible_text_events"`
	ReasoningObserved         bool          `json:"reasoning_observed"`
	ObservableReasoningWindow bool          `json:"observable_reasoning_window"`
	DecodeStatus              string        `json:"decode_status"`
	Timing                    TimingRecord  `json:"timing"`
	RetryEvents               int           `json:"retry_events"`
	RetryWaitSeconds          float64       `json:"retry_wait_seconds"`
	AttemptDiscards           int           `json:"attempt_discards"`
	RetryReason               string        `json:"retry_reason,omitempty"`
	OutputLimitReached        bool          `json:"output_limit_reached"`
	TerminalReceived          bool          `json:"terminal_received"`
	Success                   bool          `json:"success"`
	TimedOut                  bool          `json:"timed_out"`
	Cancelled                 bool          `json:"cancelled"`
	Error                     string        `json:"error,omitempty"`
	LatencyEligible           bool          `json:"latency_eligible"`
	FitEligible               bool          `json:"fit_eligible"`
	ExclusionReasons          []string      `json:"exclusion_reasons,omitempty"`
}

type Distribution struct {
	Count  int      `json:"count"`
	Label  string   `json:"label"`
	Min    *float64 `json:"min"`
	Median *float64 `json:"median"`
	Max    *float64 `json:"max"`
	P95    *float64 `json:"p95"`
}

type Aggregate struct {
	Provider                 string       `json:"provider"`
	RequestedModel           string       `json:"requested_model"`
	Workload                 string       `json:"workload"`
	RequestedInput           int          `json:"requested_input_tokens"`
	RequestedOutput          int          `json:"requested_output_tokens"`
	MeasuredRuns             int          `json:"measured_runs"`
	SuccessfulRuns           int          `json:"successful_runs"`
	LatencyValidRuns         int          `json:"latency_valid_runs"`
	FitValidRuns             int          `json:"fit_valid_runs"`
	DecodeValidRuns          int          `json:"decode_valid_runs"`
	ErrorRuns                int          `json:"error_runs"`
	CacheState               string       `json:"cache_state"`
	ComputedInputTokens      *float64     `json:"median_computed_input_tokens"`
	TotalInputTokens         *float64     `json:"median_provider_total_input_tokens"`
	CachedInputTokens        *float64     `json:"median_cached_input_tokens"`
	CacheWriteTokens         *float64     `json:"median_cache_write_tokens"`
	OutputTokens             *float64     `json:"median_output_tokens"`
	ActivityTTFTMS           Distribution `json:"activity_ttft_ms"`
	VisibleTTFTMS            Distribution `json:"visible_ttft_ms"`
	EndToEndMS               Distribution `json:"end_to_end_ms"`
	DecodeTokensPerSecond    Distribution `json:"decode_tokens_per_second"`
	VisibleTokensPerSecond   Distribution `json:"visible_decode_tokens_per_second"`
	TPOTMS                   Distribution `json:"tpot_ms"`
	EffectiveInputTokensRate Distribution `json:"client_observed_effective_input_tokens_per_second"`
}

type Fit struct {
	Provider                string  `json:"provider"`
	RequestedModel          string  `json:"requested_model"`
	Range                   string  `json:"range"`
	MinimumInputTokens      int     `json:"minimum_input_tokens"`
	MaximumInputTokens      int     `json:"maximum_input_tokens"`
	DistinctLengths         int     `json:"distinct_lengths"`
	ValidRuns               int     `json:"valid_runs"`
	SlopeSecondsPerToken    float64 `json:"slope_seconds_per_token"`
	EffectiveTokensPerSec   float64 `json:"client_observed_effective_prefill_tokens_per_second"`
	InterceptSeconds        float64 `json:"intercept_seconds"`
	MedianAbsoluteErrorSecs float64 `json:"median_absolute_error_seconds"`
	Method                  string  `json:"method"`
}

type Report struct {
	SchemaVersion  int               `json:"schema_version"`
	Benchmark      BenchmarkMetadata `json:"benchmark"`
	Budget         Budget            `json:"budget"`
	TermLLMVersion string            `json:"term_llm_version"`
	Targets        []TargetMetadata  `json:"targets"`
	Runs           []RunRecord       `json:"runs"`
	Aggregates     []Aggregate       `json:"aggregates"`
	Fits           []Fit             `json:"fits"`
	Limitations    []string          `json:"limitations"`
}

type Runner struct {
	Clock Clock
}

func (r Runner) clock() Clock {
	if r.Clock == nil {
		return realClock{}
	}
	return r.Clock
}

func (r Runner) RunTarget(ctx context.Context, target Target, opts Options) ([]RunRecord, error) {
	return r.runTarget(ctx, target, opts)
}
