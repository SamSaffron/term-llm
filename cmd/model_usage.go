package cmd

import (
	"context"

	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func recordStoreHelperUsage(ctx context.Context, store session.Store, sessionID, model string, kind session.ModelUsageKind, u core.Usage) {
	runtime.RecordStoreHelperUsage(ctx, store, sessionID, model, kind, u)
}
