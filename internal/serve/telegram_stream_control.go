package serve

import (
	"context"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm"
	"sync"
	"sync/atomic"
)

var errTelegramRestart = errors.New("Telegram stream interrupted for process replacement")

// telegramReplyCursor preserves the unfinished text segment, not prior delivered chunks.
type telegramReplyCursor struct {
	MessageID      int                 `json:"message_id"`
	Prefix         string              `json:"prefix,omitempty"`
	NeedNewMessage bool                `json:"need_new_message,omitempty"`
	Images         []string            `json:"images,omitempty"`
	Media          []llm.MediaArtifact `json:"media,omitempty"`
	PendingMedia   []llm.MediaArtifact `json:"pending_media,omitempty"`
}

// Cursor media is not-yet-sent work, not a list of already delivered uploads.
// Uncertain outbound sends still require resolution before handoff sealing.
func (c *telegramStreamControl) replyCursor() *telegramReplyCursor {
	if !c.restarting() {
		return nil
	}
	cursor := c.reply.Load()
	if cursor == nil {
		return nil
	}
	copy := *cursor
	copy.Images = append([]string(nil), cursor.Images...)
	copy.Media = append([]llm.MediaArtifact(nil), cursor.Media...)
	copy.PendingMedia = append([]llm.MediaArtifact(nil), cursor.PendingMedia...)
	return &copy
}

// telegramStreamControl distinguishes a lifecycle interruption from user Stop.
// Context cancellation is first-cause-wins; Stop must still revoke continuation
// when it arrives after the restart cancellation has already won that race.
// This is cancellation intent only: the restart owner must freeze execution
// before interrupting and join actual work before authorizing any handoff.
type telegramStreamControl struct {
	mu              sync.Mutex
	engine          *llm.Engine
	initialTurns    int64
	turnLimit       int
	ready           bool
	frozen          bool
	pendingSteering []llm.QueuedSteering
	ctx             context.Context
	parent          context.Context
	cancel          context.CancelCauseFunc
	stopped         atomic.Bool
	reply           atomic.Pointer[telegramReplyCursor]
}

func newTelegramStreamControl(parent context.Context) *telegramStreamControl {
	ctx, cancel := context.WithCancelCause(parent)
	return &telegramStreamControl{ctx: ctx, parent: parent, cancel: cancel}
}
func (c *telegramStreamControl) stop()                { c.stopped.Store(true); c.cancel(context.Canceled) }
func (c *telegramStreamControl) interruptForRestart() { c.cancel(errTelegramRestart) }
func (c *telegramStreamControl) close()               { c.cancel(context.Canceled) }
func (c *telegramStreamControl) restarting() bool {
	return c.parent.Err() == nil && context.Cause(c.ctx) == errTelegramRestart && !c.stopped.Load()
}

func (c *telegramStreamControl) bind(engine *llm.Engine, limit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.engine = engine
	c.initialTurns = engine.ModelTurns()
	c.turnLimit = (llm.Request{MaxTurns: limit}).TurnLimit()
}

// Installed only after the original input committed to durable storage, and
// only for a provider that actually supports engine-managed model boundaries.
func (c *telegramStreamControl) modelBoundary(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.ready = true
	return nil
}

func (c *telegramStreamControl) freezeForRestart(owner llm.SteeringTransition) ([]llm.QueuedSteering, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.engine == nil || c.stopped.Load() || c.ctx.Err() != nil {
		return nil, fmt.Errorf("Telegram execution is not ready for restart")
	}
	pending, err := c.engine.FreezeExecutionSnapshot(owner)
	if err != nil {
		return nil, err
	}
	c.frozen = true
	c.pendingSteering = append([]llm.QueuedSteering(nil), pending...)
	c.interruptForRestart()
	return pending, nil
}

// Read only after actual execution settles, not when cancellation returns.
func (c *telegramStreamControl) remainingTurns() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.engine == nil {
		return 0
	}
	used := int(c.engine.ModelTurns() - c.initialTurns)
	return max(0, c.turnLimit-used)
}

type telegramExecutionState struct {
	RemainingTurns int                  `json:"remaining_turns"`
	Steering       []llm.QueuedSteering `json:"steering,omitempty"`
	Reply          *telegramReplyCursor `json:"reply"`
}

// Caller must have stopped admission and joined the complete manager gate.
func (c *telegramStreamControl) executionState() (*telegramExecutionState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.restarting() {
		return nil, nil
	}
	if !c.frozen || c.engine == nil {
		return nil, fmt.Errorf("Telegram restart execution was not frozen")
	}
	reply := c.replyCursor()
	if reply == nil {
		return nil, fmt.Errorf("Telegram restart delivery has not settled")
	}
	return &telegramExecutionState{RemainingTurns: max(0, c.turnLimit-int(c.engine.ModelTurns()-c.initialTurns)), Steering: append([]llm.QueuedSteering(nil), c.pendingSteering...), Reply: reply}, nil
}
