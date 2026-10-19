package engine

import (
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"io"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/usage"
)

// loggingStream wraps a stream to accumulate usage and log it on completion
type loggingStream struct {
	inner           core.Stream
	logger          *usage.Logger
	providerName    string
	model           string
	trackedExternal string // "claude-code", "codex", or "" for direct API

	// mu guards the accumulator/logged fields against concurrent Recv/Close.
	mu              sync.Mutex
	totalInput      int
	totalOutput     int
	totalCacheRead  int
	totalCacheWrite int
	logged          bool
}

func (s *loggingStream) Recv() (core.Event, error) {
	event, err := s.inner.Recv()

	if err == nil {
		switch event.Type {
		case core.EventUsage:
			if event.Use != nil {
				s.mu.Lock()
				s.totalInput += event.Use.InputTokens
				s.totalOutput += event.Use.OutputTokens
				s.totalCacheRead += event.Use.CachedInputTokens
				s.totalCacheWrite += event.Use.CacheWriteTokens
				s.mu.Unlock()
			}
		case core.EventDone:
			s.mu.Lock()
			if !s.logged {
				s.flushLocked()
			}
			s.mu.Unlock()
		}
		return event, nil
	}

	if err == io.EOF {
		s.mu.Lock()
		if !s.logged {
			s.flushLocked()
		}
		s.mu.Unlock()
	}

	return event, err
}

func (s *loggingStream) Close() error {
	s.mu.Lock()
	if !s.logged {
		s.flushLocked()
	}
	s.mu.Unlock()
	return s.inner.Close()
}

func (s *loggingStream) flushLocked() {
	if s.totalInput == 0 && s.totalOutput == 0 {
		return
	}
	s.logged = true
	_ = s.logger.Log(usage.LogEntry{
		Timestamp:           time.Now(),
		Model:               s.model,
		Provider:            s.providerName,
		InputTokens:         s.totalInput,
		OutputTokens:        s.totalOutput,
		CacheReadTokens:     s.totalCacheRead,
		CacheWriteTokens:    s.totalCacheWrite,
		TrackedExternallyBy: s.trackedExternal,
	})
}

// wrapLoggingStream wraps a stream with usage logging
func wrapLoggingStream(inner core.Stream, providerName, model string) core.Stream {
	// If model is empty, use providerName as the model identifier
	// This helps identify what was used when providers auto-select models
	if model == "" {
		model = providerName
	}
	return &loggingStream{
		inner:           inner,
		logger:          usage.DefaultLogger(),
		providerName:    providerName,
		model:           model,
		trackedExternal: usage.GetTrackedExternallyBy(providerName),
	}
}

// wrapDebugLoggingStream wraps a stream with debug logging if enabled
func (e *Engine) wrapDebugLoggingStream(inner core.Stream) core.Stream {
	if e.debugLogger == nil {
		return inner
	}
	return &debugLoggingStream{
		inner:  inner,
		logger: e.debugLogger,
	}
}

// debugLoggingStream wraps a stream to log events for debugging
type debugLoggingStream struct {
	inner  core.Stream
	logger DebugLogger
}

func (s *debugLoggingStream) Recv() (core.Event, error) {
	event, err := s.inner.Recv()
	if err == nil {
		s.logger.LogEvent(event)
	}
	return event, err
}

func (s *debugLoggingStream) Close() error {
	return s.inner.Close()
}

// cleanupStream wraps a stream to call provider per-turn cleanup on terminal
// conditions (io.EOF, EventDone, or Close). Used for per-turn resources such
// as CLI-provider prompt/image files. MCP servers and other
// conversation-scoped state are cleaned up elsewhere (runtime eviction).
type cleanupStream struct {
	inner     core.Stream
	cleanup   func()
	closeOnce sync.Once
}

func (s *cleanupStream) Recv() (core.Event, error) {
	event, err := s.inner.Recv()
	// Trigger cleanup on terminal conditions (EOF or EventDone)
	// This ensures cleanup runs even if consumer doesn't call Close()
	if err == io.EOF || (err == nil && event.Type == core.EventDone) {
		if cleanupErr := s.cleanupOnce(); cleanupErr != nil {
			return core.Event{}, cleanupErr
		}
	}
	return event, err
}

func (s *cleanupStream) Close() error {
	err := s.inner.Close()
	if cleanupErr := s.cleanupOnce(); err == nil {
		err = cleanupErr
	}
	return err
}

func (s *cleanupStream) cleanupOnce() (err error) {
	if s.cleanup == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("stream cleanup panic: %v", r)
			}
		}()
		s.cleanup()
	})
	return err
}
