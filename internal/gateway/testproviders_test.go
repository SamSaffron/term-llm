package gateway

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
)

type oneEventStream struct {
	ctx   context.Context
	event llm.Event
	sent  bool
	err   error
}

func (s *oneEventStream) Recv() (llm.Event, error) {
	if !s.sent {
		s.sent = true
		return s.event, nil
	}
	if s.err != nil {
		return llm.Event{}, s.err
	}
	return llm.Event{}, io.EOF
}
func (*oneEventStream) Close() error { return nil }

type statefulProvider struct {
	mu       sync.Mutex
	imported string
	next     int
}

func (*statefulProvider) Name() string                   { return "stateful" }
func (*statefulProvider) Credential() string             { return "mock" }
func (*statefulProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *statefulProvider) ImportProviderState(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.imported = string(data)
	return nil
}
func (p *statefulProvider) ExportProviderState() ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return []byte(fmt.Sprintf("state-%d", p.next)), true
}
func (p *statefulProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.next++
	p.mu.Unlock()
	return &oneEventStream{ctx: ctx, event: llm.Event{Type: llm.EventTextDelta, Text: "ok"}}, nil
}

type streamDeathProvider struct{}

func (*streamDeathProvider) Name() string       { return "dead" }
func (*streamDeathProvider) Credential() string { return "mock" }
func (*streamDeathProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}
func (*streamDeathProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	return &oneEventStream{ctx: ctx, event: llm.Event{Type: llm.EventTextDelta, Text: "partial"}, err: fmt.Errorf("secret upstream transport detail")}, nil
}

type blockingProvider struct{ canceled chan struct{} }

func (*blockingProvider) Name() string                   { return "blocking" }
func (*blockingProvider) Credential() string             { return "mock" }
func (*blockingProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *blockingProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	return &blockingStream{ctx: ctx, canceled: p.canceled}, nil
}

type blockingStream struct {
	ctx      context.Context
	canceled chan struct{}
	once     sync.Once
}

func (s *blockingStream) Recv() (llm.Event, error) {
	<-s.ctx.Done()
	s.once.Do(func() { close(s.canceled) })
	return llm.Event{}, s.ctx.Err()
}
func (*blockingStream) Close() error { return nil }
