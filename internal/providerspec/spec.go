// Package providerspec owns declarative metadata for inference provider types.
// It is a leaf package: provider constructors and live model probes are registered
// by their owning packages rather than imported here.
package providerspec

import (
	"os"
	"strings"
)

// Type identifies a provider implementation.
type Type string

const (
	TypeAnthropic    Type = "anthropic"
	TypeOpenAI       Type = "openai"
	TypeChatGPT      Type = "chatgpt"
	TypeGrok         Type = "grok"
	TypeCopilot      Type = "copilot"
	TypeGemini       Type = "gemini"
	TypeOpenRouter   Type = "openrouter"
	TypeZen          Type = "zen"
	TypeOpenCodeGo   Type = "opencode-go"
	TypeClaudeBin    Type = "claude-bin"
	TypeGrokBin      Type = "grok-bin"
	TypeCursorBin    Type = "cursor-bin"
	TypeAgyBin       Type = "agy-bin"
	TypeOpenAICompat Type = "openai_compatible"
	TypeVLLM         Type = "vllm"
	TypeXAI          Type = "xai"
	TypeVenice       Type = "venice"
	TypeNearAI       Type = "nearai"
	TypeSambaNova    Type = "sambanova"
	TypeObscurify    Type = "obscurify"
	TypeBedrock      Type = "bedrock"
	TypeOllama       Type = "ollama"
)

// FieldSpec describes a field valid under providers.<name>.
type FieldSpec struct {
	Path        string
	Description string
	Sensitive   bool
	Placeholder any
}

// Fields returns a copy of the shared provider config schema.
func Fields() []FieldSpec { out := make([]FieldSpec, len(fields)); copy(out, fields); return out }

// DefaultField stores an ordered provider default.
type DefaultField struct {
	Path  string
	Value any
}

// Spec is the registry entry for a built-in provider: everything
// term-llm knows about it before reading any config. Name lists, provider
// types, API-key environment variables, `term-llm providers` output, setup
// hints, and the setup wizard all derive from specs.
//
// ConfigDefault controls whether Defaults are applied by Load and
// written to generated config templates; runtime-only defaults are still used
// by direct constructors and fast-model helpers.
type Spec struct {
	Name                    string
	Aliases                 []string
	CredentialLabel         string
	Wizard                  WizardSpec
	ModelListing            ModelListing
	ModelListTimeoutSeconds int
	// FixedToolBridge marks CLI-backed providers whose tool set is fixed by an
	// external bridge (tool discovery cannot add or remove tools).
	FixedToolBridge bool
	CacheOnly       bool
	Availability    Availability
	Type            Type
	ConfigDefault   bool
	ShowInConfig    bool
	ResetTemplate   bool
	Defaults        []DefaultField

	Description string
	Credential  Credential
	// APIKeyEnv holds the provider's API key when config does not set one.
	// Setting it also enables the provider without a providers.<name> block.
	APIKeyEnv string
	// HostEnv names the provider's server; setting it enables the provider.
	HostEnv string
	// Setup is the step that sets the provider up, when it is not simply
	// "set APIKeyEnv".
	Setup string
	// ListModels reports whether the provider can list models upstream. It is
	// derived from ModelListing (anything but ModelListingCurated or empty).
	ListModels bool
}

// ModelListing selects the model-lister implementation in cmd; curated means
// the provider has a local catalog and cannot list models upstream.
type ModelListing string

const (
	ModelListingAnthropic  ModelListing = "anthropic"
	ModelListingAPIKey     ModelListing = "api_key"
	ModelListingChatGPT    ModelListing = "chatgpt"
	ModelListingGrok       ModelListing = "grok"
	ModelListingCurated    ModelListing = "curated"
	ModelListingNearAI     ModelListing = "nearai"
	ModelListingZen        ModelListing = "zen"
	ModelListingOpenCodeGo ModelListing = "opencode-go"
	ModelListingCopilot    ModelListing = "copilot"
	ModelListingGrokBin    ModelListing = "grok-bin"
	ModelListingCursorBin  ModelListing = "cursor-bin"
	ModelListingAgyBin     ModelListing = "agy-bin"
	ModelListingClaudeBin  ModelListing = "claude-bin"
	ModelListingOllama     ModelListing = "ollama"
	ModelListingCompatible ModelListing = "compatible"
)

// Availability identifies the local credential probe, if any.
type Availability string

const (
	AvailabilityChatGPT   Availability = "chatgpt"
	AvailabilityGrok      Availability = "grok"
	AvailabilityClaude    Availability = "claude"
	AvailabilityCopilot   Availability = "copilot"
	AvailabilityGrokBin   Availability = "grok-bin"
	AvailabilityCursorBin Availability = "cursor-bin"
	AvailabilityAgyBin    Availability = "agy-bin"
)

// Availabilities lists every defined local credential probe kind.
func Availabilities() []Availability {
	return []Availability{AvailabilityChatGPT, AvailabilityGrok, AvailabilityClaude, AvailabilityCopilot, AvailabilityGrokBin, AvailabilityCursorBin, AvailabilityAgyBin}
}

// WizardSpec controls setup wizard order and presentation; zero Order omits the provider.
type WizardSpec struct {
	Order       int
	Label, Hint string
	Bootstrap   bool // setup can start an interactive OAuth login
}

// Credential describes how a built-in provider authenticates.
type Credential string

const (
	CredentialAPIKey Credential = "api_key" // API key from config or APIKeyEnv
	CredentialOAuth  Credential = "oauth"   // term-llm sign-in or a signed-in companion CLI
	CredentialAWS    Credential = "aws"     // AWS credential chain
	CredentialNone   Credential = "none"    // local server or an installed CLI
)

// EnablingEnv returns the environment variables whose presence enables the
// provider without a providers.<name> block.
func (s Spec) EnablingEnv() []string {
	var vars []string
	for _, v := range []string{s.APIKeyEnv, s.HostEnv} {
		if v != "" {
			vars = append(vars, v)
		}
	}
	return vars
}

// EnabledByEnv returns the environment variable currently enabling the
// provider, or "" when none is set.
func (s Spec) EnabledByEnv() string {
	for _, v := range s.EnablingEnv() {
		if strings.TrimSpace(os.Getenv(v)) != "" {
			return v
		}
	}
	return ""
}

// SetupHint returns the step that would set the provider up.
func (s Spec) SetupHint() string {
	switch {
	case s.Setup != "":
		return s.Setup
	case s.APIKeyEnv != "":
		return "set " + s.APIKeyEnv
	default:
		return "add providers." + s.Name + " to config.yaml"
	}
}

// RequiresAPIKey reports whether the provider cannot run without an API key.
func (s Spec) RequiresAPIKey() bool {
	return s.Credential == CredentialAPIKey
}

// All returns built-in specs in display order, with independent default slices.
func All() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	for i := range out {
		out[i] = cloneSpec(specs[i])
	}
	return out
}

func cloneSpec(s Spec) Spec {
	s.Defaults = append([]DefaultField(nil), s.Defaults...)
	s.Aliases = append([]string(nil), s.Aliases...)
	return s
}

// Lookup finds a built-in by its exact name.
func Lookup(name string) (Spec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return cloneSpec(s), true
		}
	}
	return Spec{}, false
}

// LookupSessionAlias maps a lower-cased provider label from a saved chat
// session ("github copilot" or "github copilot (gpt-4.1)") to its built-in.
// It exists only for resuming sessions whose labels differ from the provider name.
func LookupSessionAlias(label string) (Spec, bool) {
	for _, s := range specs {
		for _, a := range s.Aliases {
			if label == a || strings.HasPrefix(label, a+" (") {
				return cloneSpec(s), true
			}
		}
	}
	return Spec{}, false
}

// ByType finds the spec for a provider type, including the generic custom type.
func ByType(t Type) (Spec, bool) {
	if t == TypeOpenAICompat {
		return customSpec, true
	}
	for _, s := range specs {
		if s.Type == t {
			return cloneSpec(s), true
		}
	}
	return Spec{}, false
}

// Types returns every supported provider type, including generic custom endpoints.
func Types() []Spec { out := All(); return append(out, customSpec) }

var customSpec = Spec{Name: "openai_compatible", Type: TypeOpenAICompat, ModelListing: ModelListingCompatible, Credential: CredentialNone}

func init() {
	for i := range specs {
		specs[i].ListModels = listsModels(specs[i].ModelListing)
	}
	customSpec.ListModels = listsModels(customSpec.ModelListing)
}

func listsModels(l ModelListing) bool { return l != "" && l != ModelListingCurated }

var specs = []Spec{
	{
		Name: "anthropic", Type: TypeAnthropic, ModelListing: ModelListingAnthropic, Wizard: WizardSpec{Order: 4, Label: "Anthropic - API key", Hint: "set ANTHROPIC_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "claude-sonnet-4-6"}, {"fast_model", "claude-haiku-4-5"}},
		Description: "Anthropic API (Claude models)",
		Credential:  CredentialAPIKey, APIKeyEnv: "ANTHROPIC_API_KEY",
	},
	{
		Name: "openai", Type: TypeOpenAI, ModelListing: ModelListingAPIKey, Wizard: WizardSpec{Order: 9, Label: "OpenAI - OPENAI_API_KEY", Hint: "set OPENAI_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "gpt-5.6-sol"}, {"fast_model", "gpt-5.6-luna"}, {"use_websocket", false}},
		Description: "OpenAI Responses API",
		Credential:  CredentialAPIKey, APIKeyEnv: "OPENAI_API_KEY",
	},
	{
		Name: "chatgpt", ModelListTimeoutSeconds: 30, CredentialLabel: "ChatGPT OAuth (interactive)", Type: TypeChatGPT, ModelListing: ModelListingChatGPT, Availability: AvailabilityChatGPT, Wizard: WizardSpec{Order: 2, Label: "ChatGPT (Codex) - ChatGPT OAuth", Hint: "run 'term-llm auth login chatgpt' to sign in", Bootstrap: true}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "gpt-5.6-sol-medium"}, {"fast_model", "gpt-5.6-luna"}, {"use_websocket", true}},
		Description: "ChatGPT via native OAuth (ChatGPT Plus/Pro subscription)",
		Credential:  CredentialOAuth, Setup: "run term-llm auth login chatgpt",
	},
	{
		Name: "grok", ModelListTimeoutSeconds: 30, CredentialLabel: "Grok subscription OAuth (interactive)", Type: TypeGrok, ModelListing: ModelListingGrok, Availability: AvailabilityGrok, Wizard: WizardSpec{Order: 3, Label: "Grok Subscription - xAI OAuth", Hint: "run 'term-llm auth login grok' to sign in", Bootstrap: true}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "grok-4.6"}, {"fast_model", "grok-4.6"}},
		Description: "Grok subscription via native xAI device OAuth and Responses API",
		Credential:  CredentialOAuth, Setup: "run term-llm auth login grok",
	},
	{
		Name: "gemini", ModelListing: ModelListingCurated, Type: TypeGemini, Wizard: WizardSpec{Order: 10, Label: "Gemini - GEMINI_API_KEY", Hint: "set GEMINI_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "gemini-3-flash-preview"}, {"fast_model", "gemini-2.5-flash-lite"}},
		Description: "Google Gemini API (consumer API key)",
		Credential:  CredentialAPIKey, APIKeyEnv: "GEMINI_API_KEY",
	},
	{
		Name: "openrouter", Type: TypeOpenRouter, ModelListing: ModelListingAPIKey, Wizard: WizardSpec{Order: 1, Label: "OpenRouter - free models (API key required)", Hint: "set OPENROUTER_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "openrouter/free"}, {"fast_model", "openrouter/free"}, {"app_url", "https://github.com/samsaffron/term-llm"}, {"app_title", "term-llm"}},
		Description: "OpenRouter API (access to many providers)",
		Credential:  CredentialAPIKey, APIKeyEnv: "OPENROUTER_API_KEY",
	},
	{
		Name: "xai", Type: TypeXAI, ModelListing: ModelListingAPIKey, Wizard: WizardSpec{Order: 11, Label: "xAI - XAI_API_KEY", Hint: "set XAI_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "grok-4-1-fast"}, {"fast_model", "grok-3-mini-fast"}},
		Description: "xAI API (Grok models)",
		Credential:  CredentialAPIKey, APIKeyEnv: "XAI_API_KEY",
	},
	{
		Name: "venice", Type: TypeVenice, ModelListing: ModelListingAPIKey, Wizard: WizardSpec{Order: 12, Label: "Venice - VENICE_API_KEY", Hint: "set VENICE_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "venice-uncensored"}, {"fast_model", "llama-3.2-3b"}},
		Description: "Venice AI (private, uncensored inference — OpenAI-compatible)",
		Credential:  CredentialAPIKey, APIKeyEnv: "VENICE_API_KEY",
	},
	{
		Name: "nearai", Type: TypeNearAI, ModelListing: ModelListingNearAI, Wizard: WizardSpec{Order: 13, Label: "NEAR AI Cloud - NEARAI_API_KEY", Hint: "set NEARAI_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "zai-org/GLM-5.1-FP8"}, {"fast_model", "Qwen/Qwen3.6-35B-A3B-FP8"}},
		Description: "NEAR AI Cloud (TEE-backed private inference — OpenAI-compatible)",
		Credential:  CredentialAPIKey, APIKeyEnv: "NEARAI_API_KEY",
	},
	{
		Name: "sambanova", Type: TypeSambaNova, ModelListing: ModelListingAPIKey, Wizard: WizardSpec{Order: 14, Label: "SambaNova - SAMBANOVA_API_KEY", Hint: "set SAMBANOVA_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "gpt-oss-120b"}, {"fast_model", "Meta-Llama-3.3-70B-Instruct"}},
		Description: "SambaNova Cloud (RDU-hosted inference — OpenAI-compatible)",
		Credential:  CredentialAPIKey, APIKeyEnv: "SAMBANOVA_API_KEY",
	},
	{
		Name: "obscurify", Type: TypeObscurify, ModelListing: ModelListingAPIKey, Wizard: WizardSpec{Order: 17, Label: "Obscurify.ai - OBSCURIFY_API_KEY", Hint: "set OBSCURIFY_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "z-ai/glm-5.3"}, {"fast_model", "deepseek/deepseek-v4-flash"}},
		Description: "Obscurify.ai (privacy-focused, no-email accounts — OpenAI-compatible)",
		Credential:  CredentialAPIKey, APIKeyEnv: "OBSCURIFY_API_KEY",
	},
	{
		Name: "zen", Type: TypeZen, ModelListing: ModelListingZen, Wizard: WizardSpec{Order: 15, Label: "Zen - ZEN_API_KEY", Hint: "set ZEN_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "deepseek-v4-flash"}, {"fast_model", "deepseek-v4-flash"}},
		Description: "OpenCode Zen API (paid models)",
		Credential:  CredentialAPIKey, APIKeyEnv: "ZEN_API_KEY",
	},
	{
		Name: "opencode-go", CacheOnly: true, Type: TypeOpenCodeGo, ModelListing: ModelListingOpenCodeGo, Wizard: WizardSpec{Order: 16, Label: "OpenCode Go - OPENCODE_API_KEY", Hint: "set OPENCODE_API_KEY"}, ConfigDefault: true, ShowInConfig: true, ResetTemplate: true,
		Defaults:    []DefaultField{{"model", "glm-5.2"}, {"fast_model", "deepseek-v4-flash"}},
		Description: "OpenCode Go subscription (dynamic Chat Completions, Responses, Messages, and Gemini routing)",
		Credential:  CredentialAPIKey, APIKeyEnv: "OPENCODE_API_KEY",
	},
	{
		Name: "copilot", CacheOnly: true, ModelListTimeoutSeconds: 360, CredentialLabel: "GitHub Copilot OAuth (interactive)", Aliases: []string{"github copilot"}, Type: TypeCopilot, ModelListing: ModelListingCopilot, Availability: AvailabilityCopilot,
		Defaults:    []DefaultField{{"model", "gpt-4.1"}, {"fast_model", "gpt-4.1"}},
		Description: "GitHub Copilot API (chat via OAuth; usage via GITHUB_TOKEN/GH_TOKEN)",
		Credential:  CredentialOAuth, Setup: "run term-llm auth login copilot",
	},
	{
		Name: "claude-bin", ModelListing: ModelListingClaudeBin, ModelListTimeoutSeconds: 45, FixedToolBridge: true, CredentialLabel: "claude-bin CLI (no key needed)", Aliases: []string{"claude cli"}, Type: TypeClaudeBin, Availability: AvailabilityClaude, Wizard: WizardSpec{Order: 5, Label: "Claude Code CLI - local credentials", Hint: "install Claude Code CLI"},
		Defaults:    []DefaultField{{"model", "sonnet"}, {"fast_model", "haiku"}},
		Description: "Local Claude Code credentials (claude-bin CLI)",
		Credential:  CredentialNone, Setup: "install Claude Code (claude on PATH)",
	},
	{
		Name: "grok-bin", FixedToolBridge: true, ModelListTimeoutSeconds: 45, CredentialLabel: "grok CLI login (no key needed)", Aliases: []string{"grok cli"}, Type: TypeGrokBin, ModelListing: ModelListingGrokBin, Availability: AvailabilityGrokBin, Wizard: WizardSpec{Order: 6, Label: "Grok Build CLI - grok.com OAuth", Hint: "install and log in with the Grok Build CLI"},
		Defaults:    []DefaultField{{"model", "grok-4.6"}, {"fast_model", "grok-composer-2.5-fast"}},
		Description: "Grok Build CLI via local grok.com OAuth login",
		Credential:  CredentialOAuth, Setup: "install the Grok Build CLI and log in",
	},
	{
		Name: "cursor-bin", FixedToolBridge: true, ModelListTimeoutSeconds: 45, CredentialLabel: "Cursor Agent login or CURSOR_API_KEY", Aliases: []string{"cursor cli"}, Type: TypeCursorBin, ModelListing: ModelListingCursorBin, Availability: AvailabilityCursorBin, Wizard: WizardSpec{Order: 7, Label: "Cursor Agent CLI - Cursor login", Hint: "install Cursor Agent and run 'cursor-agent login'"},
		Defaults:    []DefaultField{{"model", "auto-smart"}, {"fast_model", "composer-2.5"}},
		Description: "Cursor Agent via local Cursor login or CURSOR_API_KEY",
		Credential:  CredentialOAuth, APIKeyEnv: "CURSOR_API_KEY", Setup: "run cursor-agent login or set CURSOR_API_KEY",
	},
	{
		Name: "agy-bin", FixedToolBridge: true, ModelListTimeoutSeconds: 45, CredentialLabel: "agy Antigravity login (no key needed)", Aliases: []string{"agy cli"}, Type: TypeAgyBin, ModelListing: ModelListingAgyBin, Availability: AvailabilityAgyBin, Wizard: WizardSpec{Order: 8, Label: "Antigravity agy CLI - Google subscription", Hint: "install agy and complete its Antigravity login"},
		Defaults:    []DefaultField{{"model", "gemini-3.6-flash-high"}, {"fast_model", "gemini-3.6-flash-low"}},
		Description: "Antigravity subscription through the local agy CLI",
		Credential:  CredentialOAuth, Setup: "install agy and complete its login",
	},
	{
		Name: "ollama", Type: TypeOllama, ModelListing: ModelListingOllama,
		Defaults:    []DefaultField{{"model", "qwen2.5-coder:7b"}, {"fast_model", "qwen2.5-coder:7b"}, {"base_url", "http://127.0.0.1:11434"}},
		Description: "Ollama local inference (native /api/chat, supports think/options.*)",
		Credential:  CredentialNone, HostEnv: "OLLAMA_HOST", Setup: "set OLLAMA_HOST or add providers.ollama to config.yaml",
	},
	{
		// vLLM has no default endpoint, so it needs a providers.vllm block; its
		// optional VLLM_API_KEY resolves like any openai_compatible provider.
		Name: "vllm", Type: TypeVLLM, ModelListing: ModelListingCompatible,
		Defaults:    []DefaultField{{"fast_model", "Qwen/Qwen3.5-122B-A10B"}},
		Description: "vLLM OpenAI-compatible server with Qwen thinking controls",
		Credential:  CredentialNone, Setup: "add providers.vllm (base_url) to config.yaml",
	},
	{
		// Generic AWS variables (AWS_PROFILE, AWS_ACCESS_KEY_ID) are too common
		// to prove Bedrock is set up, so it needs a providers.bedrock block.
		Name: "bedrock", ModelListing: ModelListingCurated, Type: TypeBedrock,
		Defaults:    []DefaultField{{"fast_model", "claude-haiku-4-5"}},
		Description: "AWS Bedrock (Anthropic Claude models via AWS credentials)",
		Credential:  CredentialAWS,
	},
}

var fields = []FieldSpec{
	{Path: "type"},
	{Path: "enabled", Placeholder: true},
	{Path: "api_key", Sensitive: true},
	{Path: "management_key", Sensitive: true, Description: "OpenRouter account analytics credential; used only by usage"},
	{Path: "model"},
	{Path: "fast_model"},
	{Path: "fast_provider"},
	{Path: "service_tier"},
	{Path: "models", Placeholder: []string{}},
	{Path: "reasoning"},
	{Path: "credentials"},
	{Path: "env", Placeholder: map[string]any{}},
	{Path: "headers", Placeholder: map[string]any{}},
	{Path: "enable_hooks", Placeholder: false},
	{Path: "use_websocket", Placeholder: false},
	{Path: "use_previous_response_id", Placeholder: true, Description: "OpenAI Responses: use server-state continuation (default true)"},
	{Path: "request_encrypted_reasoning", Placeholder: true, Description: "OpenAI Responses: request encrypted reasoning for full-history replay (default true)"},
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
	{Path: "think_level", Placeholder: "low"},
	{Path: "top_k", Placeholder: 0},
	{Path: "min_p", Placeholder: 0.0},
	{Path: "presence_penalty", Placeholder: 0.0},
	{Path: "num_ctx", Placeholder: 0},
	{Path: "num_predict", Placeholder: 0},
}
