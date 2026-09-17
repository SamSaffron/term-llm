package cmd

import (
	"os"
	"strings"
)

// liveDebugOptions keeps the environment opt-in scoped to live sessions. CLI
// debug flags remain additive; an environment value never disables them.
func (s *serveServer) liveDebugOptions() (enabled, raw bool) {
	enabled, raw = s.cfg.debug || s.cfg.debugRaw, s.cfg.debugRaw
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TERM_LLM_LIVE_DEBUG"))) {
	case "raw":
		return true, true
	case "1", "true", "on", "metadata":
		return true, raw
	default:
		// Empty, false/off, and unrecognized values do not enable sensitive logging.
		return enabled, raw
	}
}
