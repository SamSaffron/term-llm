package config

import (
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/providerspec"
)

// KeySpec describes a user-visible configuration key. New user-visible
// mapstructure fields should be added here so defaults, validation,
// completions, config show, and reset templates do not drift apart.
type KeySpec struct {
	Path           string
	Default        any
	HasDefault     bool
	Description    string
	Sensitive      bool
	RedactExplicit bool
	ShowInConfig   bool
	ResetTemplate  bool
	Placeholder    any
}

// ProviderFieldSpec describes a shared providers.<name> schema field.
type ProviderFieldSpec = providerspec.FieldSpec

// Provider metadata lives in the leaf registry. These aliases preserve the config API.
type DefaultField = providerspec.DefaultField
type ProviderSpec = providerspec.Spec
type ProviderCredential = providerspec.Credential

const (
	CredentialAPIKey = providerspec.CredentialAPIKey
	CredentialOAuth  = providerspec.CredentialOAuth
	CredentialAWS    = providerspec.CredentialAWS
	CredentialNone   = providerspec.CredentialNone
)

func BuiltinProviders() []ProviderSpec                 { return providerspec.All() }
func BuiltinProvider(name string) (ProviderSpec, bool) { return providerspec.Lookup(name) }
func BuiltinProviderOfType(t ProviderType) (ProviderSpec, bool) {
	s, ok := providerspec.ByType(t)
	return s, ok && t != ProviderTypeOpenAICompat
}

const (
	DefaultConfigProvider = "openrouter"

	DefaultAskMaxTurns            = 50
	DefaultAskStdinInlineMaxBytes = int64(10 * 1024)
	DefaultAskStdinMaxBytes       = int64(20 * 1024 * 1024)
	DefaultChatMaxTurns           = 200
	DefaultExecSuggestions        = 3

	DefaultAssistantInstructions = "You are a helpful assistant."
	DefaultChatTerminalTitle     = "smart"
	DefaultLifecycleOSC          = "off"
	DefaultLifecycleSinkTimeout  = "2s"
	DefaultShareProvider         = "github"
	DefaultShareTimeout          = "120s"
	DefaultEditContextLines      = 3
	DefaultEditDiffFormat        = "auto"

	DefaultImageProvider         = "gemini"
	DefaultImageOutputDir        = "~/Pictures/term-llm"
	DefaultImageGeminiModel      = "gemini-2.5-flash-image"
	DefaultImageOpenAIModel      = "gpt-image-2"
	DefaultImageChatGPTModel     = "gpt-image-2.5-flare"
	DefaultImageXAIModel         = "grok-2-image-1212"
	DefaultImageVeniceModel      = "nano-banana-pro"
	DefaultImageVeniceResolution = "2K"
	DefaultImageFluxModel        = "flux-2-pro"
	DefaultImageFluxEditModel    = "flux-kontext-pro"
	DefaultImageOpenRouterModel  = "google/gemini-2.5-flash-image"
	DefaultImageDebugDelay       = 0.0

	DefaultAudioProvider         = "venice"
	DefaultAudioOutputDir        = "~/Music/term-llm"
	DefaultAudioVeniceModel      = "tts-kokoro"
	DefaultAudioVeniceVoice      = "af_sky"
	DefaultAudioVeniceFormat     = "mp3"
	DefaultAudioVeniceSpeed      = 1.0
	DefaultAudioGeminiModel      = "gemini-3.1-flash-tts-preview"
	DefaultAudioGeminiVoice      = "Kore"
	DefaultAudioGeminiFormat     = "wav"
	DefaultAudioElevenLabsModel  = "eleven_multilingual_v2"
	DefaultAudioElevenLabsVoice  = "JBFqnCBsd6RMkjVDRZzb"
	DefaultAudioElevenLabsFormat = "mp3_44100_128"

	DefaultMusicProvider         = "venice"
	DefaultMusicOutputDir        = "~/Music/term-llm"
	DefaultMusicVeniceModel      = "elevenlabs-sound-effects-v2"
	DefaultMusicVeniceFormat     = "mp3"
	DefaultMusicElevenLabsModel  = "music_v1"
	DefaultMusicElevenLabsFormat = "mp3_44100_128"
	DefaultMusicPollInterval     = "2s"
	DefaultMusicPollTimeout      = "10m"

	// LiveProviderChatGPT speaks the realtime protocol directly using ChatGPT OAuth.
	LiveProviderChatGPT = "chatgpt"
	// LiveProviderOpenAI uses the public Realtime API with an API key.
	LiveProviderOpenAI = "openai"
	// LiveProviderGemini uses Gemini Live with server-proxied PCM audio.
	LiveProviderGemini       = "gemini"
	DefaultLiveOpenAIModel   = "gpt-live-1"
	DefaultLiveOpenAIVoice   = "marin"
	DefaultLiveOpenAIBaseURL = "https://api.openai.com/v1"
	DefaultLiveGeminiModel   = "gemini-3.8-live"
	DefaultLiveGeminiVoice   = "Kore"
	DefaultLiveGeminiBaseURL = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

	DefaultLiveProvider               = LiveProviderChatGPT
	DefaultLiveIdleTimeout            = "10m"
	DefaultLiveClassifyStatus         = 0.60
	DefaultLiveClassifyNewSession     = 0.70
	DefaultLiveClassifySwitchSession  = 0.70
	DefaultLiveClassifySteerNow       = 0.80
	DefaultLiveClassifySide           = 0.85
	DefaultLiveChatGPTModel           = "gpt-live-1-codex"
	DefaultLiveChatGPTVoice           = "cove"
	DefaultLiveChatGPTCallBaseURL     = "https://chatgpt.com/backend-api/codex"
	DefaultLiveChatGPTSidebandBaseURL = "https://api.openai.com/v1"

	DefaultTranscriptionProvider        = "openai"
	DefaultTranscriptionOpenAIModel     = "whisper-1"
	DefaultTranscriptionMistralModel    = "voxtral-mini-latest"
	DefaultTranscriptionVeniceModel     = "nvidia/parakeet-tdt-0.6b-v3"
	DefaultTranscriptionElevenLabsModel = "scribe_v2"

	DefaultEmbedOpenAIModel   = "text-embedding-3-small"
	DefaultEmbedChatGPTModel  = DefaultEmbedOpenAIModel
	DefaultEmbedVeniceModel   = "text-embedding-qwen3-8b"
	DefaultEmbedGeminiModel   = "gemini-embedding-001"
	DefaultEmbedJinaModel     = "jina-embeddings-v3"
	DefaultEmbedVoyageModel   = "voyage-3.5"
	DefaultEmbedOllamaModel   = "nomic-embed-text"
	DefaultOllamaBaseURL      = "http://127.0.0.1:11434"
	DefaultEmbedOllamaBaseURL = DefaultOllamaBaseURL

	DefaultSearchProvider      = "exa_mcp"
	DefaultSearchFetchProvider = "jina"
	DefaultSearchExaMCPURL     = "https://mcp.exa.ai/mcp"

	DefaultTypeSafeModel          = "jev-latest"
	DefaultTypeSafeBaseURL        = "https://api.typesafe.ai"
	DefaultTypeSafeTimeoutSeconds = 10

	DefaultOpenAIDecisionsModel          = "gpt-6-luna"
	DefaultOpenAIDecisionsBaseURL        = "https://api.openai.com/v1"
	DefaultOpenAIDecisionsTimeoutSeconds = 10

	DefaultCloudflareClassifyModel = "clef"
	// {account_id} is replaced with CLOUDFLARE_ACCOUNT_ID when the request is built.
	DefaultCloudflareClassifyBaseURL        = "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/run"
	DefaultCloudflareClassifyTimeoutSeconds = 10

	DefaultReasoningMaxSummaryChars = 12000
	DefaultReasoningMaxRawChars     = 20000
	DefaultReasoningHiddenLabel     = "Thinking..."

	DefaultToolsShellAutoRunEnv    = "TERM_LLM_ALLOW_AUTORUN"
	DefaultToolsShellNonTTYEnv     = "TERM_LLM_ALLOW_NON_TTY"
	DefaultToolsMaxToolOutputChars = 20000

	DefaultToolDiscoveryMode           = "auto"
	DefaultToolDiscoveryStrategy       = "auto"
	DefaultToolDiscoveryThreshold      = 24
	DefaultToolDiscoveryMaxActiveTools = 300

	DefaultSessionsEnabled          = true
	DefaultSessionsMaxAgeDays       = 0
	DefaultSessionsMaxCount         = 0
	DefaultSessionsStripImageBase64 = false

	DefaultFileTrackingEnabled                    = true
	DefaultFileTrackingMaxFileBytes               = 2 * 1024 * 1024
	DefaultFileTrackingMaxSessionBytes            = 100 * 1024 * 1024
	DefaultFileTrackingMaxTotalBytes              = int64(1024 * 1024 * 1024)
	DefaultFileTrackingMaxObservationRows         = 10000
	DefaultFileTrackingMaxObservationSessionRows  = 1000
	DefaultFileTrackingMaxObservationBytes        = int64(16 * 1024 * 1024)
	DefaultFileTrackingMaxObservationSessionBytes = int64(2 * 1024 * 1024)
	DefaultFileTrackingMaxObservationAgeDays      = 30

	DefaultSkillsMetadataBudgetTokens = 8000
	DefaultSkillsMaxVisibleSkills     = 50

	DefaultServeBasePath        = "/ui"
	DefaultServeResponseTimeout = "30m"
	DefaultServeAutoTitle       = true

	DefaultAutoCompact = true
)

// DefaultLinkedDevicesICEServers returns the canonical default ICE server list.
// Explicit ice_servers: [] in config overrides this to disable all servers.
func DefaultLinkedDevicesICEServers() []LinkedDevicesICEServerConfig {
	return []LinkedDevicesICEServerConfig{
		{
			URLs: []string{"stun:stun.term-llm.com:3478"},
		},
		{
			URLs:             []string{"turns:stun.term-llm.com:5349?transport=tcp"},
			CredentialSource: "broker",
		},
	}
}

var keySpecs = []KeySpec{
	def("default_provider", DefaultConfigProvider),
	def("provider_discovery", ProviderDiscoveryAuto),
	def("auto_compact", DefaultAutoCompact),

	optional("approval.default_mode", withoutResetTemplate()),

	def("guardian.backend", "llm"),
	optional("guardian.classify.provider"),
	def("guardian.classify.min_confidence", 0.15),
	optional("guardian.fallback.provider"),
	optional("guardian.fallback.model"),
	optional("guardian.fallback.log_path"),
	optional("guardian.provider"),
	optional("guardian.model"),
	optional("guardian.policy_path"),
	optional("guardian.timeout_seconds", withPlaceholder(0)),
	def("guardian.classify_all_shell", false),

	optional("exec.provider"),
	optional("exec.model"),
	optional("exec.approval_mode", withoutResetTemplate()),
	def("exec.suggestions", DefaultExecSuggestions),
	def("exec.instructions", ""),

	optional("ask.provider"),
	optional("ask.model"),
	optional("ask.approval_mode", withoutResetTemplate()),
	def("ask.instructions", DefaultAssistantInstructions),
	def("ask.max_turns", DefaultAskMaxTurns),
	def("ask.stdin_inline_max_bytes", DefaultAskStdinInlineMaxBytes),
	def("ask.stdin_max_bytes", DefaultAskStdinMaxBytes),

	optional("chat.provider"),
	optional("chat.model"),
	optional("chat.approval_mode", withoutResetTemplate()),
	def("chat.instructions", DefaultAssistantInstructions),
	def("chat.max_turns", DefaultChatMaxTurns),
	def("chat.terminal_title", DefaultChatTerminalTitle),
	def("chat.terminal_title_format", ""),
	def("chat.terminal_progress", false),

	def("commit.message_agent", "commit-message"),

	def("lifecycle.enabled", true),
	def("lifecycle.adapters", []string{"auto"}),
	def("lifecycle.osc", DefaultLifecycleOSC),
	def("lifecycle.commands", []map[string]any{}),

	def("share.provider", DefaultShareProvider),
	def("share.command", []string{}),
	def("share.timeout", DefaultShareTimeout),

	optional("edit.provider"),
	optional("edit.model"),
	optional("edit.approval_mode", withoutResetTemplate()),
	def("edit.instructions", ""),
	def("edit.show_line_numbers", true),
	def("edit.context_lines", DefaultEditContextLines),
	optional("edit.editor"),
	def("edit.diff_format", DefaultEditDiffFormat),

	def("image.provider", DefaultImageProvider),
	def("image.output_dir", DefaultImageOutputDir),
	optional("image.gemini.api_key", sensitive()),
	def("image.gemini.model", DefaultImageGeminiModel),
	optional("image.gemini.image_size"),
	optional("image.openai.api_key", sensitive()),
	def("image.openai.model", DefaultImageOpenAIModel),
	def("image.chatgpt.model", DefaultImageChatGPTModel),
	optional("image.xai.api_key", sensitive()),
	def("image.xai.model", DefaultImageXAIModel),
	optional("image.venice.api_key", sensitive()),
	def("image.venice.model", DefaultImageVeniceModel),
	optional("image.venice.edit_model"),
	def("image.venice.resolution", DefaultImageVeniceResolution),
	optional("image.flux.api_key", sensitive()),
	def("image.flux.model", DefaultImageFluxModel),
	optional("image.openrouter.api_key", sensitive()),
	def("image.openrouter.model", DefaultImageOpenRouterModel),
	def("image.debug.delay", DefaultImageDebugDelay),

	def("audio.provider", DefaultAudioProvider),
	def("audio.output_dir", DefaultAudioOutputDir),
	optional("audio.venice.api_key", sensitive()),
	def("audio.venice.model", DefaultAudioVeniceModel),
	def("audio.venice.voice", DefaultAudioVeniceVoice),
	def("audio.venice.format", DefaultAudioVeniceFormat),
	optional("audio.gemini.api_key", sensitive()),
	def("audio.gemini.model", DefaultAudioGeminiModel),
	def("audio.gemini.voice", DefaultAudioGeminiVoice),
	def("audio.gemini.format", DefaultAudioGeminiFormat),
	optional("audio.elevenlabs.api_key", sensitive()),
	def("audio.elevenlabs.model", DefaultAudioElevenLabsModel),
	def("audio.elevenlabs.voice", DefaultAudioElevenLabsVoice),
	def("audio.elevenlabs.format", DefaultAudioElevenLabsFormat),

	def("music.provider", DefaultMusicProvider),
	def("music.output_dir", DefaultMusicOutputDir),
	optional("music.venice.api_key", sensitive()),
	def("music.venice.model", DefaultMusicVeniceModel),
	def("music.venice.format", DefaultMusicVeniceFormat),
	optional("music.elevenlabs.api_key", sensitive()),
	def("music.elevenlabs.model", DefaultMusicElevenLabsModel),
	def("music.elevenlabs.format", DefaultMusicElevenLabsFormat),

	def("transcription.provider", DefaultTranscriptionProvider),
	optional("transcription.model"),
	def("transcription.save_dir", ""),
	def("transcription.timestamps", false),
	optional("transcription.venice.api_key", sensitive()),
	def("transcription.venice.model", DefaultTranscriptionVeniceModel),
	optional("transcription.elevenlabs.api_key", sensitive()),
	def("transcription.elevenlabs.model", DefaultTranscriptionElevenLabsModel),

	def("live.enabled", false),
	def("live.provider", DefaultLiveProvider),
	optional("live.instructions"),
	def("live.idle_timeout", DefaultLiveIdleTimeout),
	// Opt-in separately from live.enabled. Agent adds a model turn before every
	// request; classify adds one bounded classification call instead.
	def("live.control_plane", string(LiveControlPlaneOff)),
	optional("live.classify.provider"),
	def("live.classify.log_decisions", true),
	def("live.classify.log_state", true),
	def("live.classify.min_confidence.status", DefaultLiveClassifyStatus),
	def("live.classify.min_confidence.new_session", DefaultLiveClassifyNewSession),
	def("live.classify.min_confidence.switch_session", DefaultLiveClassifySwitchSession),
	def("live.classify.min_confidence.steer_now", DefaultLiveClassifySteerNow),
	def("live.classify.min_confidence.side", DefaultLiveClassifySide),
	// Unset means "follow the provider's fast model", which is the shared dial.
	optional("live.control_provider"),
	optional("live.control_model"),
	optional("live.openai.api_key", sensitive()),
	def("live.openai.model", DefaultLiveOpenAIModel),
	def("live.openai.voice", DefaultLiveOpenAIVoice),
	def("live.openai.base_url", DefaultLiveOpenAIBaseURL),
	optional("live.openai.headers", withPlaceholder(map[string]any{})),
	optional("live.gemini.api_key", sensitive()),
	def("live.gemini.model", DefaultLiveGeminiModel),
	def("live.gemini.voice", DefaultLiveGeminiVoice),
	def("live.gemini.base_url", DefaultLiveGeminiBaseURL),
	def("live.chatgpt.model", DefaultLiveChatGPTModel),
	def("live.chatgpt.voice", DefaultLiveChatGPTVoice),
	def("live.chatgpt.call_base_url", DefaultLiveChatGPTCallBaseURL),
	def("live.chatgpt.sideband_base_url", DefaultLiveChatGPTSidebandBaseURL),

	optional("embed.provider"),
	def("embed.batch_size", 32),
	optional("embed.openai.api_key", sensitive()),
	def("embed.openai.model", DefaultEmbedOpenAIModel),
	optional("embed.gemini.api_key", sensitive()),
	def("embed.gemini.model", DefaultEmbedGeminiModel),
	optional("embed.jina.api_key", sensitive()),
	def("embed.jina.model", DefaultEmbedJinaModel),
	optional("embed.voyage.api_key", sensitive()),
	def("embed.voyage.model", DefaultEmbedVoyageModel),
	def("embed.ollama.base_url", DefaultEmbedOllamaBaseURL),
	def("embed.ollama.model", DefaultEmbedOllamaModel),

	def("search.provider", DefaultSearchProvider),
	def("search.fetch_provider", DefaultSearchFetchProvider),
	def("search.force_external", false),
	optional("search.exa.api_key", sensitive()),
	def("search.exa_mcp.url", DefaultSearchExaMCPURL),
	optional("search.exa_mcp.api_key", sensitive()),
	optional("search.perplexity.api_key", sensitive()),
	optional("search.parallel.api_key", sensitive()),
	optional("search.tavily.api_key", sensitive()),
	optional("search.brave.api_key", sensitive()),
	optional("search.google.api_key", sensitive()),
	optional("search.google.cx"),

	def("classify.default_provider", "typesafe"),
	optional("classify.providers.typesafe.type"),
	optional("classify.providers.typesafe.api_key", sensitive()),
	def("classify.providers.typesafe.model", DefaultTypeSafeModel),
	def("classify.providers.typesafe.base_url", DefaultTypeSafeBaseURL),
	def("classify.providers.typesafe.timeout_seconds", DefaultTypeSafeTimeoutSeconds),
	def("classify.providers.typesafe.images", ClassifyImagesAssets),
	optional("classify.providers.typesafe.headers", withPlaceholder(map[string]any{})),

	def("reasoning.display", ReasoningDisplayAuto),
	def("reasoning.source", ReasoningSourceSummaryOrProviderSafe),
	def("reasoning.status", ReasoningStatusTitle),
	def("reasoning.history", ReasoningHistoryCollapsed),
	def("reasoning.export", ReasoningExportAsk),
	def("reasoning.raw", false),
	def("reasoning.max_summary_chars", DefaultReasoningMaxSummaryChars),
	def("reasoning.max_raw_chars", DefaultReasoningMaxRawChars),
	def("reasoning.extract_titles", true),
	def("reasoning.hidden_label", DefaultReasoningHiddenLabel),
	def("reasoning.persist_summaries", true),
	def("reasoning.chat.display", ReasoningInherit),
	def("reasoning.chat.status", ReasoningInherit),
	def("reasoning.chat.history", ReasoningInherit),
	def("reasoning.chat.export", ReasoningInherit),
	optional("reasoning.chat.raw", withPlaceholder(false)),
	def("reasoning.ask.display", ReasoningInherit),
	def("reasoning.ask.status", ReasoningInherit),
	def("reasoning.ask.history", ReasoningInherit),
	def("reasoning.ask.export", ReasoningInherit),
	optional("reasoning.ask.raw", withPlaceholder(false)),
	def("reasoning.serve.display", ReasoningInherit),
	def("reasoning.serve.status", ReasoningInherit),
	def("reasoning.serve.history", ReasoningInherit),
	def("reasoning.serve.export", ReasoningInherit),
	optional("reasoning.serve.raw", withPlaceholder(false)),
	def("reasoning.jobs.display", ReasoningInherit),
	def("reasoning.jobs.status", ReasoningInherit),
	def("reasoning.jobs.history", ReasoningInherit),
	def("reasoning.jobs.export", ReasoningInherit),
	optional("reasoning.jobs.raw", withPlaceholder(false)),

	optional("theme.primary"),
	optional("theme.secondary"),
	optional("theme.success"),
	optional("theme.error"),
	optional("theme.warning"),
	optional("theme.muted"),
	optional("theme.text"),
	optional("theme.spinner"),
	optional("theme.reasoning_summary"),
	optional("theme.reasoning_header"),
	optional("theme.reasoning_raw"),

	def("tools.enabled", []string{}),
	def("tools.read_dirs", []string{}),
	def("tools.write_dirs", []string{}),
	def("tools.shell_allow", []string{}),
	def("tools.shell_auto_run", false),
	def("tools.shell_auto_run_env", DefaultToolsShellAutoRunEnv),
	def("tools.shell_non_tty_env", DefaultToolsShellNonTTYEnv),
	optional("tools.image_provider"),
	def("tools.max_tool_output_chars", DefaultToolsMaxToolOutputChars),
	def("tools.grep_rerank.enabled", DefaultGrepRerankEnabled),
	optional("tools.grep_rerank.provider"),
	def("tools.grep_rerank.threshold", DefaultGrepRerankThreshold),
	def("tools.grep_rerank.timeout_ms", DefaultGrepRerankTimeoutMS),
	def("tools.grep_rerank.max_candidates", DefaultGrepRerankMaxCandidates),

	def("tool_discovery.mode", DefaultToolDiscoveryMode),
	def("tool_discovery.strategy", DefaultToolDiscoveryStrategy),
	def("tool_discovery.threshold", DefaultToolDiscoveryThreshold),
	def("tool_discovery.max_active_tools", DefaultToolDiscoveryMaxActiveTools),

	def("agents.use_builtin", true),
	def("agents.search_paths", []string{}),
	optional("agents.preferences", withPlaceholder(map[string]any{})),

	def("skills.enabled", true),
	def("skills.auto_invoke", true),
	def("skills.metadata_budget_tokens", DefaultSkillsMetadataBudgetTokens),
	def("skills.max_visible_skills", DefaultSkillsMaxVisibleSkills),
	def("skills.include_project_skills", true),
	def("skills.include_ecosystem_paths", true),
	def("skills.always_enabled", []string{}),
	def("skills.never_auto", []string{}),

	def("agents_md.enabled", true),

	def("sessions.enabled", DefaultSessionsEnabled),
	def("sessions.max_age_days", DefaultSessionsMaxAgeDays),
	def("sessions.max_count", DefaultSessionsMaxCount),
	def("sessions.path", ""),
	def("sessions.strip_image_base64", DefaultSessionsStripImageBase64),

	def("diagnostics.enabled", false),
	def("diagnostics.dir", ""),
	def("debug_logs.enabled", false),
	def("debug_logs.dir", ""),

	optional("loop.approval_mode", withoutResetTemplate()),

	def("serve.base_path", DefaultServeBasePath),
	def("serve.auto_title", DefaultServeAutoTitle),
	optional("serve.platforms", withPlaceholder([]string{})),
	optional("serve.approval_mode", withoutResetTemplate()),
	optional("serve.title"),
	def("serve.disable_location_sharing", false),
	optional("serve.files_dir"),
	optional("serve.widgets_dir"),
	optional("serve.extensions_dir"),
	optional("serve.extensions"),
	def("serve.response_timeout", DefaultServeResponseTimeout),
	optional("serve.telegram.token", sensitive()),
	optional("serve.telegram.allowed_user_ids", withPlaceholder([]int64{})),
	optional("serve.telegram.allowed_usernames", withPlaceholder([]string{})),
	optional("serve.telegram.idle_timeout", withPlaceholder(30)),
	optional("serve.telegram.interrupt_timeout", withPlaceholder(3)),
	optional("serve.web_push.vapid_public_key", sensitive()),
	optional("serve.web_push.vapid_private_key", sensitive()),
	optional("serve.web_push.subject"),
	optional("serve.mcp.approval_mode", withoutResetTemplate()),
	def("serve.projects.enabled", true),

	def("file_tracking.enabled", DefaultFileTrackingEnabled),
	def("file_tracking.max_file_bytes", DefaultFileTrackingMaxFileBytes),
	def("file_tracking.max_session_bytes", DefaultFileTrackingMaxSessionBytes),
	def("file_tracking.max_total_bytes", int(DefaultFileTrackingMaxTotalBytes)),
	def("file_tracking.max_observation_rows", DefaultFileTrackingMaxObservationRows),
	def("file_tracking.max_observation_session_rows", DefaultFileTrackingMaxObservationSessionRows),
	def("file_tracking.max_observation_bytes", int(DefaultFileTrackingMaxObservationBytes)),
	def("file_tracking.max_observation_session_bytes", int(DefaultFileTrackingMaxObservationSessionBytes)),
	def("file_tracking.max_observation_age_days", DefaultFileTrackingMaxObservationAgeDays),
	def("file_tracking.path", ""),

	optional("linked_devices.broker_url", described("HTTPS rendezvous broker URL; loopback HTTP is allowed for development.")),
	optional("linked_devices.admission_credential", sensitive(), described("Optional private credential used only to admit host registration at the broker.")),
	def("linked_devices.ice_servers", DefaultLinkedDevicesICEServers(), redactExplicit(), described("Ordered STUN/TURN policy shared by the host and approved devices; an explicit empty list uses host candidates only.")),
}

type specOpt func(*KeySpec)

func def(path string, value any, opts ...specOpt) KeySpec {
	s := KeySpec{Path: path, Default: value, HasDefault: true, ShowInConfig: true, ResetTemplate: true}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

func optional(path string, opts ...specOpt) KeySpec {
	s := KeySpec{Path: path, ShowInConfig: true, ResetTemplate: true, Placeholder: ""}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

func sensitive() specOpt {
	return func(s *KeySpec) { s.Sensitive = true }
}

func redactExplicit() specOpt {
	return func(s *KeySpec) { s.RedactExplicit = true }
}

func described(description string) specOpt {
	return func(s *KeySpec) { s.Description = description }
}

func withPlaceholder(v any) specOpt {
	return func(s *KeySpec) { s.Placeholder = v }
}

func withoutResetTemplate() specOpt {
	return func(s *KeySpec) { s.ResetTemplate = false }
}

// ConfigKeySpecs returns the ordered canonical key schema.
func ConfigKeySpecs() []KeySpec {
	out := make([]KeySpec, len(keySpecs))
	copy(out, keySpecs)
	return out
}

// ProviderKeySpecs returns valid provider field specs in display order.
func ProviderKeySpecs() []ProviderFieldSpec { return providerspec.Fields() }

// DefaultProviderSpecs returns canonical built-in provider defaults.
func DefaultProviderSpecs() []ProviderSpec { return providerspec.All() }

// DefaultProviderNames returns provider names whose defaults should be shown in
// generated config output.
func DefaultProviderNames() []string {
	var names []string
	for _, spec := range providerspec.All() {
		if spec.ShowInConfig || spec.ConfigDefault || spec.ResetTemplate {
			names = append(names, spec.Name)
		}
	}
	return names
}

// GetDefaults returns a map of all default configuration values.
func GetDefaults() map[string]any {
	defaults := make(map[string]any)
	for _, spec := range keySpecs {
		if spec.HasDefault {
			defaults[spec.Path] = spec.Default
		}
	}
	for _, provider := range providerspec.All() {
		if !provider.ConfigDefault {
			continue
		}
		for _, field := range provider.Defaults {
			defaults["providers."+provider.Name+"."+field.Path] = field.Value
		}
	}
	return defaults
}

// DefaultForKey returns the canonical default for a non-provider config key.
func DefaultForKey(path string) (any, bool) {
	for _, spec := range keySpecs {
		if spec.Path == path && spec.HasDefault {
			return spec.Default, true
		}
	}
	return nil, false
}

func defaultString(path string) string {
	v, ok := DefaultForKey(path)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func defaultInt(path string) int {
	v, ok := DefaultForKey(path)
	if !ok {
		return 0
	}
	s, _ := v.(int)
	return s
}

func defaultBool(path string) bool {
	v, ok := DefaultForKey(path)
	if !ok {
		return false
	}
	s, _ := v.(bool)
	return s
}

// DefaultProviderValue returns a canonical default field for a built-in provider
// name or provider type.
func DefaultProviderValue(name, field string) (any, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	field = strings.TrimSpace(field)
	if name == "" || field == "" {
		return nil, false
	}
	for _, spec := range providerspec.All() {
		if spec.Name != name && string(spec.Type) != name {
			continue
		}
		for _, def := range spec.Defaults {
			if def.Path == field {
				return def.Value, true
			}
		}
	}
	return nil, false
}

// DefaultProviderModel returns the canonical default chat model for a built-in provider.
func DefaultProviderModel(name string) string {
	if v, ok := DefaultProviderValue(name, "model"); ok {
		s, _ := v.(string)
		return s
	}
	return ""
}

// DefaultProviderFastModel returns the canonical lightweight model for a built-in provider.
func DefaultProviderFastModel(name string) string {
	if v, ok := DefaultProviderValue(name, "fast_model"); ok {
		s, _ := v.(string)
		return s
	}
	return ""
}

// DefaultProviderFastModels returns the default fast-model map keyed by provider type/name.
func DefaultProviderFastModels() map[string]string {
	out := make(map[string]string)
	for _, spec := range providerspec.All() {
		fast := ""
		for _, def := range spec.Defaults {
			if def.Path == "fast_model" {
				fast, _ = def.Value.(string)
				break
			}
		}
		if fast == "" {
			continue
		}
		out[spec.Name] = fast
		if spec.Type != "" {
			out[string(spec.Type)] = fast
		}
	}
	return out
}

func buildKnownKeys() map[string]bool {
	known := make(map[string]bool)
	for _, spec := range keySpecs {
		addPathAndParents(known, spec.Path)
	}
	known["providers"] = true
	return known
}

func buildKnownProviderKeys() map[string]bool {
	known := make(map[string]bool)
	for _, spec := range providerspec.Fields() {
		addPathAndParents(known, spec.Path)
	}
	return known
}

func addPathAndParents(known map[string]bool, path string) {
	parts := strings.Split(path, ".")
	for i := 1; i <= len(parts); i++ {
		known[strings.Join(parts[:i], ".")] = true
	}
}

// KnownKeys contains all valid non-dynamic configuration key paths.
var KnownKeys = buildKnownKeys()

// KnownProviderKeys contains valid keys for provider configurations.
var KnownProviderKeys = buildKnownProviderKeys()

// KnownKeyPaths returns all statically known config key paths in sorted order.
func KnownKeyPaths() []string {
	keys := make([]string, 0, len(KnownKeys))
	for key := range KnownKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// SchemaDefaultKeys returns the default-bearing config keys in sorted order.
func SchemaDefaultKeys() []string {
	defaults := GetDefaults()
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
