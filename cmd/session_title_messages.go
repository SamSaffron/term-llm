package cmd

import (
	"context"

	sessionsapp "github.com/samsaffron/term-llm/internal/app/sessions"
	"github.com/samsaffron/term-llm/internal/session"
)

// loadSessionTitleMessages is shared by `sessions autotitle` and serve.
func loadSessionTitleMessages(ctx context.Context, store session.Store, sessionID string) ([]session.Message, error) {
	return sessionsapp.LoadTitleMessages(ctx, store, sessionID)
}

const sessionTitleMessagePageSize = sessionsapp.TitleMessagePageSize
