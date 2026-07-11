package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	mapstructure "github.com/go-viper/mapstructure/v2"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// ProviderType defines the supported provider implementations
type ProviderType string

const (
	ProviderTypeAnthropic    ProviderType = "anthropic"
	ProviderTypeOpenAI       ProviderType = "openai"
	ProviderTypeChatGPT      ProviderType = "chatgpt"
	ProviderTypeCopilot      ProviderType = "copilot"
	ProviderTypeGemini       ProviderType = "gemini"
	ProviderTypeGeminiCLI    ProviderType = "gemini-cli"
	ProviderTypeOpenRouter   ProviderType = "openrouter"
	ProviderTypeZen          ProviderType = "zen"
	ProviderTypeClaudeBin    ProviderType = "claude-bin"
	ProviderTypeGrokBin      ProviderType = "grok-bin"
	ProviderTypeOpenAICompat ProviderType = "openai_compatible"
	ProviderTypeVLLM         ProviderType = "vllm"
	ProviderTypeXAI          ProviderType = "xai"
	ProviderTypeVenice       ProviderType = "venice"
	ProviderTypeNearAI       ProviderType = "nearai"
	ProviderTypeSambaNova    ProviderType = "sambanova"
	ProviderTypeBedrock      ProviderType = "bedrock"
	ProviderTypeOllama       ProviderType = "ollama"
)

// builtInProviderTypes maps known provider names to their types
var builtInProviderTypes = map[string]ProviderType{
	"anthropic":  ProviderTypeAnthropic,
	"openai":     ProviderTypeOpenAI,
	"chatgpt":    ProviderTypeChatGPT,
	"copilot":    ProviderTypeCopilot,
	"gemini":     ProviderTypeGemini,
	"gemini-cli": ProviderTypeGeminiCLI,
	"openrouter": ProviderTypeOpenRouter,
	"zen":        ProviderTypeZen,
	"claude-bin": ProviderTypeClaudeBin,
	"grok-bin":   ProviderTypeGrokBin,
	"vllm":       ProviderTypeVLLM,
	"xai":        ProviderTypeXAI,
	"venice":     ProviderTypeVenice,
	"nearai":     ProviderTypeNearAI,
	"sambanova":  ProviderTypeSambaNova,
	"bedrock":    ProviderTypeBedrock,
	"ollama":     ProviderTypeOllama,
}

// InferProviderType returns the provider type for a given provider name
// Explicit type takes precedence, then built-in names, then defaults to openai_compatible
func InferProviderType(name string, explicit ProviderType) ProviderType {
	if explicit != "" {
		return explicit
	}
	if t, ok := builtInProviderTypes[name]; ok {
		return t
	}
	return ProviderTypeOpenAICompat
}

// FileUploadConfig controls how user-uploaded files are forwarded to a provider.
// Native MIME types may be sent as provider-native file/document parts when the
// provider implementation supports them. Text-embed MIME types may be converted
// to ordinary prompt text as a portable fallback.
type FileUploadConfig struct {
	NativeMimeTypes    []string `mapstructure:"native_mime_types" yaml:"native_mime_types,omitempty"`
	MaxNativeBytes     int64    `mapstructure:"max_native_bytes" yaml:"max_native_bytes,omitempty"`
	TextEmbedMimeTypes []string `mapstructure:"text_embed_mime_types" yaml:"text_embed_mime_types,omitempty"`
	MaxTextEmbedBytes  int64    `mapstructure:"max_text_embed_bytes" yaml:"max_text_embed_bytes,omitempty"`
}

// ResponsesConfig controls advanced OpenAI Responses API execution features.
// It is separate from ReasoningConfig, which only governs display/export.
type ResponsesConfig struct {
	ReasoningMode           string                           `mapstructure:"reasoning_mode" yaml:"reasoning_mode,omitempty"`
	ReasoningContext        string                           `mapstructure:"reasoning_context" yaml:"reasoning_context,omitempty"`
	MultiAgent              ResponsesMultiAgentConfig        `mapstructure:"multi_agent" yaml:"multi_agent,omitempty"`
	ProgrammaticToolCalling ResponsesProgrammaticToolsConfig `mapstructure:"programmatic_tool_calling" yaml:"programmatic_tool_calling,omitempty"`
	PromptCache             ResponsesPromptCacheConfig       `mapstructure:"prompt_cache" yaml:"prompt_cache,omitempty"`
}

type ResponsesMultiAgentConfig struct {
	Enabled                bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
	MaxConcurrentSubagents int  `mapstructure:"max_concurrent_subagents" yaml:"max_concurrent_subagents,omitempty"`
}

type ResponsesProgrammaticToolsConfig struct {
	Enabled bool     `mapstructure:"enabled" yaml:"enabled,omitempty"`
	Tools   []string `mapstructure:"tools" yaml:"tools,omitempty"`
}

type ResponsesPromptCacheConfig struct {
	Mode string `mapstructure:"mode" yaml:"mode,omitempty"`
	TTL  string `mapstructure:"ttl" yaml:"ttl,omitempty"`
}

// ProviderModelConfig describes a configured model entry. A model can be a
// simple string in YAML (stored in ProviderConfig.Models) or an object with
// metadata here. Alias is the friendly name exposed in pickers/completions;
// ID is the upstream model string sent to the provider.
type ProviderModelConfig struct {
	ID               string   `mapstructure:"id" yaml:"id,omitempty"`
	Alias            string   `mapstructure:"alias" yaml:"alias,omitempty"`
	ContextWindow    int      `mapstructure:"context_window" yaml:"context_window,omitempty"`
	MaxOutputTokens  int      `mapstructure:"max_output_tokens" yaml:"max_output_tokens,omitempty"`
	ParseReasoning   *bool    `mapstructure:"parse_reasoning" yaml:"parse_reasoning,omitempty"`
	IncludeReasoning *bool    `mapstructure:"include_reasoning" yaml:"include_reasoning,omitempty"`
	ThinkingParam    string   `mapstructure:"thinking_param" yaml:"thinking_param,omitempty"`
	ReasoningEfforts []string `mapstructure:"reasoning_efforts" yaml:"reasoning_efforts,omitempty"`
	VisionVia        string   `mapstructure:"vision_via" yaml:"vision_via,omitempty"`
}

func (m ProviderModelConfig) DisplayName() string {
	if alias := strings.TrimSpace(m.Alias); alias != "" {
		return alias
	}
	return strings.TrimSpace(m.ID)
}

// ProviderConfig is a unified configuration for any provider
type ProviderConfig struct {
	// Type of provider - inferred from key name for built-ins, required for custom
	Type ProviderType `mapstructure:"type"`

	// Common fields
	APIKey       string                `mapstructure:"api_key"`
	Model        string                `mapstructure:"model"`
	FastModel    string                `mapstructure:"fast_model"`    // Lightweight model for control-plane tasks
	FastProvider string                `mapstructure:"fast_provider"` // Optional provider key override for FastModel
	ServiceTier  string                `mapstructure:"service_tier"`  // Optional model service tier (e.g. "fast"/"priority" for ChatGPT)
	Models       []string              `mapstructure:"models"`        // Available model names/aliases for autocomplete
	ModelConfigs []ProviderModelConfig `mapstructure:"-"`             // Metadata for object entries in models
	Reasoning    string                `mapstructure:"reasoning"`     // "auto"/empty, "enabled", or "disabled" for suffix-based reasoning efforts
	Credentials  string                `mapstructure:"credentials"`   // "api_key", "codex", "gemini-cli"
	Env          map[string]string     `mapstructure:"env"`           // Extra subprocess env vars for providers that shell out (e.g. claude-bin)
	EnableHooks  bool                  `mapstructure:"enable_hooks"`  // Opt in to Claude Code hooks for claude-bin (disabled by default)
	UseWebSocket bool                  `mapstructure:"use_websocket"` // Enable Responses-over-WebSocket for providers that support it
	Responses    ResponsesConfig       `mapstructure:"responses"`     // Advanced Responses API execution controls
	FileUpload   *FileUploadConfig     `mapstructure:"file_upload"`   // Optional upload/native-file support overrides
	VisionVia    string                `mapstructure:"vision_via"`    // Optional provider:model route for indirect image understanding

	// Search behavior - nil means auto (use native if available)
	UseNativeSearch *bool `mapstructure:"use_native_search"`

	// Model token limits (for custom/self-hosted models not in hardcoded tables)
	ContextWindow   int `mapstructure:"context_window"`
	MaxOutputTokens int `mapstructure:"max_output_tokens"`

	// OpenAI-compatible specific
	BaseURL           string `mapstructure:"base_url"`            // Base URL - /chat/completions is appended
	URL               string `mapstructure:"url"`                 // Full URL - used as-is without appending endpoint
	NoStreamOptions   bool   `mapstructure:"no_stream_options"`   // Don't send stream_options (for servers that reject it)
	VLLMThinkingParam string `mapstructure:"vllm_thinking_param"` // vLLM chat_template_kwargs key: "enable_thinking" (Qwen) or "thinking" (DeepSeek)
	ParseReasoning    *bool  `mapstructure:"parse_reasoning"`     // Send parse_reasoning for OpenAI-compatible reasoning parsers
	IncludeReasoning  *bool  `mapstructure:"include_reasoning"`   // Send include_reasoning when parse_reasoning is enabled
	ThinkingParam     string `mapstructure:"thinking_param"`      // chat_template_kwargs key to set true when reasoning effort is requested

	// OpenRouter specific
	AppURL   string `mapstructure:"app_url"`
	AppTitle string `mapstructure:"app_title"`

	// AWS Bedrock specific
	Region       string            `mapstructure:"region"`            // AWS region (defaults to AWS_REGION env var)
	Profile      string            `mapstructure:"profile"`           // AWS profile from ~/.aws/credentials
	AccessKey    string            `mapstructure:"access_key_id"`     // Explicit AWS access key ID
	SecretKey    string            `mapstructure:"secret_access_key"` // Explicit AWS secret access key
	SessionToken string            `mapstructure:"session_token"`     // Optional AWS session token (temporary creds)
	ModelMap     map[string]string `mapstructure:"model_map"`         // Friendly name -> Bedrock model ID/ARN

	// Ollama-native sampling options (type: ollama only)
	Think           *bool    `mapstructure:"think"`            // Enable extended thinking / reasoning
	TopK            *int     `mapstructure:"top_k"`            // Top-K sampling
	MinP            *float64 `mapstructure:"min_p"`            // Min-P sampling
	PresencePenalty *float64 `mapstructure:"presence_penalty"` // Presence penalty
	NumCtx          *int     `mapstructure:"num_ctx"`          // Context window size in tokens
	NumPredict      *int     `mapstructure:"num_predict"`      // Max tokens to generate (-1 = unlimited)

	// Runtime fields (populated after credential resolution)
	ResolvedAPIKey string                              `mapstructure:"-"`
	AccountID      string                              `mapstructure:"-"`
	OAuthCreds     *credentials.GeminiOAuthCredentials `mapstructure:"-"`
	ResolvedURL    string                              `mapstructure:"-"` // Resolved URL (after srv:// lookup)

	// Resolution tracking - provider credential discovery is deferred until needed,
	// and expensive values are resolved lazily before inference.
	credentialsResolved bool `mapstructure:"-"`
	needsLazyResolution bool `mapstructure:"-"`
}

// Reasoning display policy values.
const (
	ReasoningDisplayAuto      = "auto"
	ReasoningDisplayOff       = "off"
	ReasoningDisplayStatus    = "status"
	ReasoningDisplayCollapsed = "collapsed"
	ReasoningDisplayExpanded  = "expanded"
	ReasoningDisplayRaw       = "raw"

	ReasoningSourceSummaryOnly           = "summary_only"
	ReasoningSourceSummaryOrProviderSafe = "summary_or_provider_safe"
	ReasoningSourceAll                   = "all"

	ReasoningStatusNone    = "none"
	ReasoningStatusGeneric = "generic"
	ReasoningStatusTitle   = "title"
	ReasoningStatusSummary = "summary"

	ReasoningHistoryNone           = "none"
	ReasoningHistoryCollapsed      = "collapsed"
	ReasoningHistoryExpanded       = "expanded"
	ReasoningHistoryTranscriptOnly = "transcript_only"

	ReasoningExportNever     = "never"
	ReasoningExportAsk       = "ask"
	ReasoningExportSummaries = "summaries"
	ReasoningExportRaw       = "raw"

	ReasoningInherit = "inherit"
)

// ReasoningSurfaceConfig holds optional per-surface overrides for reasoning UI.
type ReasoningSurfaceConfig struct {
	Display string `mapstructure:"display" yaml:"display,omitempty"`
	Status  string `mapstructure:"status" yaml:"status,omitempty"`
	History string `mapstructure:"history" yaml:"history,omitempty"`
	Export  string `mapstructure:"export" yaml:"export,omitempty"`
	Raw     *bool  `mapstructure:"raw" yaml:"raw,omitempty"`
}

// ReasoningConfig controls display/export of provider reasoning summaries and
// raw thinking. It is intentionally separate from provider reasoning effort.
type ReasoningConfig struct {
	Display          string                 `mapstructure:"display" yaml:"display,omitempty"`
	Source           string                 `mapstructure:"source" yaml:"source,omitempty"`
	Status           string                 `mapstructure:"status" yaml:"status,omitempty"`
	History          string                 `mapstructure:"history" yaml:"history,omitempty"`
	Export           string                 `mapstructure:"export" yaml:"export,omitempty"`
	Raw              bool                   `mapstructure:"raw" yaml:"raw,omitempty"`
	MaxSummaryChars  int                    `mapstructure:"max_summary_chars" yaml:"max_summary_chars,omitempty"`
	MaxRawChars      int                    `mapstructure:"max_raw_chars" yaml:"max_raw_chars,omitempty"`
	ExtractTitles    bool                   `mapstructure:"extract_titles" yaml:"extract_titles,omitempty"`
	HiddenLabel      string                 `mapstructure:"hidden_label" yaml:"hidden_label,omitempty"`
	PersistSummaries bool                   `mapstructure:"persist_summaries" yaml:"persist_summaries,omitempty"`
	Chat             ReasoningSurfaceConfig `mapstructure:"chat" yaml:"chat,omitempty"`
	Ask              ReasoningSurfaceConfig `mapstructure:"ask" yaml:"ask,omitempty"`
	Serve            ReasoningSurfaceConfig `mapstructure:"serve" yaml:"serve,omitempty"`
	Jobs             ReasoningSurfaceConfig `mapstructure:"jobs" yaml:"jobs,omitempty"`

	// Presence markers let programmatic configs explicitly set false for boolean
	// options while still allowing partial configs to inherit defaults. Viper
	// populates these from InConfig; direct callers can set them when they need a
	// false override.
	RawSet              bool `mapstructure:"-" yaml:"-"`
	ExtractTitlesSet    bool `mapstructure:"-" yaml:"-"`
	PersistSummariesSet bool `mapstructure:"-" yaml:"-"`
}

// DefaultReasoningConfig returns the safe, useful default reasoning display policy.
func DefaultReasoningConfig() ReasoningConfig {
	return ReasoningConfig{
		Display:          defaultString("reasoning.display"),
		Source:           defaultString("reasoning.source"),
		Status:           defaultString("reasoning.status"),
		History:          defaultString("reasoning.history"),
		Export:           defaultString("reasoning.export"),
		Raw:              defaultBool("reasoning.raw"),
		MaxSummaryChars:  defaultInt("reasoning.max_summary_chars"),
		MaxRawChars:      defaultInt("reasoning.max_raw_chars"),
		ExtractTitles:    defaultBool("reasoning.extract_titles"),
		HiddenLabel:      defaultString("reasoning.hidden_label"),
		PersistSummaries: defaultBool("reasoning.persist_summaries"),
	}
}

// ResolveReasoning returns a fully-populated reasoning display policy for a
// surface ("chat", "ask", "serve", or "jobs"), applying optional per-surface
// overrides and the TERM_LLM_SHOW_RAW_REASONING debug override.
func (c *Config) ResolveReasoning(surface string) ReasoningConfig {
	resolved := DefaultReasoningConfig()
	if c != nil {
		resolved = mergeReasoningConfig(resolved, c.Reasoning)
		switch strings.ToLower(strings.TrimSpace(surface)) {
		case "chat":
			resolved = applyReasoningSurface(resolved, c.Reasoning.Chat)
		case "ask":
			resolved = applyReasoningSurface(resolved, c.Reasoning.Ask)
		case "serve":
			resolved = applyReasoningSurface(resolved, c.Reasoning.Serve)
		case "jobs":
			resolved = applyReasoningSurface(resolved, c.Reasoning.Jobs)
		}
	}
	resolved.normalize()
	if parseEnvBool(os.Getenv("TERM_LLM_SHOW_RAW_REASONING")) {
		resolved.Display = ReasoningDisplayRaw
		resolved.Source = ReasoningSourceAll
		resolved.Raw = true
	}
	return resolved
}

func mergeReasoningConfig(base, override ReasoningConfig) ReasoningConfig {
	if isZeroReasoningConfig(override) {
		return base
	}
	if override.Display != "" {
		base.Display = override.Display
	}
	if override.Source != "" {
		base.Source = override.Source
	}
	if override.Status != "" {
		base.Status = override.Status
	}
	if override.History != "" {
		base.History = override.History
	}
	if override.Export != "" {
		base.Export = override.Export
	}
	if override.RawSet || override.Raw {
		base.Raw = override.Raw
	}
	if override.MaxSummaryChars != 0 {
		base.MaxSummaryChars = override.MaxSummaryChars
	}
	if override.MaxRawChars != 0 {
		base.MaxRawChars = override.MaxRawChars
	}
	if override.ExtractTitlesSet || override.ExtractTitles {
		base.ExtractTitles = override.ExtractTitles
	}
	if override.PersistSummariesSet || override.PersistSummaries {
		base.PersistSummaries = override.PersistSummaries
	}
	if override.HiddenLabel != "" {
		base.HiddenLabel = override.HiddenLabel
	}
	base.Chat = override.Chat
	base.Ask = override.Ask
	base.Serve = override.Serve
	base.Jobs = override.Jobs
	return base
}

func isZeroReasoningConfig(r ReasoningConfig) bool {
	return r.Display == "" &&
		r.Source == "" &&
		r.Status == "" &&
		r.History == "" &&
		r.Export == "" &&
		!r.Raw &&
		r.MaxSummaryChars == 0 &&
		r.MaxRawChars == 0 &&
		!r.ExtractTitles &&
		r.HiddenLabel == "" &&
		!r.PersistSummaries &&
		!r.RawSet &&
		!r.ExtractTitlesSet &&
		!r.PersistSummariesSet &&
		r.Chat == (ReasoningSurfaceConfig{}) &&
		r.Ask == (ReasoningSurfaceConfig{}) &&
		r.Serve == (ReasoningSurfaceConfig{}) &&
		r.Jobs == (ReasoningSurfaceConfig{})
}

func applyReasoningSurface(base ReasoningConfig, surface ReasoningSurfaceConfig) ReasoningConfig {
	if v := strings.TrimSpace(surface.Display); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.Display = v
	}
	if v := strings.TrimSpace(surface.Status); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.Status = v
	}
	if v := strings.TrimSpace(surface.History); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.History = v
	}
	if v := strings.TrimSpace(surface.Export); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.Export = v
	}
	if surface.Raw != nil {
		base.Raw = *surface.Raw
	}
	return base
}

func (r *ReasoningConfig) normalize() {
	if r == nil {
		return
	}
	defaults := DefaultReasoningConfig()
	r.Display = normalizeOneOf(r.Display, defaults.Display, ReasoningDisplayAuto, ReasoningDisplayOff, ReasoningDisplayStatus, ReasoningDisplayCollapsed, ReasoningDisplayExpanded, ReasoningDisplayRaw)
	r.Source = normalizeOneOf(r.Source, defaults.Source, ReasoningSourceSummaryOnly, ReasoningSourceSummaryOrProviderSafe, ReasoningSourceAll)
	r.Status = normalizeOneOf(r.Status, defaults.Status, ReasoningStatusNone, ReasoningStatusGeneric, ReasoningStatusTitle, ReasoningStatusSummary)
	r.History = normalizeOneOf(r.History, defaults.History, ReasoningHistoryNone, ReasoningHistoryCollapsed, ReasoningHistoryExpanded, ReasoningHistoryTranscriptOnly)
	r.Export = normalizeOneOf(r.Export, defaults.Export, ReasoningExportNever, ReasoningExportAsk, ReasoningExportSummaries, ReasoningExportRaw)
	if r.MaxSummaryChars <= 0 {
		r.MaxSummaryChars = defaults.MaxSummaryChars
	}
	if r.MaxRawChars <= 0 {
		r.MaxRawChars = defaults.MaxRawChars
	}
	if strings.TrimSpace(r.HiddenLabel) == "" {
		r.HiddenLabel = defaults.HiddenLabel
	}
}

func normalizeOneOf(value, fallback string, allowed ...string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || v == ReasoningInherit {
		return fallback
	}
	for _, allow := range allowed {
		if v == allow {
			return v
		}
	}
	return fallback
}

func parseEnvBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

type Config struct {
	DefaultProvider string                    `mapstructure:"default_provider"`
	Providers       map[string]ProviderConfig `mapstructure:"providers"`
	Diagnostics     DiagnosticsConfig         `mapstructure:"diagnostics"`
	DebugLogs       DebugLogsConfig           `mapstructure:"debug_logs"`
	Sessions        SessionsConfig            `mapstructure:"sessions"`
	Approval        ApprovalConfig            `mapstructure:"approval"`
	Guardian        GuardianConfig            `mapstructure:"guardian"`
	Exec            ExecConfig                `mapstructure:"exec"`
	Ask             AskConfig                 `mapstructure:"ask"`
	Chat            ChatConfig                `mapstructure:"chat"`
	Edit            EditConfig                `mapstructure:"edit"`
	Image           ImageConfig               `mapstructure:"image"`
	Audio           AudioConfig               `mapstructure:"audio"`
	Music           MusicConfig               `mapstructure:"music"`
	Transcription   TranscriptionConfig       `mapstructure:"transcription"`
	Embed           EmbedConfig               `mapstructure:"embed"`
	Search          SearchConfig              `mapstructure:"search"`
	Reasoning       ReasoningConfig           `mapstructure:"reasoning"`
	Theme           ThemeConfig               `mapstructure:"theme"`
	Tools           ToolsConfig               `mapstructure:"tools"`
	Agents          AgentsConfig              `mapstructure:"agents"`
	Skills          SkillsConfig              `mapstructure:"skills"`
	AgentsMd        AgentsMdConfig            `mapstructure:"agents_md"`
	AutoCompact     bool                      `mapstructure:"auto_compact"`
	Serve           ServeConfig               `mapstructure:"serve"`
	FileTracking    FileTrackingConfig        `mapstructure:"file_tracking"`
}

// ApprovalConfig configures default approval behavior.
type ApprovalConfig struct {
	// DefaultMode controls the approval mode for new chat sessions when no CLI flag
	// or resumed session value overrides it. Valid values: prompt, auto.
	// yolo is intentionally not accepted as a config default.
	DefaultMode string `mapstructure:"default_mode" yaml:"default_mode,omitempty"`
}

// GuardianConfig configures auto approval policy review.
type GuardianConfig struct {
	Provider       string `mapstructure:"provider" yaml:"provider,omitempty"`
	Model          string `mapstructure:"model" yaml:"model,omitempty"`
	PolicyPath     string `mapstructure:"policy_path" yaml:"policy_path,omitempty"`
	TimeoutSeconds int    `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
}

// ServeConfig holds configuration for the serve command platforms.
type ServeConfig struct {
	Platforms              []string            `mapstructure:"platforms" yaml:"platforms,omitempty"`
	BasePath               string              `mapstructure:"base_path" yaml:"base_path,omitempty"`
	Title                  string              `mapstructure:"title" yaml:"title,omitempty"`
	DisableLocationSharing bool                `mapstructure:"disable_location_sharing" yaml:"disable_location_sharing,omitempty"`
	FilesDir               string              `mapstructure:"files_dir" yaml:"files_dir,omitempty"`
	WidgetsDir             string              `mapstructure:"widgets_dir" yaml:"widgets_dir,omitempty"`
	ResponseTimeout        string              `mapstructure:"response_timeout" yaml:"response_timeout,omitempty"` // Go duration string, e.g. "30m" or "1h"
	Telegram               TelegramServeConfig `mapstructure:"telegram" yaml:"telegram,omitempty"`
	WebPush                WebPushConfig       `mapstructure:"web_push" yaml:"web_push,omitempty"`
}

// WebPushConfig holds VAPID keys for Web Push notifications.
type WebPushConfig struct {
	VAPIDPublicKey  string `mapstructure:"vapid_public_key" yaml:"vapid_public_key,omitempty"`
	VAPIDPrivateKey string `mapstructure:"vapid_private_key" yaml:"vapid_private_key,omitempty"`
	Subject         string `mapstructure:"subject" yaml:"subject,omitempty"` // mailto: or URL
}

// TelegramServeConfig holds configuration for the Telegram bot platform.
type TelegramServeConfig struct {
	Token            string   `mapstructure:"token" yaml:"token,omitempty"`
	AllowedUserIDs   []int64  `mapstructure:"allowed_user_ids" yaml:"allowed_user_ids,omitempty"`
	AllowedUsernames []string `mapstructure:"allowed_usernames" yaml:"allowed_usernames,omitempty"`
	IdleTimeout      int      `mapstructure:"idle_timeout" yaml:"idle_timeout,omitempty"`           // minutes
	InterruptTimeout int      `mapstructure:"interrupt_timeout" yaml:"interrupt_timeout,omitempty"` // seconds, 0 = default (3)
}

// AgentsConfig configures the agent system
type AgentsConfig struct {
	UseBuiltin  bool                       `mapstructure:"use_builtin"`  // Enable built-in agents (default true)
	SearchPaths []string                   `mapstructure:"search_paths"` // Additional directories to search for agents
	Preferences map[string]AgentPreference `mapstructure:"preferences"`  // Per-agent preference overrides
}

// AgentPreference allows overriding agent settings via config.yaml.
// All fields are optional - only set fields override the agent's defaults.
type AgentPreference struct {
	// Model preferences
	Provider string `mapstructure:"provider,omitempty" yaml:"provider,omitempty"`
	Model    string `mapstructure:"model,omitempty" yaml:"model,omitempty"`

	// Tool configuration
	ToolsEnabled  []string `mapstructure:"tools_enabled,omitempty" yaml:"tools_enabled,omitempty"`
	ToolsDisabled []string `mapstructure:"tools_disabled,omitempty" yaml:"tools_disabled,omitempty"`

	// Shell settings
	ShellAllow   []string `mapstructure:"shell_allow,omitempty" yaml:"shell_allow,omitempty"`
	ShellAutoRun *bool    `mapstructure:"shell_auto_run,omitempty" yaml:"shell_auto_run,omitempty"`

	// Spawn settings
	SpawnMaxParallel   *int     `mapstructure:"spawn_max_parallel,omitempty" yaml:"spawn_max_parallel,omitempty"`
	SpawnMaxDepth      *int     `mapstructure:"spawn_max_depth,omitempty" yaml:"spawn_max_depth,omitempty"`
	SpawnTimeout       *int     `mapstructure:"spawn_timeout,omitempty" yaml:"spawn_timeout,omitempty"`
	SpawnAllowedAgents []string `mapstructure:"spawn_allowed_agents,omitempty" yaml:"spawn_allowed_agents,omitempty"`

	// Behavior
	MaxTurns *int  `mapstructure:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	Search   *bool `mapstructure:"search,omitempty" yaml:"search,omitempty"`
}

// SkillsConfig configures the Agent Skills system
type SkillsConfig struct {
	Enabled              bool `mapstructure:"enabled"`                // Enable the skills system
	AutoInvoke           bool `mapstructure:"auto_invoke"`            // Allow model-driven activation
	MetadataBudgetTokens int  `mapstructure:"metadata_budget_tokens"` // Max tokens for skill metadata
	MaxVisibleSkills     int  `mapstructure:"max_visible_skills"`     // Max skills shown in system prompt

	IncludeProjectSkills  bool `mapstructure:"include_project_skills"`  // Discover from project-local paths
	IncludeEcosystemPaths bool `mapstructure:"include_ecosystem_paths"` // Include ~/.agents/skills, ~/.codex/skills, ~/.claude/skills, ~/.gemini/skills, etc.

	AlwaysEnabled []string `mapstructure:"always_enabled"` // Always include in metadata
	NeverAuto     []string `mapstructure:"never_auto"`     // Must be explicit activation
}

// AgentsMdConfig configures optional AGENTS.md loading
type AgentsMdConfig struct {
	Enabled bool `mapstructure:"enabled"` // Load AGENTS.md into system prompt
}

// ToolsConfig configures the local tool system
type ToolsConfig struct {
	Enabled            []string `mapstructure:"enabled"`               // Enabled tool names (CLI names)
	ReadDirs           []string `mapstructure:"read_dirs"`             // Directories for read operations
	WriteDirs          []string `mapstructure:"write_dirs"`            // Directories for write operations
	ShellAllow         []string `mapstructure:"shell_allow"`           // Shell command patterns
	ShellAutoRun       bool     `mapstructure:"shell_auto_run"`        // Auto-approve matching shell
	ShellAutoRunEnv    string   `mapstructure:"shell_auto_run_env"`    // Env var required for auto-run
	ShellNonTTYEnv     string   `mapstructure:"shell_non_tty_env"`     // Env var for non-TTY execution
	ImageProvider      string   `mapstructure:"image_provider"`        // Override for image provider
	MaxToolOutputChars int      `mapstructure:"max_tool_output_chars"` // Global max chars per tool output (default 20000)
}

// DiagnosticsConfig configures diagnostic data collection
type DiagnosticsConfig struct {
	Enabled bool   `mapstructure:"enabled"` // Enable diagnostic data collection
	Dir     string `mapstructure:"dir"`     // Override default directory
}

// DebugLogsConfig configures debug logging of LLM requests and responses
type DebugLogsConfig struct {
	Enabled bool   `mapstructure:"enabled"` // Enable debug logging
	Dir     string `mapstructure:"dir"`     // Override default directory (defaults to ~/.local/share/term-llm/debug/)
}

// SessionsConfig configures session storage
type SessionsConfig struct {
	Enabled          bool   `mapstructure:"enabled"`            // Master switch - set to false to disable all session storage
	MaxAgeDays       int    `mapstructure:"max_age_days"`       // Auto-delete sessions older than N days (0=never)
	MaxCount         int    `mapstructure:"max_count"`          // Keep at most N sessions, delete oldest (0=unlimited)
	Path             string `mapstructure:"path"`               // Optional SQLite DB path override (supports :memory:)
	StripImageBase64 bool   `mapstructure:"strip_image_base64"` // Store path/metadata only for images with ImagePath (smaller DB, less portable)
}

// FileTrackingConfig configures recording of file changes made by agent tools
type FileTrackingConfig struct {
	Enabled         bool   `mapstructure:"enabled"`           // Opt-in: record before/after content of files agents modify
	MaxFileBytes    int    `mapstructure:"max_file_bytes"`    // Per-file content cap; larger files recorded metadata-only (default 2 MiB)
	MaxSessionBytes int    `mapstructure:"max_session_bytes"` // Retained-content budget per session (default 100 MiB)
	MaxTotalBytes   int64  `mapstructure:"max_total_bytes"`   // Whole-database size cap; oldest sessions' history pruned on startup (default 1 GiB)
	Path            string `mapstructure:"path"`              // Optional SQLite DB path override
}

// ThemeConfig allows customization of UI colors
// Colors can be ANSI color numbers (0-255) or hex codes (#RRGGBB)
type ThemeConfig struct {
	Primary          string `mapstructure:"primary"`           // main accent (commands, highlights)
	Secondary        string `mapstructure:"secondary"`         // secondary accent (headers, borders)
	Success          string `mapstructure:"success"`           // success states
	Error            string `mapstructure:"error"`             // error states
	Warning          string `mapstructure:"warning"`           // warnings
	Muted            string `mapstructure:"muted"`             // dimmed text
	Text             string `mapstructure:"text"`              // primary text
	Spinner          string `mapstructure:"spinner"`           // loading spinner
	ReasoningSummary string `mapstructure:"reasoning_summary"` // reasoning summary body
	ReasoningHeader  string `mapstructure:"reasoning_header"`  // reasoning header lines
	ReasoningRaw     string `mapstructure:"reasoning_raw"`     // raw reasoning body
}

type ExecConfig struct {
	Provider     string `mapstructure:"provider"`     // Override provider for exec
	Model        string `mapstructure:"model"`        // Override model for exec
	Suggestions  int    `mapstructure:"suggestions"`  // Number of command suggestions (default 3)
	Instructions string `mapstructure:"instructions"` // Custom context for suggestions
}

type AskConfig struct {
	Provider     string `mapstructure:"provider"`     // Override provider for ask only
	Model        string `mapstructure:"model"`        // Override model for ask only
	Instructions string `mapstructure:"instructions"` // Custom system prompt for ask
	MaxTurns     int    `mapstructure:"max_turns"`    // Max agentic turns (default 20)
}

type ChatConfig struct {
	Provider            string `mapstructure:"provider"`              // Override provider for chat only
	Model               string `mapstructure:"model"`                 // Override model for chat only
	Instructions        string `mapstructure:"instructions"`          // Custom system prompt for chat
	MaxTurns            int    `mapstructure:"max_turns"`             // Max agentic turns (default 200)
	TerminalTitle       string `mapstructure:"terminal_title"`        // smart, basic, or off (default smart)
	TerminalTitleFormat string `mapstructure:"terminal_title_format"` // Optional custom terminal title template
	TerminalProgress    bool   `mapstructure:"terminal_progress"`     // Enable terminal progress indicators (default false)
}

type EditConfig struct {
	Provider        string `mapstructure:"provider"`          // Override provider for edit
	Model           string `mapstructure:"model"`             // Override model for edit
	Instructions    string `mapstructure:"instructions"`      // Custom instructions for edits
	ShowLineNumbers bool   `mapstructure:"show_line_numbers"` // Show line numbers in diff
	ContextLines    int    `mapstructure:"context_lines"`     // Lines of context in diff
	Editor          string `mapstructure:"editor"`            // Override $EDITOR
	DiffFormat      string `mapstructure:"diff_format"`       // "auto", "udiff", or "replace" (default: auto)
}

// ImageConfig configures image generation settings
type ImageConfig struct {
	Provider   string                `mapstructure:"provider"`   // default image provider: gemini, openai, chatgpt, xai, venice, flux, openrouter, debug
	OutputDir  string                `mapstructure:"output_dir"` // default save directory
	Gemini     ImageGeminiConfig     `mapstructure:"gemini"`
	OpenAI     ImageOpenAIConfig     `mapstructure:"openai"`
	ChatGPT    ImageChatGPTConfig    `mapstructure:"chatgpt"`
	XAI        ImageXAIConfig        `mapstructure:"xai"`
	Venice     ImageVeniceConfig     `mapstructure:"venice"`
	Flux       ImageFluxConfig       `mapstructure:"flux"`
	OpenRouter ImageOpenRouterConfig `mapstructure:"openrouter"`
	Debug      ImageDebugConfig      `mapstructure:"debug"`
}

// ImageGeminiConfig configures Gemini image generation
type ImageGeminiConfig struct {
	APIKey    string `mapstructure:"api_key"`
	Model     string `mapstructure:"model"`
	ImageSize string `mapstructure:"image_size"` // Default image size: 1K, 2K, 4K
}

// ImageOpenAIConfig configures OpenAI image generation
type ImageOpenAIConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
}

// ImageChatGPTConfig configures ChatGPT image generation via the chatgpt.com
// backend's built-in image_generation tool (OAuth, no API key required).
type ImageChatGPTConfig struct {
	Model string `mapstructure:"model"` // e.g., gpt-5.4-mini (default) or gpt-5.4
}

// ImageXAIConfig configures xAI (Grok) image generation
type ImageXAIConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // grok-2-image or grok-2-image-1212
}

// ImageVeniceConfig configures Venice AI image generation
type ImageVeniceConfig struct {
	APIKey     string `mapstructure:"api_key"`
	Model      string `mapstructure:"model"`
	EditModel  string `mapstructure:"edit_model"`
	Resolution string `mapstructure:"resolution"`
}

// ImageFluxConfig configures Flux (Black Forest Labs) image generation
type ImageFluxConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // flux-2-pro for generation, flux-kontext-pro for editing
}

// ImageOpenRouterConfig configures OpenRouter image generation
type ImageOpenRouterConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // e.g., google/gemini-2.5-flash-image
}

// ImageDebugConfig configures the debug image provider (local random images)
type ImageDebugConfig struct {
	Delay float64 `mapstructure:"delay"` // delay in seconds before returning (e.g., 1.5)
}

// AudioConfig configures speech/audio generation settings.
type AudioConfig struct {
	Provider   string                `mapstructure:"provider"`   // default audio provider: venice, gemini, or elevenlabs
	OutputDir  string                `mapstructure:"output_dir"` // default save directory
	Venice     AudioVeniceConfig     `mapstructure:"venice"`
	Gemini     AudioGeminiConfig     `mapstructure:"gemini"`
	ElevenLabs AudioElevenLabsConfig `mapstructure:"elevenlabs"`
}

// AudioVeniceConfig configures Venice AI text-to-speech generation.
type AudioVeniceConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
	Voice  string `mapstructure:"voice"`
	Format string `mapstructure:"format"`
}

// AudioGeminiConfig configures Gemini text-to-speech generation.
type AudioGeminiConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
	Voice  string `mapstructure:"voice"`
	Format string `mapstructure:"format"`
}

// AudioElevenLabsConfig configures ElevenLabs text-to-speech generation.
type AudioElevenLabsConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
	Voice  string `mapstructure:"voice"`
	Format string `mapstructure:"format"`
}

// MusicConfig configures music and sound-effect generation settings.
type MusicConfig struct {
	Provider   string                `mapstructure:"provider"`   // default music provider: venice or elevenlabs
	OutputDir  string                `mapstructure:"output_dir"` // default save directory
	Venice     MusicVeniceConfig     `mapstructure:"venice"`
	ElevenLabs MusicElevenLabsConfig `mapstructure:"elevenlabs"`
}

// MusicVeniceConfig configures Venice music/audio generation.
type MusicVeniceConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
	Format string `mapstructure:"format"`
}

// MusicElevenLabsConfig configures ElevenLabs music generation.
type MusicElevenLabsConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
	Format string `mapstructure:"format"`
}

// TranscriptionConfig configures audio transcription settings.
type TranscriptionConfig struct {
	Provider   string                        `mapstructure:"provider"`   // named provider from providers map; default "openai"
	Model      string                        `mapstructure:"model"`      // optional model override
	SaveDir    string                        `mapstructure:"save_dir"`   // if set, persist each uploaded audio file here
	Timestamps bool                          `mapstructure:"timestamps"` // Request timestamp metadata where supported
	Venice     TranscriptionVeniceConfig     `mapstructure:"venice"`
	ElevenLabs TranscriptionElevenLabsConfig `mapstructure:"elevenlabs"`
}

// TranscriptionVeniceConfig configures Venice speech-to-text.
type TranscriptionVeniceConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
}

// TranscriptionElevenLabsConfig configures ElevenLabs speech-to-text.
type TranscriptionElevenLabsConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
}

// EmbedConfig configures text embedding generation
type EmbedConfig struct {
	Provider string            `mapstructure:"provider"` // default embedding provider: gemini, openai, jina, voyage, ollama
	OpenAI   EmbedOpenAIConfig `mapstructure:"openai"`
	Gemini   EmbedGeminiConfig `mapstructure:"gemini"`
	Jina     EmbedJinaConfig   `mapstructure:"jina"`
	Voyage   EmbedVoyageConfig `mapstructure:"voyage"`
	Ollama   EmbedOllamaConfig `mapstructure:"ollama"`
}

// EmbedOpenAIConfig configures OpenAI embedding generation
type EmbedOpenAIConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // text-embedding-3-small (default), text-embedding-3-large
}

// EmbedGeminiConfig configures Gemini embedding generation
type EmbedGeminiConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // gemini-embedding-001 (default)
}

// EmbedJinaConfig configures Jina AI embedding generation
type EmbedJinaConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // jina-embeddings-v3 (default), jina-embeddings-v4
}

// EmbedVoyageConfig configures Voyage AI embedding generation
type EmbedVoyageConfig struct {
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"` // voyage-3.5 (default), voyage-3-large, voyage-code-3
}

// EmbedOllamaConfig configures Ollama embedding generation
type EmbedOllamaConfig struct {
	BaseURL string `mapstructure:"base_url"` // default: http://127.0.0.1:11434
	Model   string `mapstructure:"model"`    // nomic-embed-text (default)
}

// SearchConfig configures web search providers
type SearchConfig struct {
	Provider      string                 `mapstructure:"provider"`       // exa_mcp (default), exa, perplexity, tavily, brave, google, duckduckgo
	FetchProvider string                 `mapstructure:"fetch_provider"` // jina (default), exa_mcp, none
	ForceExternal bool                   `mapstructure:"force_external"` // force external search for all providers
	Exa           SearchExaConfig        `mapstructure:"exa"`
	ExaMCP        SearchExaMCPConfig     `mapstructure:"exa_mcp"`
	Perplexity    SearchPerplexityConfig `mapstructure:"perplexity"`
	Tavily        SearchTavilyConfig     `mapstructure:"tavily"`
	Brave         SearchBraveConfig      `mapstructure:"brave"`
	Google        SearchGoogleConfig     `mapstructure:"google"`
}

// SearchExaConfig configures Exa search
type SearchExaConfig struct {
	APIKey string `mapstructure:"api_key"`
}

// SearchExaMCPConfig configures Exa MCP search/fetch
type SearchExaMCPConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`
}

// SearchPerplexityConfig configures Perplexity search
type SearchPerplexityConfig struct {
	APIKey string `mapstructure:"api_key"`
}

// SearchTavilyConfig configures Tavily search
type SearchTavilyConfig struct {
	APIKey string `mapstructure:"api_key"`
}

// SearchBraveConfig configures Brave search
type SearchBraveConfig struct {
	APIKey string `mapstructure:"api_key"`
}

// SearchGoogleConfig configures Google Custom Search
type SearchGoogleConfig struct {
	APIKey string `mapstructure:"api_key"`
	CX     string `mapstructure:"cx"` // Custom Search Engine ID
}

func Load() (*Config, error) {
	configPath, err := GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get config dir: %w", err)
	}

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(configPath)
	viper.AddConfigPath(".")

	viper.RegisterAlias("provider", "default_provider")

	// Set defaults from GetDefaults() - single source of truth
	for key, value := range GetDefaults() {
		viper.SetDefault(key, value)
	}

	// Read config file (optional - won't error if missing)
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config: %w", err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg, viper.DecodeHook(providerModelsDecodeHook())); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	applyProviderModelConfigs(&cfg, providerModelConfigsFromViper(viper.GetViper()))
	markReasoningConfigPresence(&cfg.Reasoning, viper.GetViper())

	if err := overlayProviderEnvFromRawConfig(&cfg); err != nil {
		return nil, fmt.Errorf("failed to load raw provider env config: %w", err)
	}

	// Initialize providers map if nil
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}

	if err := cfg.ResolveProviderCredentials(cfg.DefaultProvider); err != nil {
		return nil, fmt.Errorf("%s credentials: %w", cfg.DefaultProvider, err)
	}

	resolveImageCredentials(&cfg.Image)
	resolveAudioCredentials(&cfg.Audio)
	resolveMusicCredentials(&cfg.Music)
	resolveTranscriptionCredentials(&cfg.Transcription)
	resolveEmbedCredentials(&cfg.Embed)
	resolveSearchCredentials(&cfg.Search)

	return &cfg, nil
}

func providerModelsDecodeHook() mapstructure.DecodeHookFunc {
	stringSliceType := reflect.TypeOf([]string{})
	return func(from reflect.Type, to reflect.Type, data any) (any, error) {
		if to != stringSliceType || from.Kind() != reflect.Slice {
			return data, nil
		}

		value := reflect.ValueOf(data)
		if !value.IsValid() {
			return data, nil
		}
		hasObject := false
		for i := 0; i < value.Len(); i++ {
			if _, ok := mapFromAny(value.Index(i).Interface()); ok {
				hasObject = true
				break
			}
		}
		if !hasObject {
			return data, nil
		}

		models := make([]string, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			model, ok := modelDisplayStringFromAny(value.Index(i).Interface())
			if !ok {
				return data, fmt.Errorf("models entries must be strings or objects with id/alias")
			}
			if model != "" {
				models = append(models, model)
			}
		}
		return models, nil
	}
}

func modelDisplayStringFromAny(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v), true
	default:
		m, ok := mapFromAny(raw)
		if !ok {
			return "", false
		}
		alias := stringFromMap(m, "alias")
		if alias != "" {
			return alias, true
		}
		id := stringFromMap(m, "id")
		if id == "" {
			id = stringFromMap(m, "model")
		}
		return id, id != ""
	}
}

func providerModelConfigsFromViper(v *viper.Viper) map[string][]ProviderModelConfig {
	if v == nil {
		return nil
	}
	providers := v.GetStringMap("providers")
	if len(providers) == 0 {
		return nil
	}
	out := make(map[string][]ProviderModelConfig)
	for providerName, rawProvider := range providers {
		providerMap, ok := mapFromAny(rawProvider)
		if !ok {
			continue
		}
		modelsRaw, ok := lookupMapValue(providerMap, "models")
		if !ok {
			continue
		}
		configs := providerModelConfigsFromAny(modelsRaw)
		if len(configs) > 0 {
			out[providerName] = configs
		}
	}
	return out
}

func providerModelConfigsFromAny(raw any) []ProviderModelConfig {
	value := reflect.ValueOf(raw)
	if !value.IsValid() || value.Kind() != reflect.Slice {
		return nil
	}
	var configs []ProviderModelConfig
	for i := 0; i < value.Len(); i++ {
		m, ok := mapFromAny(value.Index(i).Interface())
		if !ok {
			continue
		}
		cfg := providerModelConfigFromMap(m)
		if cfg.ID == "" && cfg.Alias == "" {
			continue
		}
		configs = append(configs, cfg)
	}
	return configs
}

func providerModelConfigFromMap(m map[string]any) ProviderModelConfig {
	return ProviderModelConfig{
		ID:               firstNonEmpty(stringFromMap(m, "id"), stringFromMap(m, "model")),
		Alias:            stringFromMap(m, "alias"),
		ContextWindow:    intFromMap(m, "context_window"),
		MaxOutputTokens:  intFromMap(m, "max_output_tokens"),
		ParseReasoning:   boolPtrFromMap(m, "parse_reasoning"),
		IncludeReasoning: boolPtrFromMap(m, "include_reasoning"),
		ThinkingParam:    stringFromMap(m, "thinking_param"),
		ReasoningEfforts: stringSliceFromMap(m, "reasoning_efforts"),
		VisionVia:        stringFromMap(m, "vision_via"),
	}
}

// ModelConfigForProviderModel returns the configured metadata entry for a model,
// matching either its upstream id, alias/display name, or configured reasoning-effort variants.
func ModelConfigForProviderModel(cfg *Config, providerName, modelName string) (ProviderModelConfig, bool) {
	if cfg == nil {
		return ProviderModelConfig{}, false
	}
	providerName = strings.TrimSpace(providerName)
	pc, ok := cfg.Providers[providerName]
	if !ok {
		return ProviderModelConfig{}, false
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = strings.TrimSpace(pc.Model)
	}
	if modelName == "" || len(pc.ModelConfigs) == 0 {
		return ProviderModelConfig{}, false
	}
	for _, entry := range pc.ModelConfigs {
		if providerModelConfigMatches(entry, modelName) {
			return entry, true
		}
	}
	return ProviderModelConfig{}, false
}

// DisplayModelForProviderModel returns the user-facing name for a configured model.
// When a models[] object defines an alias, the alias takes display precedence even
// if callers track the upstream model id internally. Configured reasoning-effort
// suffixes are preserved (for example upstream/model-high -> friendly-high).
func DisplayModelForProviderModel(cfg *Config, providerName, modelName string) string {
	modelName = strings.TrimSpace(modelName)
	entry, ok := ModelConfigForProviderModel(cfg, providerName, modelName)
	if !ok {
		return modelName
	}
	alias := strings.TrimSpace(entry.Alias)
	if alias == "" {
		return modelName
	}
	if effort := providerModelConfigMatchedEffort(entry, modelName); effort != "" {
		return alias + "-" + effort
	}
	return alias
}

// VisionViaForProviderModel returns the configured vision routing target for a model.
// A per-model models[].vision_via value overrides providers.<name>.vision_via.
// The value is a provider:model string understood by llm.ParseProviderModel.
func VisionViaForProviderModel(cfg *Config, providerName, modelName string) string {
	if entry, ok := ModelConfigForProviderModel(cfg, providerName, modelName); ok {
		if target := strings.TrimSpace(entry.VisionVia); target != "" {
			return target
		}
	}
	if cfg == nil {
		return ""
	}
	pc, ok := cfg.Providers[strings.TrimSpace(providerName)]
	if !ok {
		return ""
	}
	return strings.TrimSpace(pc.VisionVia)
}

func providerModelConfigMatches(entry ProviderModelConfig, modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return false
	}
	candidates := []string{entry.ID, entry.Alias, entry.DisplayName()}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == modelName {
			return true
		}
		for _, effort := range entry.ReasoningEfforts {
			effort = strings.TrimSpace(effort)
			if effort != "" && candidate+"-"+effort == modelName {
				return true
			}
		}
	}
	return false
}

func providerModelConfigMatchedEffort(entry ProviderModelConfig, modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	candidates := []string{entry.ID, entry.Alias, entry.DisplayName()}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for _, effort := range entry.ReasoningEfforts {
			effort = strings.TrimSpace(effort)
			if effort != "" && candidate+"-"+effort == modelName {
				return effort
			}
		}
	}
	return ""
}

func applyProviderModelConfigs(cfg *Config, modelConfigs map[string][]ProviderModelConfig) {
	if cfg == nil || len(modelConfigs) == 0 {
		return
	}
	for providerName, entries := range modelConfigs {
		pc, ok := cfg.Providers[providerName]
		if !ok {
			continue
		}
		pc.ModelConfigs = entries
		seen := make(map[string]bool, len(pc.Models)+len(entries))
		models := make([]string, 0, len(pc.Models)+len(entries))
		appendModel := func(model string) {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				return
			}
			seen[model] = true
			models = append(models, model)
		}
		for _, model := range pc.Models {
			appendModel(model)
		}
		for _, entry := range entries {
			name := entry.DisplayName()
			appendModel(name)
			for _, effort := range entry.ReasoningEfforts {
				effort = strings.TrimSpace(effort)
				if name != "" && effort != "" {
					appendModel(name + "-" + effort)
				}
			}
		}
		pc.Models = models
		cfg.Providers[providerName] = pc
	}
}

func mapFromAny(raw any) (map[string]any, bool) {
	switch m := raw.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for key, value := range m {
			out[fmt.Sprint(key)] = value
		}
		return out, true
	default:
		return nil, false
	}
}

func lookupMapValue(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	lower := strings.ToLower(key)
	for k, v := range m {
		if strings.ToLower(k) == lower {
			return v, true
		}
	}
	return nil, false
}

func stringFromMap(m map[string]any, key string) string {
	v, ok := lookupMapValue(m, key)
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func intFromMap(m map[string]any, key string) int {
	v, ok := lookupMapValue(m, key)
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case int32:
		return int(n)
	case uint64:
		return int(n)
	case uint:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	default:
		return 0
	}
}

func boolPtrFromMap(m map[string]any, key string) *bool {
	v, ok := lookupMapValue(m, key)
	if !ok || v == nil {
		return nil
	}
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

func stringSliceFromMap(m map[string]any, key string) []string {
	v, ok := lookupMapValue(m, key)
	if !ok || v == nil {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return trimStringSlice(s)
	default:
		value := reflect.ValueOf(v)
		if !value.IsValid() || value.Kind() != reflect.Slice {
			return nil
		}
		out := make([]string, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			item := strings.TrimSpace(fmt.Sprint(value.Index(i).Interface()))
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	}
}

func trimStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func markReasoningConfigPresence(reasoning *ReasoningConfig, v *viper.Viper) {
	if reasoning == nil || v == nil {
		return
	}
	reasoning.RawSet = v.InConfig("reasoning.raw")
	reasoning.ExtractTitlesSet = v.InConfig("reasoning.extract_titles")
	reasoning.PersistSummariesSet = v.InConfig("reasoning.persist_summaries")
}

// writeConfigPreservingEnvCase calls v.WriteConfig() but preserves the case
// of keys under providers.<name>.env. Viper unconditionally lowercases YAML
// keys (see github.com/spf13/viper#411), which silently breaks env vars like
// CLAUDE_CODE_OAUTH_TOKEN even when the caller is only touching unrelated
// settings (telegram tokens, agent prefs, ...). This helper snapshots env
// subsections from the raw file before the write and rewrites them after,
// so unrelated saves no longer corrupt env casing.
func writeConfigPreservingEnvCase(v *viper.Viper) error {
	configFile := v.ConfigFileUsed()
	envSnapshot, _ := snapshotProviderEnvSections(configFile)
	if err := v.WriteConfig(); err != nil {
		return err
	}
	if len(envSnapshot) == 0 {
		return nil
	}
	return rewriteProviderEnvSections(configFile, envSnapshot)
}

func snapshotProviderEnvSections(path string) (map[string]map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Providers map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for name, p := range raw.Providers {
		if len(p.Env) > 0 {
			out[name] = p.Env
		}
	}
	return out, nil
}

func rewriteProviderEnvSections(path string, snapshot map[string]map[string]string) error {
	if path == "" || len(snapshot) == 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	rootMap := root.Content[0]
	if rootMap.Kind != yaml.MappingNode {
		return nil
	}
	providersNode := findOrCreateChildMapping(rootMap, "providers")
	for name, env := range snapshot {
		providerNode := findOrCreateChildMapping(providersNode, name)
		envNode := findOrCreateChildMapping(providerNode, "env")
		envNode.Content = nil
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			envNode.Content = append(envNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Value: env[k]},
			)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return err
	}
	enc.Close()
	return writeFileAtomically(path, buf.Bytes(), 0o600)
}

// WriteFileAtomically replaces path with data using a temp-file + fsync + rename sequence.
// If path already exists, its mode is preserved; otherwise defaultPerm is used.
// A final-path symlink is followed so dotfile-managed config symlinks keep pointing
// at the original target instead of being replaced by a regular file.
func WriteFileAtomically(path string, data []byte, defaultPerm os.FileMode) error {
	writePath, err := resolveFinalSymlink(path)
	if err != nil {
		return err
	}

	perm := defaultPerm
	if info, err := os.Stat(writePath); err == nil {
		perm = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat existing file: %w", err)
	}
	return writeFileAtomically(writePath, data, perm)
}

func resolveFinalSymlink(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", fmt.Errorf("stat path: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, err)
	}
	return resolved, nil
}

func writeFileAtomically(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tf, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tf.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := tf.Write(data); err != nil {
		_ = tf.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tf.Chmod(perm); err != nil {
		_ = tf.Close()
		return fmt.Errorf("set temp file permissions: %w", err)
	}
	if err := tf.Sync(); err != nil {
		_ = tf.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tf.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	cleanup = false
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync config directory after replacing %s: %w", path, err)
	}

	return nil
}

func syncDirectory(dir string) error {
	df, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open config directory: %w", err)
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return fmt.Errorf("sync config directory: %w", err)
	}
	return nil
}

func findOrCreateChildMapping(parent *yaml.Node, key string) *yaml.Node {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			child := parent.Content[i+1]
			if child.Kind != yaml.MappingNode {
				child.Kind = yaml.MappingNode
				child.Tag = ""
				child.Value = ""
				child.Content = nil
			}
			return child
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	mapping := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content, keyNode, mapping)
	return mapping
}

func overlayProviderEnvFromRawConfig(cfg *Config) error {
	configFile := viper.ConfigFileUsed()
	if configFile == "" {
		return nil
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}

	var raw struct {
		Providers map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}

	if len(raw.Providers) == 0 {
		return nil
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}

	for name, provider := range raw.Providers {
		if provider.Env == nil {
			continue
		}
		providerCfg := cfg.Providers[name]
		providerCfg.Env = make(map[string]string, len(provider.Env))
		for k, v := range provider.Env {
			providerCfg.Env[k] = v
		}
		cfg.Providers[name] = providerCfg
	}

	return nil
}

// GetBuiltInProviderNames returns a list of all built-in provider type names.
func GetBuiltInProviderNames() []string {
	names := make([]string, 0, len(builtInProviderTypes))
	for name := range builtInProviderTypes {
		names = append(names, name)
	}
	return names
}

// ApplyOverrides applies provider and model overrides to the config.
// If provider is non-empty, it overrides the global provider.
// If model is non-empty, it overrides the model for the active provider.
func (c *Config) ApplyOverrides(provider, model string) {
	if provider != "" {
		c.DefaultProvider = provider
	}
	if model != "" && c.DefaultProvider != "" {
		cfg, ok := c.Providers[c.DefaultProvider]
		if !ok {
			// Initialize new provider config if it doesn't exist
			cfg = ProviderConfig{
				Model: model,
			}
		} else {
			cfg.Model = model
		}
		c.Providers[c.DefaultProvider] = cfg
	}
}

// ResolveProviderCredentials resolves and caches credentials for the named provider.
// Missing providers are ignored.
func (c *Config) ResolveProviderCredentials(name string) error {
	if c == nil || name == "" {
		return nil
	}
	providerCfg, ok := c.Providers[name]
	if !ok {
		return nil
	}
	if err := resolveProviderCredentials(name, &providerCfg); err != nil {
		return err
	}
	c.Providers[name] = providerCfg
	return nil
}

// GetResolvedProviderConfig returns the config for the specified provider name
// after resolving any deferred credential discovery needed for that provider.
// Returns nil if the provider is not configured.
func (c *Config) GetResolvedProviderConfig(name string) (*ProviderConfig, error) {
	if err := c.ResolveProviderCredentials(name); err != nil {
		return nil, err
	}
	if cfg, ok := c.Providers[name]; ok {
		return &cfg, nil
	}
	return nil, nil
}

// GetProviderConfig returns the config for the specified provider name.
// Returns nil if the provider is not configured.
func (c *Config) GetProviderConfig(name string) *ProviderConfig {
	if cfg, ok := c.Providers[name]; ok {
		return &cfg
	}
	return nil
}

// GetActiveProviderConfig returns the config for the default provider.
// Returns nil if the default provider is not configured.
func (c *Config) GetActiveProviderConfig() *ProviderConfig {
	return c.GetProviderConfig(c.DefaultProvider)
}

// needsLazyResolve checks if a value requires expensive resolution (1Password, files, commands, SRV)
func needsLazyResolve(value string) bool {
	return strings.HasPrefix(value, "op://") ||
		strings.HasPrefix(value, "srv://") ||
		strings.HasPrefix(value, "file://") ||
		(strings.HasPrefix(value, "$(") && strings.HasSuffix(value, ")"))
}

// resolveProviderCredentials resolves credentials for a provider based on its type.
// Expensive operations (op://, file://, srv://, $()) are deferred - call ResolveForInference() before use.
func resolveProviderCredentials(name string, cfg *ProviderConfig) error {
	if cfg.credentialsResolved || cfg.ResolvedAPIKey != "" || cfg.ResolvedURL != "" || cfg.OAuthCreds != nil {
		cfg.credentialsResolved = true
		return nil
	}

	providerType := InferProviderType(name, cfg.Type)

	// Check if URL fields need lazy resolution
	if needsLazyResolve(cfg.BaseURL) || needsLazyResolve(cfg.URL) {
		cfg.needsLazyResolution = true
	} else {
		// Resolve URL fields immediately (only env var expansion)
		cfg.BaseURL = expandEnv(cfg.BaseURL)
		cfg.URL = expandEnv(cfg.URL)
	}

	// Expand environment variables in other fields
	cfg.AppURL = expandEnv(cfg.AppURL)
	cfg.AppTitle = expandEnv(cfg.AppTitle)
	resolveProviderEnv(cfg)

	// Check if api_key uses magic syntax (op://, file://, $(), etc.)
	// If so, defer resolution until inference time
	if cfg.APIKey != "" && needsLazyResolve(cfg.APIKey) {
		cfg.needsLazyResolution = true
		cfg.credentialsResolved = true
		return nil
	}

	// Check if AWS Bedrock credential fields need lazy resolution
	if (cfg.AccessKey != "" && needsLazyResolve(cfg.AccessKey)) ||
		(cfg.SecretKey != "" && needsLazyResolve(cfg.SecretKey)) ||
		(cfg.SessionToken != "" && needsLazyResolve(cfg.SessionToken)) {
		cfg.needsLazyResolution = true
	}

	// Provider-specific credential resolution (non-lazy)
	switch providerType {
	case ProviderTypeAnthropic:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("ANTHROPIC_API_KEY")
		}

	case ProviderTypeOpenAI:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("OPENAI_API_KEY")
		}

	case ProviderTypeGemini:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("GEMINI_API_KEY")
		}

	case ProviderTypeGeminiCLI:
		creds, err := credentials.GetGeminiOAuthCredentials()
		if err != nil {
			return err
		}
		cfg.OAuthCreds = creds

	case ProviderTypeOpenRouter:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("OPENROUTER_API_KEY")
		}

	case ProviderTypeZen:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("ZEN_API_KEY")
		}
		// Empty API key is valid for free tier

	case ProviderTypeXAI:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("XAI_API_KEY")
		}

	case ProviderTypeVenice:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("VENICE_API_KEY")
		}

	case ProviderTypeNearAI:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("NEARAI_API_KEY")
		}

	case ProviderTypeSambaNova:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			cfg.ResolvedAPIKey = os.Getenv("SAMBANOVA_API_KEY")
		}

	case ProviderTypeBedrock:
		// Expand env vars in non-lazy credential fields (skip $() which is resolved later)
		if !needsLazyResolve(cfg.AccessKey) {
			cfg.AccessKey = expandEnv(cfg.AccessKey)
		}
		if !needsLazyResolve(cfg.SecretKey) {
			cfg.SecretKey = expandEnv(cfg.SecretKey)
		}
		if !needsLazyResolve(cfg.SessionToken) {
			cfg.SessionToken = expandEnv(cfg.SessionToken)
		}
		cfg.Region = expandEnv(cfg.Region)

	case ProviderTypeOpenAICompat, ProviderTypeVLLM:
		cfg.ResolvedAPIKey = expandEnv(cfg.APIKey)
		if cfg.ResolvedAPIKey == "" {
			// Try provider-specific env var (e.g., CEREBRAS_API_KEY for "cerebras")
			envName := strings.ToUpper(name) + "_API_KEY"
			cfg.ResolvedAPIKey = os.Getenv(envName)
		}
	}

	cfg.credentialsResolved = true
	return nil
}

func resolveProviderEnv(cfg *ProviderConfig) {
	if len(cfg.Env) == 0 {
		return
	}
	for k, v := range cfg.Env {
		if needsLazyResolve(v) {
			cfg.needsLazyResolution = true
			continue
		}
		cfg.Env[k] = expandEnv(v)
	}
}

// ResolveForInference performs lazy resolution of expensive config values (op://, file://, srv://, $()).
// Call this before creating a provider for inference.
func (cfg *ProviderConfig) ResolveForInference() error {
	if !cfg.needsLazyResolution {
		return nil
	}

	var err error

	// Resolve URL fields
	if needsLazyResolve(cfg.BaseURL) {
		cfg.ResolvedURL, err = ResolveValue(cfg.BaseURL)
		if err != nil {
			return fmt.Errorf("base_url: %w", err)
		}
	}
	if needsLazyResolve(cfg.URL) {
		cfg.ResolvedURL, err = ResolveValue(cfg.URL)
		if err != nil {
			return fmt.Errorf("url: %w", err)
		}
	}

	// Resolve API key
	if needsLazyResolve(cfg.APIKey) {
		cfg.ResolvedAPIKey, err = ResolveValue(cfg.APIKey)
		if err != nil {
			return fmt.Errorf("api_key: %w", err)
		}
	}

	// Resolve AWS Bedrock credential fields
	if needsLazyResolve(cfg.AccessKey) {
		cfg.AccessKey, err = ResolveValue(cfg.AccessKey)
		if err != nil {
			return fmt.Errorf("access_key_id: %w", err)
		}
	}
	if needsLazyResolve(cfg.SecretKey) {
		cfg.SecretKey, err = ResolveValue(cfg.SecretKey)
		if err != nil {
			return fmt.Errorf("secret_access_key: %w", err)
		}
	}
	if needsLazyResolve(cfg.SessionToken) {
		cfg.SessionToken, err = ResolveValue(cfg.SessionToken)
		if err != nil {
			return fmt.Errorf("session_token: %w", err)
		}
	}

	for k, v := range cfg.Env {
		if needsLazyResolve(v) {
			resolved, err := ResolveValue(v)
			if err != nil {
				return fmt.Errorf("env.%s: %w", k, err)
			}
			cfg.Env[k] = resolved
		}
	}

	cfg.needsLazyResolution = false
	return nil
}

// DescribeCredentialSource returns a human-readable description of which credential
// source will be used for the given provider. This is used by `config show` to help
// users understand where their credentials are coming from.
// Returns a short label (e.g., "ANTHROPIC_API_KEY env") and whether any credential was found.
func DescribeCredentialSource(name string, cfg *ProviderConfig) (string, bool) {
	providerType := InferProviderType(name, cfg.Type)

	// If there's a lazy-resolved api_key (op://, file://, $()), describe it
	if cfg.APIKey != "" && needsLazyResolve(cfg.APIKey) {
		return fmt.Sprintf("api_key (deferred: %s)", truncateValue(cfg.APIKey, 30)), true
	}

	switch providerType {
	case ProviderTypeAnthropic:
		return describeAnthropicCredential(cfg)
	case ProviderTypeOpenAI:
		return describeEnvKeyCredential(cfg, "OPENAI_API_KEY")
	case ProviderTypeGemini:
		return describeEnvKeyCredential(cfg, "GEMINI_API_KEY")
	case ProviderTypeGeminiCLI:
		if _, err := credentials.GetGeminiOAuthCredentials(); err == nil {
			return "gemini-cli OAuth (~/.gemini/oauth_creds.json)", true
		}
		return "gemini-cli OAuth (not found)", false
	case ProviderTypeOpenRouter:
		return describeEnvKeyCredential(cfg, "OPENROUTER_API_KEY")
	case ProviderTypeZen:
		source, found := describeEnvKeyCredential(cfg, "ZEN_API_KEY")
		if !found {
			return "none (free tier)", true // Zen works without a key
		}
		return source, found
	case ProviderTypeXAI:
		return describeEnvKeyCredential(cfg, "XAI_API_KEY")
	case ProviderTypeVenice:
		return describeEnvKeyCredential(cfg, "VENICE_API_KEY")
	case ProviderTypeNearAI:
		return describeEnvKeyCredential(cfg, "NEARAI_API_KEY")
	case ProviderTypeSambaNova:
		return describeEnvKeyCredential(cfg, "SAMBANOVA_API_KEY")
	case ProviderTypeClaudeBin:
		return "claude-bin CLI (no key needed)", true
	case ProviderTypeGrokBin:
		return "grok CLI login (no key needed)", true
	case ProviderTypeChatGPT:
		return "ChatGPT OAuth (interactive)", true
	case ProviderTypeCopilot:
		return "GitHub Copilot OAuth (interactive)", true
	case ProviderTypeOpenAICompat, ProviderTypeVLLM:
		envName := strings.ToUpper(name) + "_API_KEY"
		return describeEnvKeyCredential(cfg, envName)
	}

	return "unknown", false
}

// describeAnthropicCredential walks the Anthropic credential cascade and returns
// a description of which source will be used. Mirrors the logic in NewAnthropicProvider.
func describeAnthropicCredential(cfg *ProviderConfig) (string, bool) {
	// 1. Explicit API key from config
	apiKey := expandEnv(cfg.APIKey)
	if apiKey != "" {
		return "config api_key", true
	}

	// 2. ANTHROPIC_API_KEY env
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "ANTHROPIC_API_KEY env", true
	}

	return "none (set ANTHROPIC_API_KEY or config api_key)", false
}

// describeEnvKeyCredential checks config api_key then an environment variable.
func describeEnvKeyCredential(cfg *ProviderConfig, envName string) (string, bool) {
	apiKey := expandEnv(cfg.APIKey)
	if apiKey != "" {
		return "config api_key", true
	}
	if os.Getenv(envName) != "" {
		return envName + " env", true
	}
	return fmt.Sprintf("none (set %s or config api_key)", envName), false
}

// truncateValue truncates a string for display, adding "..." if too long.
func truncateValue(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// resolveImageCredentials resolves API credentials for all image providers
func resolveImageCredentials(cfg *ImageConfig) {
	// Gemini image credentials
	cfg.Gemini.APIKey = expandEnv(cfg.Gemini.APIKey)
	if cfg.Gemini.APIKey == "" {
		cfg.Gemini.APIKey = os.Getenv("GEMINI_API_KEY")
	}

	// OpenAI image credentials
	cfg.OpenAI.APIKey = expandEnv(cfg.OpenAI.APIKey)
	if cfg.OpenAI.APIKey == "" {
		cfg.OpenAI.APIKey = os.Getenv("OPENAI_API_KEY")
	}

	// xAI image credentials
	cfg.XAI.APIKey = expandEnv(cfg.XAI.APIKey)
	if cfg.XAI.APIKey == "" {
		cfg.XAI.APIKey = os.Getenv("XAI_API_KEY")
	}

	// Venice image credentials
	cfg.Venice.APIKey = expandEnv(cfg.Venice.APIKey)
	if cfg.Venice.APIKey == "" {
		cfg.Venice.APIKey = os.Getenv("VENICE_API_KEY")
	}

	// Flux (BFL) image credentials
	cfg.Flux.APIKey = expandEnv(cfg.Flux.APIKey)
	if cfg.Flux.APIKey == "" {
		cfg.Flux.APIKey = os.Getenv("BFL_API_KEY")
	}

	// OpenRouter image credentials
	cfg.OpenRouter.APIKey = expandEnv(cfg.OpenRouter.APIKey)
	if cfg.OpenRouter.APIKey == "" {
		cfg.OpenRouter.APIKey = os.Getenv("OPENROUTER_API_KEY")
	}
}

// resolveAudioCredentials resolves API credentials for audio providers.
func resolveAudioCredentials(cfg *AudioConfig) {
	cfg.Venice.APIKey = expandEnv(cfg.Venice.APIKey)
	if cfg.Venice.APIKey == "" {
		cfg.Venice.APIKey = os.Getenv("VENICE_API_KEY")
	}
	cfg.Gemini.APIKey = expandEnv(cfg.Gemini.APIKey)
	if cfg.Gemini.APIKey == "" {
		cfg.Gemini.APIKey = os.Getenv("GEMINI_API_KEY")
	}
	cfg.ElevenLabs.APIKey = expandEnv(cfg.ElevenLabs.APIKey)
	if cfg.ElevenLabs.APIKey == "" {
		cfg.ElevenLabs.APIKey = os.Getenv("ELEVENLABS_API_KEY")
	}
	if cfg.ElevenLabs.APIKey == "" {
		cfg.ElevenLabs.APIKey = os.Getenv("XI_API_KEY")
	}
}

// resolveMusicCredentials resolves API credentials for music providers.
func resolveMusicCredentials(cfg *MusicConfig) {
	cfg.Venice.APIKey = expandEnv(cfg.Venice.APIKey)
	if cfg.Venice.APIKey == "" {
		cfg.Venice.APIKey = os.Getenv("VENICE_API_KEY")
	}
	cfg.ElevenLabs.APIKey = expandEnv(cfg.ElevenLabs.APIKey)
	if cfg.ElevenLabs.APIKey == "" {
		cfg.ElevenLabs.APIKey = os.Getenv("ELEVENLABS_API_KEY")
	}
	if cfg.ElevenLabs.APIKey == "" {
		cfg.ElevenLabs.APIKey = os.Getenv("XI_API_KEY")
	}
}

// resolveTranscriptionCredentials resolves API credentials for transcription providers.
func resolveTranscriptionCredentials(cfg *TranscriptionConfig) {
	cfg.Venice.APIKey = expandEnv(cfg.Venice.APIKey)
	if cfg.Venice.APIKey == "" {
		cfg.Venice.APIKey = os.Getenv("VENICE_API_KEY")
	}
	cfg.ElevenLabs.APIKey = expandEnv(cfg.ElevenLabs.APIKey)
	if cfg.ElevenLabs.APIKey == "" {
		cfg.ElevenLabs.APIKey = os.Getenv("ELEVENLABS_API_KEY")
	}
	if cfg.ElevenLabs.APIKey == "" {
		cfg.ElevenLabs.APIKey = os.Getenv("XI_API_KEY")
	}
}

// resolveEmbedCredentials resolves credentials for all embedding providers
func resolveEmbedCredentials(cfg *EmbedConfig) {
	// OpenAI embed credentials
	cfg.OpenAI.APIKey = expandEnv(cfg.OpenAI.APIKey)
	if cfg.OpenAI.APIKey == "" {
		cfg.OpenAI.APIKey = os.Getenv("OPENAI_API_KEY")
	}

	// Gemini embed credentials
	cfg.Gemini.APIKey = expandEnv(cfg.Gemini.APIKey)
	if cfg.Gemini.APIKey == "" {
		cfg.Gemini.APIKey = os.Getenv("GEMINI_API_KEY")
	}

	// Jina embed credentials
	cfg.Jina.APIKey = expandEnv(cfg.Jina.APIKey)
	if cfg.Jina.APIKey == "" {
		cfg.Jina.APIKey = os.Getenv("JINA_API_KEY")
	}

	// Voyage embed credentials
	cfg.Voyage.APIKey = expandEnv(cfg.Voyage.APIKey)
	if cfg.Voyage.APIKey == "" {
		cfg.Voyage.APIKey = os.Getenv("VOYAGE_API_KEY")
	}

	// Ollama base URL
	cfg.Ollama.BaseURL = expandEnv(cfg.Ollama.BaseURL)
}

// resolveSearchCredentials resolves API credentials for all search providers
func resolveSearchCredentials(cfg *SearchConfig) {
	// Exa credentials
	cfg.Exa.APIKey = expandEnv(cfg.Exa.APIKey)
	if cfg.Exa.APIKey == "" {
		cfg.Exa.APIKey = os.Getenv("EXA_API_KEY")
	}

	// Exa MCP credentials (optional; remote MCP has a free tier without a key)
	cfg.ExaMCP.URL = expandEnv(cfg.ExaMCP.URL)
	cfg.ExaMCP.APIKey = expandEnv(cfg.ExaMCP.APIKey)
	if cfg.ExaMCP.APIKey == "" && (cfg.ExaMCP.URL == "" || cfg.ExaMCP.URL == DefaultSearchExaMCPURL) {
		cfg.ExaMCP.APIKey = os.Getenv("EXA_API_KEY")
	}

	// Perplexity credentials
	cfg.Perplexity.APIKey = expandEnv(cfg.Perplexity.APIKey)
	if cfg.Perplexity.APIKey == "" {
		cfg.Perplexity.APIKey = os.Getenv("PERPLEXITY_API_KEY")
	}

	// Tavily credentials
	cfg.Tavily.APIKey = expandEnv(cfg.Tavily.APIKey)
	if cfg.Tavily.APIKey == "" {
		cfg.Tavily.APIKey = os.Getenv("TAVILY_API_KEY")
	}

	// Brave credentials
	cfg.Brave.APIKey = expandEnv(cfg.Brave.APIKey)
	if cfg.Brave.APIKey == "" {
		cfg.Brave.APIKey = os.Getenv("BRAVE_API_KEY")
	}

	// Google credentials
	cfg.Google.APIKey = expandEnv(cfg.Google.APIKey)
	if cfg.Google.APIKey == "" {
		cfg.Google.APIKey = os.Getenv("GOOGLE_SEARCH_API_KEY")
	}
	cfg.Google.CX = expandEnv(cfg.Google.CX)
	if cfg.Google.CX == "" {
		cfg.Google.CX = os.Getenv("GOOGLE_SEARCH_CX")
	}
}

// ParseProviderModel splits "provider:model" into separate parts.
// Returns (provider, model). Model will be empty if not specified.
// This is a simple version that doesn't validate against configured providers.
func ParseProviderModel(s string) (provider, model string) {
	parts := strings.SplitN(s, ":", 2)
	provider = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		model = strings.TrimSpace(parts[1])
	}
	return provider, model
}

// expandEnv expands ${VAR} or $VAR in a string
func expandEnv(s string) string {
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		varName := s[2 : len(s)-1]
		return os.Getenv(varName)
	}
	if strings.HasPrefix(s, "$") {
		return os.Getenv(s[1:])
	}
	return s
}

// NormalizeVeniceAPIKey trims whitespace and strips an accidental "Bearer "
// prefix from a Venice API key. Shared across llm, image, and video packages.
func NormalizeVeniceAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if strings.HasPrefix(strings.ToLower(apiKey), "bearer ") {
		apiKey = strings.TrimSpace(apiKey[len("Bearer "):])
	}
	return apiKey
}

// GetConfigDir returns the XDG config directory for term-llm.
// Uses $XDG_CONFIG_HOME if set, otherwise ~/.config
func GetConfigDir() (string, error) {
	if xdgHome := os.Getenv("XDG_CONFIG_HOME"); xdgHome != "" {
		return filepath.Join(xdgHome, "term-llm"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".config", "term-llm"), nil
}

// GetConfigPath returns the path where the config file should be located
func GetConfigPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "config.yaml"), nil
}

// GetDiagnosticsDir returns the XDG data directory for term-llm diagnostics.
// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share
func GetDiagnosticsDir() string {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm", "diagnostics")
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "term-llm-diagnostics") // fallback
	}
	return filepath.Join(homeDir, ".local", "share", "term-llm", "diagnostics")
}

// GetDebugLogsDir returns the XDG data directory for term-llm debug logs.
// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share
func GetDebugLogsDir() string {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm", "debug")
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "term-llm-debug") // fallback
	}
	return filepath.Join(homeDir, ".local", "share", "term-llm", "debug")
}

// KnownAgentPreferenceKeys contains valid keys for agent preference configurations
var KnownAgentPreferenceKeys = map[string]bool{
	"provider":             true,
	"model":                true,
	"tools_enabled":        true,
	"tools_disabled":       true,
	"shell_allow":          true,
	"shell_auto_run":       true,
	"spawn_max_parallel":   true,
	"spawn_max_depth":      true,
	"spawn_timeout":        true,
	"spawn_allowed_agents": true,
	"max_turns":            true,
	"search":               true,
}

// IsKnownKey checks if a key path is a known configuration key
// For provider keys (providers.*), validates the sub-keys
// For agent preference keys (agents.preferences.*), validates the sub-keys
func IsKnownKey(keyPath string) bool {
	// Check direct match
	if KnownKeys[keyPath] {
		return true
	}

	// Check for providers.* pattern
	if strings.HasPrefix(keyPath, "providers.") {
		parts := strings.Split(keyPath, ".")
		if len(parts) == 2 {
			// providers.<name> is always valid
			return true
		}
		if len(parts) >= 3 {
			subKey := strings.Join(parts[2:], ".")
			if KnownProviderKeys[subKey] {
				return true
			}
			if len(parts) >= 4 && parts[2] == "env" {
				// providers.<name>.env.<VAR> - arbitrary subprocess env vars
				return true
			}
			if len(parts) >= 4 && parts[2] == "model_map" {
				// providers.<name>.model_map.<alias> - arbitrary friendly aliases
				return true
			}
		}
	}

	// Check for agents.preferences.* pattern
	if strings.HasPrefix(keyPath, "agents.preferences.") {
		parts := strings.SplitN(keyPath, ".", 4)
		if len(parts) == 3 {
			// agents.preferences.<agent-name> is always valid
			return true
		}
		if len(parts) == 4 {
			// agents.preferences.<agent-name>.<key> - check if <key> is valid
			return KnownAgentPreferenceKeys[parts[3]]
		}
	}

	return false
}

// Exists returns true if a config file exists
func Exists() bool {
	path, err := GetConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// NeedsSetup returns true if config file doesn't exist
func NeedsSetup() bool {
	return !Exists()
}

// Save writes the config to disk
func Save(cfg *Config) error {
	path, err := GetConfigPath()
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	type saveProvider struct {
		Type         ProviderType      `yaml:"type,omitempty"`
		Model        string            `yaml:"model,omitempty"`
		FastModel    string            `yaml:"fast_model,omitempty"`
		FastProvider string            `yaml:"fast_provider,omitempty"`
		ServiceTier  string            `yaml:"service_tier,omitempty"`
		Responses    ResponsesConfig   `yaml:"responses,omitempty"`
		FileUpload   *FileUploadConfig `yaml:"file_upload,omitempty"`
		BaseURL      string            `yaml:"base_url,omitempty"`
		AppURL       string            `yaml:"app_url,omitempty"`
		AppTitle     string            `yaml:"app_title,omitempty"`
	}
	type saveExec struct {
		Suggestions int `yaml:"suggestions"`
	}
	type saveImage struct {
		Provider string `yaml:"provider,omitempty"`
	}
	type saveConfig struct {
		DefaultProvider string                  `yaml:"default_provider,omitempty"`
		Exec            saveExec                `yaml:"exec"`
		Image           *saveImage              `yaml:"image,omitempty"`
		Providers       map[string]saveProvider `yaml:"providers,omitempty"`
	}

	saved := saveConfig{
		DefaultProvider: cfg.DefaultProvider,
		Exec:            saveExec{Suggestions: cfg.Exec.Suggestions},
		Providers:       make(map[string]saveProvider, len(cfg.Providers)),
	}
	if cfg.Image.Provider != "" {
		saved.Image = &saveImage{Provider: cfg.Image.Provider}
	}
	for name, p := range cfg.Providers {
		saved.Providers[name] = saveProvider{
			Type:         p.Type,
			Model:        p.Model,
			FastModel:    p.FastModel,
			FastProvider: p.FastProvider,
			ServiceTier:  p.ServiceTier,
			Responses:    p.Responses,
			FileUpload:   p.FileUpload,
			BaseURL:      p.BaseURL,
			AppURL:       p.AppURL,
			AppTitle:     p.AppTitle,
		}
	}

	data, err := yaml.Marshal(saved)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return writeFileAtomically(path, data, 0o600)
}

// SetAgentPreference sets a preference for a specific agent.
// Uses viper to merge with existing config.
// Supports "provider:model" format for the provider key (e.g., "chatgpt:gpt-5.2-codex").
// Returns a list of keys that were set (may be multiple for provider:model format).
func SetAgentPreference(agentName, key, value string) ([]string, error) {
	// Validate the key
	if !KnownAgentPreferenceKeys[key] {
		return nil, fmt.Errorf("unknown agent preference key: %s", key)
	}

	configPath, err := GetConfigPath()
	if err != nil {
		return nil, err
	}

	// Ensure config directory exists
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	// Load existing config using a separate viper instance
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	// Try to read existing config (ignore if doesn't exist)
	_ = v.ReadInConfig()

	var keysSet []string

	// Handle provider:model format
	if key == "provider" && strings.Contains(value, ":") {
		provider, model := ParseProviderModel(value)

		providerKey := fmt.Sprintf("agents.preferences.%s.provider", agentName)
		modelKey := fmt.Sprintf("agents.preferences.%s.model", agentName)

		v.Set(providerKey, provider)
		v.Set(modelKey, model)
		keysSet = append(keysSet, "provider", "model")
	} else {
		// Set the preference
		viperKey := fmt.Sprintf("agents.preferences.%s.%s", agentName, key)

		// Parse value based on key type
		switch key {
		case "max_turns", "spawn_max_parallel", "spawn_max_depth", "spawn_timeout":
			// Integer values
			var intVal int
			if _, err := fmt.Sscanf(value, "%d", &intVal); err != nil {
				return nil, fmt.Errorf("invalid integer value for %s: %s", key, value)
			}
			if intVal < 0 {
				return nil, fmt.Errorf("negative value not allowed for %s: %d", key, intVal)
			}
			v.Set(viperKey, intVal)
		case "search", "shell_auto_run":
			// Boolean values (case-insensitive)
			lowerVal := strings.ToLower(value)
			boolVal := lowerVal == "true" || value == "1" || lowerVal == "yes"
			v.Set(viperKey, boolVal)
		case "tools_enabled", "tools_disabled", "shell_allow", "spawn_allowed_agents":
			// Array values (comma-separated)
			if value == "" {
				v.Set(viperKey, []string{})
			} else {
				parts := strings.Split(value, ",")
				for i := range parts {
					parts[i] = strings.TrimSpace(parts[i])
				}
				v.Set(viperKey, parts)
			}
		default:
			// String values
			v.Set(viperKey, value)
		}
		keysSet = append(keysSet, key)
	}

	return keysSet, writeConfigPreservingEnvCase(v)
}

// GetAgentPreference returns the preferences for a specific agent.
func GetAgentPreference(agentName string) (AgentPreference, bool) {
	cfg, err := Load()
	if err != nil {
		return AgentPreference{}, false
	}

	if cfg.Agents.Preferences == nil {
		return AgentPreference{}, false
	}

	pref, ok := cfg.Agents.Preferences[agentName]
	return pref, ok
}

// ClearAgentPreferences removes all preferences for a specific agent.
func ClearAgentPreferences(agentName string) error {
	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	// Load existing config
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return nil // Nothing to clear
		}
		return err
	}

	// Get all preferences
	prefs := v.GetStringMap("agents.preferences")
	if prefs == nil {
		return nil // Nothing to clear
	}

	// Remove this agent's preferences
	delete(prefs, agentName)

	// Set the updated preferences map
	if len(prefs) == 0 {
		// Remove the entire preferences section if empty
		v.Set("agents.preferences", nil)
	} else {
		v.Set("agents.preferences", prefs)
	}

	return writeConfigPreservingEnvCase(v)
}

// SetServeTelegramConfig saves Telegram bot configuration using viper.
// Merges with existing config rather than overwriting.
func SetServeTelegramConfig(c TelegramServeConfig) error {
	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")
	_ = v.ReadInConfig()

	v.Set("serve.telegram.token", c.Token)
	v.Set("serve.telegram.allowed_user_ids", c.AllowedUserIDs)
	v.Set("serve.telegram.allowed_usernames", c.AllowedUsernames)
	if c.IdleTimeout > 0 {
		v.Set("serve.telegram.idle_timeout", c.IdleTimeout)
	}
	if c.InterruptTimeout > 0 {
		v.Set("serve.telegram.interrupt_timeout", c.InterruptTimeout)
	}

	return writeConfigPreservingEnvCase(v)
}

// SetServeWebPushConfig saves Web Push VAPID configuration using viper.
func SetServeWebPushConfig(c WebPushConfig) error {
	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")
	_ = v.ReadInConfig()

	v.Set("serve.web_push.vapid_public_key", c.VAPIDPublicKey)
	v.Set("serve.web_push.vapid_private_key", c.VAPIDPrivateKey)
	if c.Subject != "" {
		v.Set("serve.web_push.subject", c.Subject)
	}

	return writeConfigPreservingEnvCase(v)
}
