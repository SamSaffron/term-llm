package serve

import (
	"context"
	"errors"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// telegramReceiver separates a restart receive pause from platform shutdown.
// Only the polling cycle mutates state; pause joins that cycle before reading it.
type telegramReceiver struct {
	mu              sync.Mutex
	state           telegramPollState
	running, paused bool
	wake            chan struct{}
	cancel          context.CancelFunc
	done            chan struct{}
}

func (r *telegramReceiver) run(ctx context.Context, bot *tgbotapi.BotAPI, accept func(context.Context, tgbotapi.Update) bool) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("Telegram receiver already running")
	}
	r.running = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.running = false; r.mu.Unlock() }()
	for {
		r.mu.Lock()
		if ctx.Err() != nil {
			r.mu.Unlock()
			return nil
		}
		if r.paused {
			wake := r.wake
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
			}
			continue
		}
		pollCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		r.cancel = cancel
		r.done = done
		r.mu.Unlock()
		err := runTelegramPolling(pollCtx, bot, &r.state, func(update tgbotapi.Update) bool { return accept(pollCtx, update) })
		interrupted := pollCtx.Err() != nil
		cancel()
		r.mu.Lock()
		r.cancel = nil
		r.done = nil
		paused := r.paused
		close(done)
		r.mu.Unlock()
		if err != nil || (!paused && !interrupted) {
			return err
		}
	}
}

func (r *telegramReceiver) pause(ctx context.Context) (int, error) {
	r.mu.Lock()
	if !r.paused {
		r.paused = true
		r.wake = make(chan struct{})
	}
	if r.cancel != nil {
		r.cancel()
	}
	done := r.done
	r.mu.Unlock()
	if done != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-done:
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused || r.done != nil {
		return 0, errors.New("Telegram receiving resumed before pause settled")
	}
	return r.state.NextOffset, nil
}

func (r *telegramReceiver) resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		r.paused = false
		close(r.wake)
		r.wake = nil
	}
}

func (r *telegramReceiver) restore(offset int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running || offset < 0 {
		return errors.New("Telegram receive offset can only be restored before startup")
	}
	r.state.NextOffset = offset
	return nil
}

// PauseReceiving stops and joins polling/admission without cancelling message
// handlers or closing session runtimes. Those retain their natural grace period.
func (p *TelegramPlatform) PauseReceiving(ctx context.Context) (int, error) {
	return p.receiver.pause(ctx)
}

// ResumeReceiving rolls back a receive pause after a failed process replacement.
func (p *TelegramPlatform) ResumeReceiving() { p.receiver.resume() }

// RestoreReceivingOffset installs an authenticated handoff's acknowledgement
// boundary before Run. It is not sufficient to restore in-flight conversations.
func (p *TelegramPlatform) RestoreReceivingOffset(offset int) error {
	return p.receiver.restore(offset)
}
