package config

import (
	"sort"
	"strings"
)

// KeySpec describes a user-visible configuration key. New user-visible
// mapstructure fields should be added here so defaults, validation,
// completions, config show, and reset templates do not drift apart.
type KeySpec struct {
	Path          string
	Default       any
	HasDefault    bool
	Description   string
	Sensitive     bool
	ShowInConfig  bool
	ResetTemplate bool
	Placeholder   any
}

// ProviderFieldSpec describes a field valid under providers.<name>.
type ProviderFieldSpec struct {
	Path        string
	Description string
	Sensitive   bool
	Placeholder any
}

// DefaultField stores an ordered provider default.
type DefaultField struct {
	Path  string
	Value any
}

// ProviderSpec describes a built-in provider's canonical runtime/config
// defaults. ConfigDefault controls whether the defaults are registered with
// Viper and written to generated config templates; runtime-only provider specs
// are still used by direct constructors and fast-model helpers.
type ProviderSpec struct {
	Name          string
	Type          ProviderType
	ConfigDefault bool
	ShowInConfig  bool
	ResetTemplate bool
	Defaults      []DefaultField
}

const (
	DefaultConfigProvider = "anthropic"

	DefaultApprovalMode = "prompt"

	DefaultAskMaxTurns     = 50
	DefaultChatMaxTurns    = 200
	DefaultExecSuggestions = 3

	DefaultAssistantInstructions = "You are a helpful assistant. Today's date is {{date}}."
	DefaultChatTerminalTitle     = "smart"
	DefaultEditContextLines      = 3
	DefaultEditDiffFormat        = "auto"

	DefaultImageProvider         = "gemini"
	DefaultImageOutputDir        = "~/Pictures/term-llm"
	DefaultImageGeminiModel      = "gemini-2.5-flash-image"
	DefaultImageOpenAIModel      = "gpt-image-2"
	DefaultImageChatGPTModel     = "gpt-5.4-mini"
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

	DefaultTranscriptionProvider        = "openai"
	DefaultTranscriptionOpenAIModel     = "whisper-1"
	DefaultTranscriptionMistralModel    = "voxtral-mini-latest"
	DefaultTranscriptionVeniceModel     = "nvidia/parakeet-tdt-0.6b-v3"
	DefaultTranscriptionElevenLabsModel = "scribe_v2"

	DefaultEmbedOpenAIModel   = "text-embedding-3-small"
	DefaultEmbedGeminiModel   = "gemini-embedding-001"
	DefaultEmbedJinaModel     = "jina-embeddings-v3"
	DefaultEmbedVoyageModel   = "voyage-3.5"
	DefaultEmbedOllamaModel   = "nomic-embed-text"
	DefaultOllamaBaseURL      = "http://127.0.0.1:11434"
	DefaultEmbedOllamaBaseURL = DefaultOllamaBaseURL

	DefaultSearchProvider      = "exa_mcp"
	DefaultSearchFetchProvider = "jina"
	DefaultSearchExaMCPURL     = "https://mcp.exa.ai/mcp"

	DefaultReasoningMaxSummaryChars = 12000
	DefaultReasoningMaxRawChars     = 20000
	DefaultReasoningHiddenLabel     = "Thinking..."

	DefaultToolsShellAutoRunEnv    = "TERM_LLM_ALLOW_AUTORUN"
	DefaultToolsShellNonTTYEnv     = "TERM_LLM_ALLOW_NON_TTY"
	DefaultToolsMaxToolOutputChars = 20000

	DefaultSessionsEnabled          = true
	DefaultSessionsMaxAgeDays       = 0
	DefaultSessionsMaxCount         = 0
	DefaultSessionsStripImageBase64 = false

	DefaultFileTrackingMaxFileBytes    = 2 * 1024 * 1024
	DefaultFileTrackingMaxSessionBytes = 100 * 1024 * 1024
	DefaultFileTrackingMaxTotalBytes   = int64(1024 * 1024 * 1024)

	DefaultSkillsMetadataBudgetTokens = 8000
	DefaultSkillsMaxVisibleSkills     = 50

	DefaultServeBasePath        = "/ui"
	DefaultServeResponseTimeout = "30m"

	DefaultAutoCompact = true
)

var keySpecs = []KeySpec{
	def("default_provider", DefaultConfigProvider),
	def("auto_compact", DefaultAutoCompact),

	def("approval.default_mode", DefaultApprovalMode),

	optional("guardian.provider"),
	optional("guardian.model"),
	optional("guardian.policy_path"),
	optional("guardian.timeout_seconds", withPlaceholder(0)),

	optional("exec.provider"),
	optional("exec.model"),
	def("exec.suggestions", DefaultExecSuggestions),
	def("exec.instructions", ""),

	optional("ask.provider"),
	optional("ask.model"),
	def("ask.instructions", DefaultAssistantInstructions),
	def("ask.max_turns", DefaultAskMaxTurns),

	optional("chat.provider"),
	optional("chat.model"),
	def("chat.instructions", DefaultAssistantInstructions),
	def("chat.max_turns", DefaultChatMaxTurns),
	def("chat.terminal_title", DefaultChatTerminalTitle),
	def("chat.terminal_title_format", ""),
	def("chat.terminal_progress", false),

	optional("edit.provider"),
	optional("edit.model"),
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

	optional("embed.provider"),
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
	optional("search.tavily.api_key", sensitive()),
	optional("search.brave.api_key", sensitive()),
	optional("search.google.api_key", sensitive()),
	optional("search.google.cx"),

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

	def("serve.base_path", DefaultServeBasePath),
	optional("serve.platforms", withPlaceholder([]string{})),
	optional("serve.title"),
	def("serve.disable_location_sharing", false),
	optional("serve.files_dir"),
	optional("serve.widgets_dir"),
	def("serve.response_timeout", DefaultServeResponseTimeout),
	optional("serve.telegram.token", sensitive()),
	optional("serve.telegram.allowed_user_ids", withPlaceholder([]int64{})),
	optional("serve.telegram.allowed_usernames", withPlaceholder([]string{})),
	optional("serve.telegram.idle_timeout", withPlaceholder(30)),
	optional("serve.telegram.interrupt_timeout", withPlaceholder(3)),
	optional("serve.web_push.vapid_public_key", sensitive()),
	optional("serve.web_push.vapid_private_key", sensitive()),
	optional("serve.web_push.subject"),

	def("file_tracking.enabled", false),
	def("file_tracking.max_file_bytes", DefaultFileTrackingMaxFileBytes),
	def("file_tracking.max_session_bytes", DefaultFileTrackingMaxSessionBytes),
	def("file_tracking.max_total_bytes", int(DefaultFileTrackingMaxTotalBytes)),
	def("file_tracking.path", ""),
}

var providerFieldSpecs = []ProviderFieldSpec{
	{Path: "type"},
	{Path: "api_key", Sensitive: true},
	{Path: "model"},
	{Path: "fast_model"},
	{Path: "fast_provider"},
	{Path: "service_tier"},
	{Path: "models", Placeholder: []string{}},
	{Path: "reasoning"},
	{Path: "credentials"},
	{Path: "env", Placeholder: map[string]any{}},
	{Path: "enable_hooks", Placeholder: false},
	{Path: "use_websocket", Placeholder: false},
	{Path: "responses.reasoning_mode"},
	{Path: "responses.reasoning_context"},
	{Path: "responses.multi_agent.enabled", Placeholder: false},
	{Path: "responses.multi_agent.max_concurrent_subagents", Placeholder: 0},
	{Path: "responses.programmatic_tool_calling.enabled", Placeholder: false},
	{Path: "responses.programmatic_tool_calling.tools", Placeholder: []string{}},
	{Path: "responses.prompt_cache.mode"},
	{Path: "responses.prompt_cache.ttl"},
	{Path: "file_upload.native_mime_types", Placeholder: []string{}},
	{Path: "file_upload.max_native_bytes", Placeholder: 0},
	{Path: "file_upload.text_embed_mime_types", Placeholder: []string{}},
	{Path: "file_upload.max_text_embed_bytes", Placeholder: 0},
	{Path: "vision_via"},
	{Path: "use_native_search", Placeholder: false},
	{Path: "context_window", Placeholder: 0},
	{Path: "max_output_tokens", Placeholder: 0},
	{Path: "base_url"},
	{Path: "url"},
	{Path: "no_stream_options", Placeholder: false},
	{Path: "vllm_thinking_param"},
	{Path: "parse_reasoning", Placeholder: false},
	{Path: "include_reasoning", Placeholder: false},
	{Path: "thinking_param"},
	{Path: "app_url"},
	{Path: "app_title"},
	{Path: "region"},
	{Path: "profile"},
	{Path: "access_key_id", Sensitive: true},
	{Path: "secret_access_key", Sensitive: true},
	{Path: "session_token", Sensitive: true},
	{Path: "model_map", Placeholder: map[string]any{}},
	{Path: "think", Placeholder: false},
	{Path: "top_k", Placeholder: 0},
	{Path: "min_p", Placeholder: 0.0},
	{Path: "presence_penalty", Placeholder: 0.0},
	{Path: "num_ctx", Placeholder: 0},
	{Path: "num_predict", Placeholder: 0},
}

var providerSpecs = []ProviderSpec{
	{
		Name: "anthropic", Type: ProviderTypeAnthropic, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "claude-sonnet-4-6"}, {"fast_model", "claude-haiku-4-5"}},
	},
	{
		Name: "openai", Type: ProviderTypeOpenAI, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "gpt-5.6-sol"}, {"fast_model", "gpt-5.6-luna"}, {"use_websocket", false}},
	},
	{
		Name: "chatgpt", Type: ProviderTypeChatGPT, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "gpt-5.6-sol-medium"}, {"fast_model", "gpt-5.6-luna"}, {"use_websocket", true}},
	},
	{
		Name: "gemini", Type: ProviderTypeGemini, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "gemini-3-flash-preview"}, {"fast_model", "gemini-2.5-flash-lite"}},
	},
	{
		Name: "openrouter", Type: ProviderTypeOpenRouter, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "x-ai/grok-code-fast-1"}, {"fast_model", "anthropic/claude-haiku-4-5"}, {"app_url", "https://github.com/samsaffron/term-llm"}, {"app_title", "term-llm"}},
	},
	{
		Name: "xai", Type: ProviderTypeXAI, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "grok-4-1-fast"}, {"fast_model", "grok-3-mini-fast"}},
	},
	{
		Name: "venice", Type: ProviderTypeVenice, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "venice-uncensored"}, {"fast_model", "llama-3.2-3b"}},
	},
	{
		Name: "nearai", Type: ProviderTypeNearAI, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "zai-org/GLM-5.1-FP8"}, {"fast_model", "Qwen/Qwen3.6-35B-A3B-FP8"}},
	},
	{
		Name: "sambanova", Type: ProviderTypeSambaNova, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "gpt-oss-120b"}, {"fast_model", "Meta-Llama-3.3-70B-Instruct"}},
	},
	{
		Name: "zen", Type: ProviderTypeZen, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults: []DefaultField{{"model", "minimax-m2.5-free"}, {"fast_model", "minimax-m2.5-free"}},
	},
	{
		Name: "copilot", Type: ProviderTypeCopilot,
		Defaults: []DefaultField{{"model", "gpt-4.1"}, {"fast_model", "gpt-4.1"}},
	},
	{
		Name: "gemini-cli", Type: ProviderTypeGeminiCLI,
		Defaults: []DefaultField{{"model", "gemini-3-flash-preview"}, {"fast_model", "gemini-2.5-flash-lite"}},
	},
	{
		Name: "claude-bin", Type: ProviderTypeClaudeBin,
		Defaults: []DefaultField{{"model", "sonnet"}, {"fast_model", "haiku"}},
	},
	{
		Name: "grok-bin", Type: ProviderTypeGrokBin,
		Defaults: []DefaultField{{"model", "grok-4.5"}, {"fast_model", "grok-composer-2.5-fast"}},
	},
	{
		Name: "ollama", Type: ProviderTypeOllama,
		Defaults: []DefaultField{{"model", "qwen2.5-coder:7b"}, {"fast_model", "qwen2.5-coder:7b"}, {"base_url", DefaultOllamaBaseURL}},
	},
	{
		Name: "vllm", Type: ProviderTypeVLLM,
		Defaults: []DefaultField{{"fast_model", "Qwen/Qwen3.5-122B-A10B"}},
	},
	{
		Name: "bedrock", Type: ProviderTypeBedrock,
		Defaults: []DefaultField{{"fast_model", "claude-haiku-4-5"}},
	},
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

func withPlaceholder(v any) specOpt {
	return func(s *KeySpec) { s.Placeholder = v }
}

// ConfigKeySpecs returns the ordered canonical key schema.
func ConfigKeySpecs() []KeySpec {
	out := make([]KeySpec, len(keySpecs))
	copy(out, keySpecs)
	return out
}

// ProviderKeySpecs returns valid provider field specs in display order.
func ProviderKeySpecs() []ProviderFieldSpec {
	out := make([]ProviderFieldSpec, len(providerFieldSpecs))
	copy(out, providerFieldSpecs)
	return out
}

// DefaultProviderSpecs returns canonical built-in provider defaults.
func DefaultProviderSpecs() []ProviderSpec {
	out := make([]ProviderSpec, len(providerSpecs))
	copy(out, providerSpecs)
	for i := range out {
		out[i].Defaults = append([]DefaultField(nil), providerSpecs[i].Defaults...)
	}
	return out
}

// DefaultProviderNames returns provider names whose defaults should be shown in
// generated config output.
func DefaultProviderNames() []string {
	var names []string
	for _, spec := range providerSpecs {
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
	for _, provider := range providerSpecs {
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
	for _, spec := range providerSpecs {
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
	for _, spec := range providerSpecs {
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
	for _, spec := range providerFieldSpecs {
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
