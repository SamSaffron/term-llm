package cmd

import (
	sessionsapp "github.com/samsaffron/term-llm/internal/app/sessions"
)

// Test-only aliases for export helpers that moved to internal/app/sessions.
var (
	renderSessionExport = sessionsapp.RenderExport
	writeSessionExport  = sessionsapp.WriteExport
)
