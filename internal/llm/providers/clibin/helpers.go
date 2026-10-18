package clibin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

// Optional provider interfaces the CLI-bin providers implement. They mirror
// the engine-facing interfaces in the root llm package, which cannot be
// imported from here.
type toolExecutorSetter interface {
	SetToolExecutor(func(ctx context.Context, name string, args json.RawMessage) (core.ToolOutput, error))
}

type providerCleaner interface {
	CleanupMCP()
}

type providerTurnCleaner interface {
	CleanupTurn()
}

// ToolInfoFormatter renders a short preview of a tool call's arguments for UI
// display. The root llm package installs its implementation at init time
// because the formatter lives with the engine.
var ToolInfoFormatter = func(core.ToolCall) string { return "" }

const (
	// serviceTierFast is Codex/ChatGPT's API value for user-facing "fast" mode.
	serviceTierFast = "priority"
	// serviceTierFastAlias is the legacy/user-facing alias accepted in config and requests.
	serviceTierFastAlias = "fast"
)

// normalizeServiceTier maps user-facing aliases to API values. Unknown values
// are returned trimmed.
func normalizeServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case serviceTierFastAlias, serviceTierFast:
		return serviceTierFast
	default:
		return strings.TrimSpace(value)
	}
}

// eventSender provides safe event sending for stream producers. Zero value is
// a no-op sender.
type eventSender struct {
	ctx context.Context
	ch  chan<- core.Event
}

func (s eventSender) inner() stream.Sender { return stream.NewSender(s.ctx, s.ch) }

// Send sends an event, blocking until delivered, context cancelled, or closed.
func (s eventSender) Send(event core.Event) error { return s.inner().Send(event) }

// TrySend attempts a non-blocking send and reports whether it was delivered.
func (s eventSender) TrySend(event core.Event) bool { return s.inner().TrySend(event) }

var errEventStreamClosed = stream.ErrClosed

func newEventStream(ctx context.Context, run func(context.Context, eventSender) error) core.Stream {
	return stream.New(ctx, func(ctx context.Context, s stream.Sender) error {
		return run(ctx, eventSender{ctx: s.Context(), ch: s.Chan()})
	})
}

func safeSendEvent(ctx context.Context, ch chan<- core.Event, event core.Event) bool {
	return stream.SafeSend(ctx, ch, event)
}

var knownEffortSuffixes = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// trimKnownEffortSuffix removes a trailing reasoning-effort suffix from model.
func trimKnownEffortSuffix(model string) (string, bool) {
	for _, suffix := range knownEffortSuffixes {
		if strings.HasSuffix(model, "-"+suffix) {
			return strings.TrimSuffix(model, "-"+suffix), true
		}
	}
	return model, false
}

// grokBinEffortVariants are the effort suffixes advertised for Grok CLI models.
var grokBinEffortVariants = []string{"low", "medium", "high", "xhigh"}

func cloneEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	return append([]string(nil), efforts...)
}

func readHydratableImagePath(path string) ([]byte, bool) {
	path = strings.TrimSpace(path)
	if path == "" || !isTermLLMUploadPath(path) {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

func isTermLLMUploadPath(path string) bool {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return false
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	uploadsDir, err = filepath.EvalSymlinks(uploadsDir)
	if err != nil {
		return false
	}
	path, err = filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	if path == uploadsDir {
		return false
	}
	return strings.HasPrefix(path, uploadsDir+string(filepath.Separator))
}

func modelInfosFromCache(cached *cache.ModelCache) []core.ModelInfo {
	if cached == nil {
		return nil
	}
	if len(cached.ModelInfos) > 0 {
		models := make([]core.ModelInfo, 0, len(cached.ModelInfos))
		for _, m := range cached.ModelInfos {
			if m.ID == "" {
				continue
			}
			models = append(models, core.ModelInfo{
				ID:                m.ID,
				DisplayName:       m.DisplayName,
				Created:           m.Created,
				OwnedBy:           m.OwnedBy,
				InputLimit:        m.InputLimit,
				OutputLimit:       m.OutputLimit,
				ConfiguredContext: m.ConfiguredContext,
				InputPrice:        m.InputPrice,
				OutputPrice:       m.OutputPrice,
				ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
				ReasoningModes:    cloneEfforts(m.ReasoningModes),
			})
		}
		return models
	}
	models := make([]core.ModelInfo, 0, len(cached.Models))
	for _, id := range cached.Models {
		if id != "" {
			models = append(models, core.ModelInfo{ID: id})
		}
	}
	return models
}

func modelInfosToCache(models []core.ModelInfo) []cache.CachedModel {
	cached := make([]cache.CachedModel, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		cached = append(cached, cache.CachedModel{
			ID:                m.ID,
			DisplayName:       m.DisplayName,
			Created:           m.Created,
			OwnedBy:           m.OwnedBy,
			InputLimit:        m.InputLimit,
			OutputLimit:       m.OutputLimit,
			ConfiguredContext: m.ConfiguredContext,
			InputPrice:        m.InputPrice,
			OutputPrice:       m.OutputPrice,
			ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
			ReasoningModes:    cloneEfforts(m.ReasoningModes),
		})
	}
	return cached
}

// maxTurnsExceededWarning mirrors engine.MaxTurnsExceededWarning(0); the engine
// package is not imported here to keep provider families independent of it.
const maxTurnsExceededWarning = core.WarningPhasePrefix + "agent is out of turns. Send a follow-up with updated instructions or increase max_turns."
