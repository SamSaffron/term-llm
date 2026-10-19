package runtime

import (
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerspec"
	"github.com/samsaffron/term-llm/internal/session"
)

// ActiveLLMFlags returns the provider/model pair after caller overrides.
func ActiveLLMFlags(cfg *config.Config) (string, string) {
	if cfg == nil {
		return "", ""
	}
	return strings.TrimSpace(cfg.DefaultProvider), strings.TrimSpace(bootstrap.ActiveModel(cfg))
}

// EffectiveSessionDirectory returns the durable worktree/local binding, falling
// back to the current directory for local sessions.
func EffectiveSessionDirectory(sess *session.Session) string {
	if sess != nil {
		if dir := strings.TrimSpace(sess.WorktreeDir); dir != "" {
			return dir
		}
		if sess.Origin != session.OriginWeb && sess.Origin != session.OriginTelegram {
			if dir := strings.TrimSpace(sess.CWD); dir != "" {
				return dir
			}
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

// ResolveSessionProviderKey maps durable provider metadata back to a config key.
func ResolveSessionProviderKey(cfg *config.Config, sess *session.Session) string {
	if sess == nil || cfg == nil {
		return ""
	}
	resolveKnownProvider := func(candidate string) string {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return ""
		}
		if candidate == "debug" {
			return candidate
		}
		for key := range cfg.Providers {
			if strings.EqualFold(candidate, key) {
				return key
			}
		}
		for _, builtIn := range config.GetBuiltInProviderNames() {
			if strings.EqualFold(candidate, builtIn) {
				return builtIn
			}
		}
		return ""
	}
	if known := resolveKnownProvider(sess.ProviderKey); known != "" {
		return known
	}
	display := strings.TrimSpace(sess.Provider)
	if display == "" {
		return ""
	}
	lower := strings.ToLower(display)
	for key := range cfg.Providers {
		lowerKey := strings.ToLower(key)
		if lower == lowerKey || strings.HasPrefix(lower, lowerKey+" (") {
			return key
		}
	}
	for _, builtIn := range config.GetBuiltInProviderNames() {
		lowerBuiltIn := strings.ToLower(builtIn)
		if lower == lowerBuiltIn || strings.HasPrefix(lower, lowerBuiltIn+" (") {
			return builtIn
		}
	}
	if spec, ok := providerspec.LookupSessionAlias(lower); ok {
		return spec.Name
	}
	if strings.HasPrefix(lower, "debug") {
		return "debug"
	}
	return ""
}

// ExtractModelFromProviderName parses "<Provider> (<model>[, ...])" display names.
func ExtractModelFromProviderName(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), "agy CLI") {
		return ""
	}
	open := strings.Index(name, "(")
	if open < 0 {
		return name
	}
	rest := name[open+1:]
	close := strings.Index(rest, ")")
	if close < 0 {
		return name
	}
	inner := rest[:close]
	if comma := strings.Index(inner, ","); comma >= 0 {
		inner = inner[:comma]
	}
	return strings.TrimSpace(inner)
}
