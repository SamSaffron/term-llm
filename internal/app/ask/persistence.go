package ask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminaltext"
	"github.com/samsaffron/term-llm/internal/tools"
)

func initializeAskPersistence(ctx context.Context, cfg *config.Config, store session.Store, sess *session.Session, instructions string, historyHasSystem, resuming bool, messages []core.Message, conversationStartedAt time.Time, userMessage core.Message, question string) *AssistantPersistence {
	if store == nil || sess == nil {
		return nil
	}
	reasoningCfg := config.DefaultReasoningConfig()
	if cfg != nil {
		reasoningCfg = cfg.ResolveReasoning("ask")
	}
	persistence := &AssistantPersistence{store: store, sess: sess, reasoningCfg: reasoningCfg}
	if instructions != "" && !historyHasSystem {
		message := &session.Message{SessionID: sess.ID, Role: core.RoleSystem, Parts: []core.Part{{Type: core.PartText, Text: instructions}}, TextContent: instructions, CreatedAt: time.Now(), Sequence: -1}
		_ = store.AddMessage(ctx, sess.ID, message)
	}
	if !resuming {
		if start, ok := llm.ConversationStartFrom(messages); ok {
			persisted := session.NewMessage(sess.ID, start, -1)
			persisted.CreatedAt = conversationStartedAt
			_ = store.AddMessage(ctx, sess.ID, persisted)
		}
	}
	user := session.NewMessage(sess.ID, userMessage, -1)
	if err := store.AddMessage(ctx, sess.ID, user); err == nil {
		persistence.InitialUserPersisted = true
	}
	_ = store.IncrementUserTurns(ctx, sess.ID)
	sess.UserTurns++
	if sess.Summary == "" {
		sess.Summary = session.TruncateSummary(askInputSummary(question, userMessage))
		_ = store.Update(ctx, sess)
	}
	return persistence
}

func newAskResponseCompletedCallback(persistence *AssistantPersistence, turnStart *time.Time) engine.ResponseCompletedCallback {
	if persistence == nil {
		return nil
	}
	return func(ctx context.Context, _ int, message core.Message, _ core.TurnMetrics) error {
		return persistence.persist(ctx, message, time.Since(*turnStart).Milliseconds(), true)
	}
}

func newAskTurnCompletedCallback(persistence *AssistantPersistence, store session.Store, sess **session.Session, engine *engine.Engine, turnStart *time.Time) engine.TurnCompletedCallback {
	if persistence == nil {
		return nil
	}
	return func(ctx context.Context, _ int, messages []core.Message, metrics core.TurnMetrics) error {
		appendStart := 0
		if len(messages) > 0 && messages[0].Role == core.RoleAssistant {
			persistence.mu.Lock()
			finalizeText := !persistence.pendingTextSet
			persistence.mu.Unlock()
			if finalizeText {
				if err := persistence.persist(ctx, messages[0], time.Since(*turnStart).Milliseconds(), true); err != nil {
					return err
				}
			}
			appendStart = 1
		}
		current := *sess
		for _, message := range messages[appendStart:] {
			stored := session.NewMessageWithReasoningPolicy(current.ID, message, -1, persistence.reasoningCfg)
			if message.Role == core.RoleAssistant {
				stored.DurationMs = time.Since(*turnStart).Milliseconds()
			}
			_ = store.AddMessage(ctx, current.ID, stored)
		}
		persistence.reset()
		runtime.RecordStoreTurnUsage(ctx, store, current.ID, current.Model, session.ModelUsageMain, core.Usage{
			InputTokens: metrics.InputTokens, OutputTokens: metrics.OutputTokens,
			CachedInputTokens: metrics.CachedInputTokens, CacheWriteTokens: metrics.CacheWriteTokens,
		}, metrics.ToolCalls)
		if total, count := engine.ContextEstimateBaseline(); total > 0 {
			_ = store.UpdateContextEstimate(ctx, current.ID, total, count)
			current.LastTotalTokens, current.LastMessageCount = total, count
		}
		return nil
	}
}

func newAskSyntheticUserCallback(store session.Store, sess **session.Session, turnStart *time.Time) func(context.Context, core.Message) error {
	if store == nil || sess == nil || *sess == nil {
		return nil
	}
	return func(ctx context.Context, message core.Message) error {
		*turnStart = time.Now()
		current := *sess
		return store.AddMessage(ctx, current.ID, session.NewMessage(current.ID, message, -1))
	}
}

func wrapAskOutputCallbacks(response engine.ResponseCompletedCallback, turn engine.TurnCompletedCallback, messages *[]core.Message, mu *sync.Mutex) (engine.ResponseCompletedCallback, engine.TurnCompletedCallback) {
	wrappedResponse := func(ctx context.Context, turnIndex int, message core.Message, metrics core.TurnMetrics) error {
		if response != nil {
			if err := response(ctx, turnIndex, message, metrics); err != nil {
				return err
			}
		}
		mu.Lock()
		*messages = append(*messages, message)
		mu.Unlock()
		return nil
	}
	wrappedTurn := func(ctx context.Context, turnIndex int, turnMessages []core.Message, metrics core.TurnMetrics) error {
		mu.Lock()
		*messages = append(*messages, turnMessages...)
		mu.Unlock()
		if turn != nil {
			return turn(ctx, turnIndex, turnMessages, metrics)
		}
		return nil
	}
	return wrappedResponse, wrappedTurn
}

func newAskAssistantSnapshotCallback(persistence *AssistantPersistence, turnStart *time.Time) engine.AssistantSnapshotCallback {
	if persistence == nil {
		return nil
	}
	return func(ctx context.Context, _ int, message core.Message) error {
		return persistence.persist(ctx, message, time.Since(*turnStart).Milliseconds(), false)
	}
}

func newAskCompactionCallback(store session.Store, sess **session.Session, hooks CompactionHooks) engine.CompactionCallback {
	return func(ctx context.Context, result *engine.CompactionResult) error {
		if hooks.Record != nil {
			hooks.Record(result)
		}
		if store == nil || *sess == nil {
			return nil
		}
		_, _, refreshed, err := session.ApplyCompaction(ctx, store, *sess, nil, result)
		if err != nil {
			return err
		}
		if refreshed != nil {
			*sess = refreshed
		}
		if result != nil && !result.Usage.BillableCountersZero() {
			current := *sess
			runtime.RecordStoreHelperUsage(ctx, store, current.ID, result.Model, session.ModelUsageCompaction, result.Usage)
		}
		return nil
	}
}

func (p *PersistenceCallbacks) WrapOutputTool(enabled bool, messages *[]core.Message, mu *sync.Mutex) {
	if enabled {
		p.ResponseCompleted, p.TurnCompleted = wrapAskOutputCallbacks(p.ResponseCompleted, p.TurnCompleted, messages, mu)
	}
}

func PrepareAskPersistenceCallbacks(ctx context.Context, cfg *config.Config, store session.Store, sess **session.Session, engine *engine.Engine, turnStart *time.Time, instructions string, historyHasSystem, resuming bool, messages []core.Message, conversationStartedAt time.Time, userMessage core.Message, question string, compactionHooks CompactionHooks) PersistenceCallbacks {
	persistence := initializeAskPersistence(ctx, cfg, store, *sess, instructions, historyHasSystem, resuming, messages, conversationStartedAt, userMessage, question)
	return PersistenceCallbacks{
		Persistence:       persistence,
		ResponseCompleted: newAskResponseCompletedCallback(persistence, turnStart),
		TurnCompleted:     newAskTurnCompletedCallback(persistence, store, sess, engine, turnStart),
		SyntheticUser:     newAskSyntheticUserCallback(store, sess, turnStart),
		AssistantSnapshot: newAskAssistantSnapshotCallback(persistence, turnStart),
		Compaction:        newAskCompactionCallback(store, sess, compactionHooks),
	}
}

type AssistantPersistence struct {
	store session.Store
	// Compaction may refresh the session object, but preserves its durable ID.
	// Callbacks that update mutable session metadata follow the separate **Session.
	sess                 *session.Session
	reasoningCfg         config.ReasoningConfig
	InitialUserPersisted bool

	mu                 sync.Mutex
	pendingAssistantID int64
	pendingTextSet     bool
	lastAssistant      core.Message
	lastAssistantSet   bool
}

func (p *AssistantPersistence) persist(ctx context.Context, assistantMsg core.Message, durationMs int64, finalizeText bool) error {
	if p == nil || p.store == nil || p.sess == nil {
		return nil
	}
	sessionMsg := session.NewMessageWithReasoningPolicy(p.sess.ID, assistantMsg, -1, p.reasoningCfg)
	sessionMsg.DurationMs = durationMs

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastAssistant = assistantMsg
	p.lastAssistantSet = true
	if p.pendingAssistantID != 0 {
		sessionMsg.ID = p.pendingAssistantID
		err := session.UpdateStreamingMessage(ctx, p.store, p.sess.ID, sessionMsg, finalizeText)
		if err == nil {
			if finalizeText {
				p.pendingTextSet = true
			}
			return nil
		}
		if !errors.Is(err, session.ErrNotFound) {
			return err
		}
		p.pendingAssistantID = 0
		p.pendingTextSet = false
		sessionMsg.ID = 0
	}

	if err := p.store.AddMessage(ctx, p.sess.ID, sessionMsg); err != nil {
		return err
	}
	p.pendingAssistantID = sessionMsg.ID
	p.pendingTextSet = finalizeText
	return nil
}

func (p *AssistantPersistence) persistCollectedText(ctx context.Context, text string, durationMs int64) error {
	if p == nil || text == "" {
		return nil
	}
	return p.persist(ctx, core.AssistantText(text), durationMs, true)
}

func (p *AssistantPersistence) PersistInterrupted(ctx context.Context, collectedText string, durationMs int64) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	hasPendingSnapshot := p.pendingAssistantID != 0 && p.lastAssistantSet
	assistantMsg := p.lastAssistant
	p.mu.Unlock()
	if hasPendingSnapshot {
		return p.persist(ctx, assistantMsg, durationMs, true)
	}
	return p.persistCollectedText(ctx, collectedText, durationMs)
}

func (p *AssistantPersistence) reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.pendingAssistantID = 0
	p.pendingTextSet = false
	p.lastAssistant = core.Message{}
	p.lastAssistantSet = false
	p.mu.Unlock()
}

func persistAskAssistantResponse(ctx context.Context, store session.Store, sess *session.Session, assistantMsg core.Message, durationMs int64, reasoningPersistenceCfg config.ReasoningConfig) error {
	sessionMsg := session.NewMessageWithReasoningPolicy(sess.ID, assistantMsg, -1, reasoningPersistenceCfg)
	sessionMsg.DurationMs = durationMs
	return store.AddMessage(ctx, sess.ID, sessionMsg)
}

func writeGuardianStatus(w io.Writer, event tools.GuardianEvent) {
	if w == nil {
		return
	}
	if message := strings.TrimSpace(terminaltext.SanitizeSingleLine(event.Message)); message != "" {
		fmt.Fprintln(w, message)
	}
}

// CompactionHooks lets the command layer observe compaction results without
// the ask package depending on command internals.
type CompactionHooks struct {
	// Record receives every compaction result (for stats aggregation).
	Record func(*engine.CompactionResult)
}

type PersistenceCallbacks struct {
	Persistence       *AssistantPersistence
	ResponseCompleted engine.ResponseCompletedCallback
	TurnCompleted     engine.TurnCompletedCallback
	SyntheticUser     func(context.Context, core.Message) error
	AssistantSnapshot engine.AssistantSnapshotCallback
	Compaction        engine.CompactionCallback
}
