package serve

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// telegramReplyFinalizer borrows one settled stream's transcript and
// presentation state. It does not own stream cancellation, callback teardown,
// the session lock, or goroutine cleanup.
type telegramReplyFinalizer struct {
	manager                        *telegramSessionMgr
	bot                            botSender
	sess                           *telegramSession
	chatID                         int64
	user                           llm.Message
	resume                         *telegramContinuation
	events                         *telegramEventAccumulator
	presentation                   *telegramPresentation
	producedMu                     *sync.Mutex
	produced                       *[]llm.Message
	metrics                        *llm.TurnMetrics
	turnCount                      *int
	callbackStoreQueue             *telegramStoreOpQueue
	drainCallbackStoreQueue        func() bool
	updateActiveHistory            func(string)
	salvagePartialHistory          func(context.Context, string)
	stopStreamWithCleanupTimeout   func() bool
	turnPersistenceDegraded        *bool
	includeSystemPromptOnReconcile bool
}

func (f *telegramReplyFinalizer) finishSuspension(ctx context.Context, suspended *llm.SuspendedError) error {
	if !f.stopStreamWithCleanupTimeout() || !f.drainCallbackStoreQueue() {
		return fmt.Errorf("Telegram continuation cleanup did not settle")
	}
	history := suspended.Continuation.Request.Messages
	f.updateActiveHistory("")
	if f.sess.activeHistory != nil {
		history = append([]llm.Message{}, f.sess.history...)
		if f.resume == nil {
			history = append(history, normalizeUserMessageForHistory(f.user))
		}
		f.producedMu.Lock()
		history = append(history, (*f.produced)...)
		f.producedMu.Unlock()
	}
	if f.manager.store != nil && f.sess.meta != nil {
		if !f.manager.reconcileTelegramTranscript(context.WithoutCancel(ctx), f.sess, history, true, "ReplaceMessages(reload_boundary)") {
			return fmt.Errorf("persist Telegram continuation boundary")
		}
	}
	f.sess.history = history
	snapshot := f.events.Snapshot()
	messageID, messageStart, needNewMessage := f.presentation.checkpoint()
	saved := &telegramContinuation{Engine: suspended.Continuation, MessageID: messageID, Text: snapshot.Text, MessageStart: messageStart, NeedNewMessage: needNewMessage, Images: snapshot.Images, Media: snapshot.Media, Metrics: *f.metrics, Turns: *f.turnCount}
	if suspended.Continuation.DiscardPartial {
		saved.Text = suspended.Continuation.CommittedText()
		saved.MessageStart = min(saved.MessageStart, len(saved.Text))
	}
	f.manager.mu.Lock()
	if f.manager.pendingReload == nil {
		f.manager.pendingReload = make(map[int64]*telegramContinuation)
	}
	f.manager.pendingReload[f.chatID] = saved
	f.manager.mu.Unlock()
	return nil
}

func (f *telegramReplyFinalizer) finishInterrupt(streamCtx context.Context) error {
	drained := f.stopStreamWithCleanupTimeout()
	partial := f.events.Text()
	f.presentation.renderInterrupted(partial)
	if drained {
		f.salvagePartialHistory(streamCtx, "AddMessage(assistant_interrupt_fallback)")
	}
	if f.manager.store != nil && f.sess.meta != nil {
		f.manager.runStoreOpWithTimeout(f.sess.meta.ID, "UpdateStatus(interrupted)", func(storeCtx context.Context) error {
			return f.manager.store.UpdateStatus(storeCtx, f.sess.meta.ID, session.StatusInterrupted)
		})
	}
	return nil
}

func (f *telegramReplyFinalizer) finishStreamError(streamCtx context.Context, streamErr error) error {
	drained := f.stopStreamWithCleanupTimeout()
	if drained {
		f.salvagePartialHistory(streamCtx, "AddMessage(assistant_error_fallback)")
	}
	if strings.Contains(streamErr.Error(), "stream timed out") {
		_, _ = f.bot.Send(tgbotapi.NewMessage(f.chatID, "⌛ Response timed out — please try again."))
	}
	if f.manager.store != nil && f.sess.meta != nil {
		f.manager.runStoreOpWithTimeout(f.sess.meta.ID, "UpdateStatus(stream_error)", func(storeCtx context.Context) error {
			return f.manager.store.UpdateStatus(storeCtx, f.sess.meta.ID, session.StatusError)
		})
	}
	return streamErr
}

func (f *telegramReplyFinalizer) finishSuccess(ctx context.Context) error {
	snapshot := f.events.Snapshot()
	full, ran := snapshot.Text, snapshot.ToolsRan
	mediaToSend := referencedTelegramMedia(full, snapshot.Media)
	finalDeliveryCtx, cancel := context.WithTimeout(ctx, telegramFinalDeliveryTimeout)
	defer cancel()
	finalDeliveryErr := f.presentation.finalizeText(finalDeliveryCtx, full, ran)
	if full == "" && (f.manager.settings.Debug || f.manager.settings.DebugRaw) {
		log.Printf("[telegram] empty assistant text for chat %d (toolsRan=%v, text_delta=%d, reasoning_delta=%d, tool_start=%d, tool_end=%d, tool_call=%d, phase=%d, usage=%d, done=%d, retry=%d, error=%d, other=%d, other_types=%v)", f.chatID, ran, snapshot.TextDeltas, snapshot.ReasoningDeltas, snapshot.ToolStarts, snapshot.ToolEnds, snapshot.ToolCalls, snapshot.PhaseEvents, snapshot.UsageEvents, snapshot.DoneEvents, snapshot.RetryEvents, snapshot.ErrorEvents, snapshot.OtherEvents, snapshot.OtherTypes)
	}
	f.presentation.deliverMedia(snapshot.Images, mediaToSend)

	newHistory := make([]llm.Message, 0, len(f.sess.history)+2+len(*f.produced))
	newHistory = append(newHistory, f.sess.history...)
	if f.resume == nil {
		newHistory = append(newHistory, normalizeUserMessageForHistory(f.user))
	}
	f.producedMu.Lock()
	newHistory = append(newHistory, (*f.produced)...)
	f.producedMu.Unlock()
	if len(*f.produced) == 0 && full != "" {
		if f.manager.store != nil && f.sess.meta != nil {
			assistant := session.NewMessage(f.sess.meta.ID, llm.AssistantText(full), -1)
			if !f.manager.runStoreOp(ctx, f.sess.meta.ID, "AddMessage(assistant_fallback)", func(storeCtx context.Context) error {
				return f.manager.store.AddMessage(storeCtx, f.sess.meta.ID, assistant)
			}) {
				*f.turnPersistenceDegraded = true
			}
		}
		newHistory = append(newHistory, llm.AssistantText(full))
	}
	activeFallback := ""
	if len(*f.produced) == 0 {
		activeFallback = full
	}
	f.updateActiveHistory(activeFallback)
	f.sess.history = newHistory
	f.sess.activityMu.Lock()
	f.sess.lastActivity = time.Now()
	f.sess.activityMu.Unlock()
	if f.callbackStoreQueue != nil {
		queueDrained := f.drainCallbackStoreQueue()
		if queueDrained && (*f.turnPersistenceDegraded || f.callbackStoreQueue.isDegraded()) {
			f.manager.reconcileTelegramTranscript(ctx, f.sess, newHistory, f.includeSystemPromptOnReconcile, "ReplaceMessages(callback_reconcile)")
		}
	}
	f.persistMetrics(ctx)
	if finalDeliveryErr != nil {
		return fmt.Errorf("deliver complete Telegram response: %w", finalDeliveryErr)
	}
	return nil
}

func (f *telegramReplyFinalizer) persistMetrics(ctx context.Context) {
	if f.manager.store != nil && f.sess.meta != nil {
		f.producedMu.Lock()
		metrics, turns := *f.metrics, *f.turnCount
		f.producedMu.Unlock()
		if turns > 0 || metrics.ToolCalls != 0 || metrics.InputTokens != 0 || metrics.OutputTokens != 0 || metrics.CachedInputTokens != 0 || metrics.CacheWriteTokens != 0 {
			f.manager.runStoreOpWithoutCancel(ctx, f.sess.meta.ID, "UpdateMetrics", func(storeCtx context.Context) error {
				return f.manager.store.UpdateMetrics(storeCtx, f.sess.meta.ID, turns, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens, metrics.CacheWriteTokens)
			})
		}
		if total, count := f.sess.runtime.Engine.ContextEstimateBaseline(); total > 0 {
			f.sess.meta.LastTotalTokens = total
			f.sess.meta.LastMessageCount = count
			f.manager.runStoreOpWithoutCancel(ctx, f.sess.meta.ID, "UpdateContextEstimate", func(storeCtx context.Context) error {
				return f.manager.store.UpdateContextEstimate(storeCtx, f.sess.meta.ID, total, count)
			})
		}
	}
	if f.manager.store != nil && f.sess.meta != nil {
		f.manager.runStoreOp(ctx, f.sess.meta.ID, "UpdateStatus(active_end)", func(storeCtx context.Context) error {
			return f.manager.store.UpdateStatus(storeCtx, f.sess.meta.ID, session.StatusActive)
		})
		f.manager.runStoreOp(ctx, f.sess.meta.ID, "SetCurrent(end)", func(storeCtx context.Context) error {
			return f.manager.store.SetCurrent(storeCtx, f.sess.meta.ID)
		})
	}
}
