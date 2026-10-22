package engine

import (
	"errors"

	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// errCompactionUnfinished reports a run that ended while a compaction span was
// still active. runLoop's deferred cleanup uses it so an aborted attempt is
// never projected as a successful completion.
var errCompactionUnfinished = errors.New("compaction did not finish before the run ended")

// errCompactionBriefUnusable reports a soft-compaction brief turn that produced
// no applicable history, leaving the original transcript in place.
var errCompactionBriefUnusable = errors.New("soft compaction brief did not produce usable history")

// compactionLifecycle tracks engine-owned compaction activity for one run and
// emits balanced core.EventCompactionStatus transitions.
//
// A span starts when the engine begins compacting and ends when the compacted
// history is applied, the attempt fails, or the run stops. Soft compaction can
// hand off to a hard fallback summary, and a post-response compaction can run
// while a soft brief is still pending, so spans are counted: status events only
// cross the idle/active boundary. That yields one begin/end pair per contiguous
// compaction period and never emits an idle status while a brief turn is still
// running.
type compactionLifecycle struct {
	send  corestream.Sender
	depth int
}

func newCompactionLifecycle(send corestream.Sender) compactionLifecycle {
	return compactionLifecycle{send: send}
}

// active reports whether any compaction span is currently open.
func (l *compactionLifecycle) active() bool { return l.depth > 0 }

// begin starts a compaction span. It is idempotent from the wire perspective:
// nested spans keep the lifecycle active without a second begin event.
func (l *compactionLifecycle) begin() {
	l.depth++
	if l.depth == 1 {
		l.sendStatus(true, nil)
	}
}

// end closes one compaction span, emitting the idle status only when the last
// active span ends. err describes the failure of the span being closed.
func (l *compactionLifecycle) end(err error) {
	if l.depth == 0 {
		return
	}
	l.depth--
	if l.depth == 0 {
		l.sendStatus(false, err)
	}
}

// finish closes every remaining span when the owning run exits. It always
// leaves the lifecycle idle so a failed, cancelled, or suspended run cannot
// project a permanently active compaction.
func (l *compactionLifecycle) finish(err error) {
	if l.depth == 0 {
		return
	}
	if err == nil {
		err = errCompactionUnfinished
	}
	l.depth = 0
	l.sendStatus(false, err)
}

func (l *compactionLifecycle) sendStatus(active bool, err error) {
	if sendErr := l.send.Send(core.Event{Type: core.EventCompactionStatus, CompactionActive: active, Err: err}); sendErr != nil {
		runtimeoutput.Debug("send compaction status failed", "active", active, "error", sendErr)
	}
}
