package serve

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

type telegramReloadChat struct {
	ChatID           int64
	Meta             *session.Session
	History          []llm.Message
	ActiveHistory    []llm.Message
	PromptPersisted  bool
	CarryoverContext string
	CarryoverLabel   string
	CarryoverCount   int
	LastActivity     time.Time
}
type telegramContinuation struct {
	Engine         *llm.Continuation
	MessageID      int
	Text           string
	MessageStart   int
	NeedNewMessage bool
	Images         []string
	Media          []llm.MediaArtifact
	Metrics        llm.TurnMetrics
	Turns          int
}
type telegramReloadState struct {
	Pending    map[int64]*telegramContinuation
	NextOffset int
	Chats      []telegramReloadChat
}
type telegramPollingClient struct {
	ctx    context.Context
	client tgbotapi.HTTPClient
}

func (c telegramPollingClient) Do(r *http.Request) (*http.Response, error) {
	return c.client.Do(r.Clone(c.ctx))
}

// Polling owns every accepted update until its handler, actual runner, delivery,
// and persistence descendants finish. Advancing the next offset is safe during
// normal polling because those receipts remain owned across the next request.
// Reload cancels ONLY the idle long poll and waits for all accepted receipts.
func (m *telegramSessionMgr) runPolling(ctx context.Context, bot *tgbotapi.BotAPI) error {
	var mu sync.Mutex
	state := telegramReloadState{}
	kind := fmt.Sprintf("telegram:%d", bot.Self.ID)
	if _, err := process.RestoreState(kind, &state); err != nil {
		return err
	}
	m.pendingReload = state.Pending
	m.restored = make(map[int64]telegramReloadChat)
	for _, chat := range state.Chats {
		m.restored[chat.ChatID] = chat
	}
	unregister := restart.Default.Register(&restart.Resource{Prepare: func(ctx context.Context) (func(context.Context), error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mu.Lock()
		saved := telegramReloadState{NextOffset: state.NextOffset}
		mu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		saved.Pending = m.pendingReload
		for _, chat := range m.restored {
			saved.Chats = append(saved.Chats, chat)
		}
		for id, sess := range m.sessions {
			sess.mu.Lock()
			sess.cancelMu.Lock()
			done := sess.runnerDone
			sess.cancelMu.Unlock()
			if done != nil {
				select {
				case <-done:
				default:
					sess.mu.Unlock()
					return nil, fmt.Errorf("Telegram chat %d still owns a runner", id)
				}
			}
			sess.activityMu.Lock()
			lastActivity := sess.lastActivity
			sess.activityMu.Unlock()
			saved.Chats = append(saved.Chats, telegramReloadChat{ChatID: id, Meta: sess.meta, History: sess.history, ActiveHistory: sess.activeHistory, PromptPersisted: sess.systemPromptPersisted, CarryoverContext: sess.carryoverContext, CarryoverLabel: sess.carryoverContextLabel, CarryoverCount: sess.carryoverMessageCount, LastActivity: lastActivity})
			sess.mu.Unlock()
		}
		return process.SaveState(kind, saved)
	}})
	defer unregister()
	defer m.closeAllSessions()
	if m.settings.Ready != nil {
		m.settings.Ready()
	}
	for ctx.Err() == nil {
		work, release, err := restart.Default.Root(ctx)
		if err != nil {
			if err = restart.Default.WaitReady(ctx); err != nil {
				return err
			}
			continue
		}
		if err := m.resumeTelegramContinuations(work, bot); err != nil {
			release()
			return err
		}
		pollingCtx, cancelPoll := restart.Default.AdmissionContext(ctx)
		polling := *bot
		polling.Client = telegramPollingClient{pollingCtx, bot.Client}
		mu.Lock()
		offset := state.NextOffset
		mu.Unlock()
		config := tgbotapi.NewUpdate(offset)
		config.Timeout = telegramUpdateLongPollSeconds
		updates, pollErr := polling.GetUpdates(config)
		cancelPoll()
		if pollErr == nil {
			for _, update := range updates {
				if update.UpdateID < offset {
					continue
				}
				if update.Message != nil {
					if !m.acquireMessageSlot(ctx) {
						release()
						return ctx.Err()
					}
					admission := m.admitMessage(update.Message)
					msg := update.Message
					if err := restart.Default.Go(work, func(work context.Context) {
						defer m.releaseMessageSlot()
						m.handleMessageWithAdmission(work, bot, msg, admission)
					}); err != nil {
						admission.release()
						m.releaseMessageSlot()
						release()
						return err
					}
				}
				offset = update.UpdateID + 1
				mu.Lock()
				state.NextOffset = offset
				mu.Unlock()
			}
		}
		release()
		if pollErr != nil && ctx.Err() == nil && !restart.Default.Draining() {
			// Never log Telegram transport URLs: they contain the bot token.
			log.Printf("[telegram] polling failed (%T); retrying", pollErr)
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	return ctx.Err()
}

func (m *telegramSessionMgr) resumeTelegramContinuations(ctx context.Context, bot botSender) error {
	m.mu.Lock()
	pending := make(map[int64]*telegramContinuation, len(m.pendingReload))
	for id, saved := range m.pendingReload {
		pending[id] = saved
	}
	m.mu.Unlock()
	for id, saved := range pending {
		if saved == nil || saved.Engine == nil {
			return fmt.Errorf("invalid Telegram continuation for chat %d", id)
		}
		if !m.acquireMessageSlot(ctx) {
			return ctx.Err()
		}
		// Reserve synchronously: another poll must not launch the same cursor
		// before this goroutine gets scheduled. Restore it if admission fails.
		m.mu.Lock()
		delete(m.pendingReload, id)
		m.mu.Unlock()
		if err := restart.Default.Go(ctx, func(ctx context.Context) {
			defer m.releaseMessageSlot()
			sess, err := m.getOrCreate(ctx, id)
			if err == nil {
				err = m.streamReplyContinuation(ctx, bot, sess, id, llm.Message{}, nil, saved)
			}
			if err != nil {
				log.Printf("[telegram] resume chat %d: %v", id, err)
				_, _ = bot.Send(tgbotapi.NewMessage(id, "The interrupted response could not resume after process replacement."))
			}
		}); err != nil {
			m.mu.Lock()
			m.pendingReload[id] = saved
			m.mu.Unlock()
			m.releaseMessageSlot()
			return err
		}
	}
	return nil
}
