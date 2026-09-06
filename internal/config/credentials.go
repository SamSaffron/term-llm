package config

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// getenvTrimmed reads an environment variable and trims surrounding space.
func getenvTrimmed(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// CredentialRef is a lazily resolved credential (or endpoint) drawn from the
// config file with optional environment-variable fallbacks.
//
// Nothing is resolved when the config is loaded. Vault lookups, file reads and
// `$()` subprocesses only run when a feature actually needs the value, so
// starting a chat never unlocks 1Password for image generation.
type CredentialRef struct {
	// Raw is the configured value, which may use any syntax accepted by
	// ResolveValue: op://, srv://, file://, $(...), ${VAR} or a literal.
	Raw string
	// Env lists environment variables consulted, in order, when Raw is empty
	// (or expands to nothing).
	Env []string
}

// Cred builds a CredentialRef from a config value and its env fallbacks.
func Cred(raw string, env ...string) CredentialRef {
	return CredentialRef{Raw: strings.TrimSpace(raw), Env: env}
}

// IsDeferred reports whether resolving this credential requires an expensive
// side effect (1Password, file read, subprocess, DNS).
func (r CredentialRef) IsDeferred() bool {
	return needsLazyResolve(r.Raw)
}

// Configured reports whether a value is available without resolving it. It
// never touches a vault, a file, DNS or a subprocess, so it is safe to call on
// startup and in "is this feature available?" checks.
func (r CredentialRef) Configured() bool {
	if r.Raw != "" {
		if name := envRefName(r.Raw); name != "" {
			return getenvTrimmed(name) != ""
		}
		return true
	}
	return r.envFallback() != ""
}

// Resolve returns the credential value, resolving deferred syntax on demand.
// Successful deferred resolutions are memoized for the life of the process so
// repeated use costs at most one vault lookup.
func (r CredentialRef) Resolve() (string, error) {
	if r.Raw != "" {
		if r.IsDeferred() {
			value, err := resolveCachedValue(r.Raw)
			if err != nil {
				return "", err
			}
			if value != "" {
				return value, nil
			}
		} else if value := strings.TrimSpace(expandEnv(r.Raw)); value != "" {
			return value, nil
		}
	}
	return r.envFallback(), nil
}

// ResolveOr returns the resolved credential, or empty string if resolution
// fails. Use it only where an error cannot be surfaced usefully.
func (r CredentialRef) ResolveOr() string {
	value, err := r.Resolve()
	if err != nil {
		return ""
	}
	return value
}

func (r CredentialRef) envFallback() string {
	for _, name := range r.Env {
		if value := getenvTrimmed(name); value != "" {
			return value
		}
	}
	return ""
}

// ResolveFirstCredential resolves the first configured reference, preserving
// precedence between cascading settings (for example audio.venice.api_key
// before image.venice.api_key). It returns an empty string when nothing is
// configured.
func ResolveFirstCredential(refs ...CredentialRef) (string, error) {
	for _, ref := range refs {
		if !ref.Configured() {
			continue
		}
		value, err := ref.Resolve()
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
	}
	return "", nil
}

// AnyCredentialConfigured reports whether any reference has a value available
// without resolving any of them.
func AnyCredentialConfigured(refs ...CredentialRef) bool {
	for _, ref := range refs {
		if ref.Configured() {
			return true
		}
	}
	return false
}

// envRefName returns the variable name when value is a bare ${VAR} or $VAR
// reference, otherwise "".
func envRefName(value string) string {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return value[2 : len(value)-1]
	}
	if strings.HasPrefix(value, "$") && !strings.HasPrefix(value, "$(") {
		return value[1:]
	}
	return ""
}

var (
	credentialCacheMu sync.Mutex
	credentialCache   = map[string]string{}
)

// resolveCachedValue resolves a deferred value once per process. Failures are
// not cached so a locked vault can be retried after signing in.
func resolveCachedValue(raw string) (string, error) {
	credentialCacheMu.Lock()
	cached, ok := credentialCache[raw]
	credentialCacheMu.Unlock()
	if ok {
		return cached, nil
	}

	value, err := ResolveValue(raw)
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)

	credentialCacheMu.Lock()
	credentialCache[raw] = value
	credentialCacheMu.Unlock()
	return value, nil
}

// ResetCredentialCache clears memoized deferred credentials. Intended for
// tests and for config reloads that must pick up rotated secrets.
func ResetCredentialCache() {
	credentialCacheMu.Lock()
	credentialCache = map[string]string{}
	credentialCacheMu.Unlock()
}

// --- Image ---------------------------------------------------------------

// GeminiKey returns the Gemini image credential.
func (c ImageConfig) GeminiKey() CredentialRef {
	return Cred(c.Gemini.APIKey, "GEMINI_API_KEY")
}

// OpenAIKey returns the OpenAI image credential.
func (c ImageConfig) OpenAIKey() CredentialRef {
	return Cred(c.OpenAI.APIKey, "OPENAI_API_KEY")
}

// XAIKey returns the xAI image credential.
func (c ImageConfig) XAIKey() CredentialRef {
	return Cred(c.XAI.APIKey, "XAI_API_KEY")
}

// VeniceKey returns the Venice image credential.
func (c ImageConfig) VeniceKey() CredentialRef {
	return Cred(c.Venice.APIKey, "VENICE_API_KEY")
}

// FluxKey returns the Flux (BFL) image credential.
func (c ImageConfig) FluxKey() CredentialRef {
	return Cred(c.Flux.APIKey, "BFL_API_KEY")
}

// OpenRouterKey returns the OpenRouter image credential.
func (c ImageConfig) OpenRouterKey() CredentialRef {
	return Cred(c.OpenRouter.APIKey, "OPENROUTER_API_KEY")
}

// --- Audio ---------------------------------------------------------------

// VeniceKey returns the Venice speech credential.
func (c AudioConfig) VeniceKey() CredentialRef {
	return Cred(c.Venice.APIKey, "VENICE_API_KEY")
}

// GeminiKey returns the Gemini speech credential.
func (c AudioConfig) GeminiKey() CredentialRef {
	return Cred(c.Gemini.APIKey, "GEMINI_API_KEY")
}

// ElevenLabsKey returns the ElevenLabs speech credential.
func (c AudioConfig) ElevenLabsKey() CredentialRef {
	return Cred(c.ElevenLabs.APIKey, "ELEVENLABS_API_KEY", "XI_API_KEY")
}

// --- Music ---------------------------------------------------------------

// VeniceKey returns the Venice music credential.
func (c MusicConfig) VeniceKey() CredentialRef {
	return Cred(c.Venice.APIKey, "VENICE_API_KEY")
}

// ElevenLabsKey returns the ElevenLabs music credential.
func (c MusicConfig) ElevenLabsKey() CredentialRef {
	return Cred(c.ElevenLabs.APIKey, "ELEVENLABS_API_KEY", "XI_API_KEY")
}

// --- Transcription -------------------------------------------------------

// VeniceKey returns the Venice transcription credential.
func (c TranscriptionConfig) VeniceKey() CredentialRef {
	return Cred(c.Venice.APIKey, "VENICE_API_KEY")
}

// ElevenLabsKey returns the ElevenLabs transcription credential.
func (c TranscriptionConfig) ElevenLabsKey() CredentialRef {
	return Cred(c.ElevenLabs.APIKey, "ELEVENLABS_API_KEY", "XI_API_KEY")
}

// --- Embeddings ----------------------------------------------------------

// OpenAIKey returns the OpenAI embedding credential.
func (c EmbedConfig) OpenAIKey() CredentialRef {
	return Cred(c.OpenAI.APIKey, "OPENAI_API_KEY")
}

// GeminiKey returns the Gemini embedding credential.
func (c EmbedConfig) GeminiKey() CredentialRef {
	return Cred(c.Gemini.APIKey, "GEMINI_API_KEY")
}

// JinaKey returns the Jina embedding credential.
func (c EmbedConfig) JinaKey() CredentialRef {
	return Cred(c.Jina.APIKey, "JINA_API_KEY")
}

// VoyageKey returns the Voyage embedding credential.
func (c EmbedConfig) VoyageKey() CredentialRef {
	return Cred(c.Voyage.APIKey, "VOYAGE_API_KEY")
}

// OllamaBaseURLRef returns the Ollama embedding endpoint, which supports the
// same deferred syntax (including srv://) as provider endpoints.
func (c EmbedConfig) OllamaBaseURLRef() CredentialRef {
	return Cred(c.Ollama.BaseURL)
}

// --- Search --------------------------------------------------------------

// ExaKey returns the Exa search credential.
func (c SearchConfig) ExaKey() CredentialRef {
	return Cred(c.Exa.APIKey, "EXA_API_KEY")
}

// ExaMCPURLRef returns the Exa MCP endpoint.
func (c SearchConfig) ExaMCPURLRef() CredentialRef {
	return Cred(c.ExaMCP.URL)
}

// ExaMCPKey returns the Exa MCP credential. The EXA_API_KEY fallback only
// applies to the hosted default endpoint; a custom endpoint must configure its
// own key.
func (c SearchConfig) ExaMCPKey() CredentialRef {
	url := strings.TrimSpace(c.ExaMCP.URL)
	if url == "" || url == DefaultSearchExaMCPURL {
		return Cred(c.ExaMCP.APIKey, "EXA_API_KEY")
	}
	return Cred(c.ExaMCP.APIKey)
}

// PerplexityKey returns the Perplexity search credential.
func (c SearchConfig) PerplexityKey() CredentialRef {
	return Cred(c.Perplexity.APIKey, "PERPLEXITY_API_KEY")
}

// ParallelKey returns the Parallel search credential.
func (c SearchConfig) ParallelKey() CredentialRef {
	return Cred(c.Parallel.APIKey, "PARALLEL_API_KEY")
}

// TavilyKey returns the Tavily search credential.
func (c SearchConfig) TavilyKey() CredentialRef {
	return Cred(c.Tavily.APIKey, "TAVILY_API_KEY")
}

// BraveKey returns the Brave search credential.
func (c SearchConfig) BraveKey() CredentialRef {
	return Cred(c.Brave.APIKey, "BRAVE_API_KEY")
}

// GoogleKey returns the Google Custom Search credential.
func (c SearchConfig) GoogleKey() CredentialRef {
	return Cred(c.Google.APIKey, "GOOGLE_SEARCH_API_KEY")
}

// GoogleCXRef returns the Google Custom Search engine ID.
func (c SearchConfig) GoogleCXRef() CredentialRef {
	return Cred(c.Google.CX, "GOOGLE_SEARCH_CX")
}

// --- Serve ---------------------------------------------------------------

// TokenRef returns the Telegram bot token.
func (c TelegramServeConfig) TokenRef() CredentialRef {
	return Cred(c.Token)
}

// PublicKeyRef returns the VAPID public key.
func (c WebPushConfig) PublicKeyRef() CredentialRef {
	return Cred(c.VAPIDPublicKey)
}

// PrivateKeyRef returns the VAPID private key.
func (c WebPushConfig) PrivateKeyRef() CredentialRef {
	return Cred(c.VAPIDPrivateKey)
}

// ResolveDeferred resolves only explicitly deferred syntax: op://, srv://,
// file://, $(...) and ${VAR}. Any other value is returned unchanged.
//
// Use it for free-form maps such as MCP server headers and environment
// entries, where a literal value may legitimately begin with '$'.
func ResolveDeferred(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, nil
	}
	if needsLazyResolve(trimmed) {
		return resolveCachedValue(trimmed)
	}
	if name := envRefName(trimmed); name != "" && strings.HasPrefix(trimmed, "${") {
		return getenvTrimmed(name), nil
	}
	return value, nil
}

// ResolveDeferredMap returns a copy of m with deferred values resolved. It
// returns m unchanged when nothing needs resolution.
func ResolveDeferredMap(m map[string]string) (map[string]string, error) {
	if len(m) == 0 {
		return m, nil
	}
	var out map[string]string
	for k, v := range m {
		resolved, err := ResolveDeferred(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		if resolved == v {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(m))
			for ck, cv := range m {
				out[ck] = cv
			}
		}
		out[k] = resolved
	}
	if out == nil {
		return m, nil
	}
	return out, nil
}
