package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// telegramInbox tracks accepted updates until their handler actually returns.
// Releasing per-chat admission only permits steering/interrupt messages; it does
// not finish the original update. A poll offset alone loses these live updates.
// Snapshots are immutable input records, not permission to replay completed tools
// or delivered replies. Conversation handoff must supply execution/delivery state.
type telegramInbox struct {
	mu      sync.Mutex
	pending map[int]*telegramInboxEntry
	paused  bool
}

type telegramInboxEntry struct {
	raw                       json.RawMessage
	started, parked, finished bool
}

type telegramInboxContextKey struct{}
type telegramInboxReceipt struct {
	inbox *telegramInbox
	id    int
}

// Start is the side-effect boundary, after per-chat admission ordering. Paused
// receipts are retained for replacement instead of being silently acknowledged.
func (r telegramInboxReceipt) start() bool {
	r.inbox.mu.Lock()
	defer r.inbox.mu.Unlock()
	entry := r.inbox.pending[r.id]
	if entry == nil {
		return false
	}
	if entry.started {
		return true
	}
	if r.inbox.paused {
		entry.parked = true
		return false
	}
	entry.started = true
	return true
}
func startTelegramInboxReceipt(ctx context.Context) bool {
	receipt, ok := ctx.Value(telegramInboxContextKey{}).(telegramInboxReceipt)
	return !ok || receipt.start()
}

// Only stop unstarted handlers after the natural grace period. The caller must
// first pause receiving, then join existing handlers before sealing Pending.
func (i *telegramInbox) pauseUnstarted() { i.mu.Lock(); i.paused = true; i.mu.Unlock() }

// Rollback transfers only fully settled parked inputs back to the intake owner.
// The owner must admit these in order before resuming polling.
func (i *telegramInbox) resumeUnstarted() ([]json.RawMessage, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	result, err := i.parkedInputsLocked()
	if err != nil {
		return nil, err
	}
	for id := range i.pending {
		delete(i.pending, id)
	}
	i.paused = false
	return result, nil
}

func (i *telegramInbox) parkedInputs() ([]json.RawMessage, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.parkedInputsLocked()
}
func (i *telegramInbox) parkedInputsLocked() ([]json.RawMessage, error) {
	ids := make([]int, 0, len(i.pending))
	for id, entry := range i.pending {
		if !entry.parked || entry.started || !entry.finished {
			return nil, fmt.Errorf("Telegram admitted input has not settled at an unstarted boundary")
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	result := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		result = append(result, append(json.RawMessage(nil), i.pending[id].raw...))
	}
	return result, nil
}

func (i *telegramInbox) begin(update tgbotapi.Update) (finish func(), duplicate bool, err error) {
	raw, err := json.Marshal(update)
	if err != nil {
		return nil, false, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, exists := i.pending[update.UpdateID]; exists {
		return nil, true, nil
	}
	if i.pending == nil {
		i.pending = make(map[int]*telegramInboxEntry)
	}
	entry := &telegramInboxEntry{raw: raw}
	i.pending[update.UpdateID] = entry
	var once sync.Once
	return func() {
		once.Do(func() {
			i.mu.Lock()
			entry.finished = true
			if !entry.parked {
				delete(i.pending, update.UpdateID)
			}
			i.mu.Unlock()
		})
	}, false, nil
}

// snapshot orders accepted inputs by Telegram update ID, independently of
// handler scheduling. Call after receiving is paused to pair with its offset.
func (i *telegramInbox) snapshot() []json.RawMessage {
	i.mu.Lock()
	defer i.mu.Unlock()
	ids := make([]int, 0, len(i.pending))
	for id := range i.pending {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	result := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		result = append(result, append(json.RawMessage(nil), i.pending[id].raw...))
	}
	return result
}
