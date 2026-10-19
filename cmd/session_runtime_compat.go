package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func effectiveSessionDirectory(sess *session.Session) string {
	return runtime.EffectiveSessionDirectory(sess)
}

func resolveSessionProviderKey(cfg *config.Config, sess *session.Session) string {
	return runtime.ResolveSessionProviderKey(cfg, sess)
}

// extractModelFromProviderName parses "<Provider> (<model>[, ...])" Name() strings shared by all providers.
func extractModelFromProviderName(name string) string {
	return runtime.ExtractModelFromProviderName(name)
}
