package cmd

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sessiontitle"
)

const (
	serveAutoTitleTimeout     = 35 * time.Second
	serveAutoTitleMaxAttempts = 2
	serveAutoTitleConcurrency = 2
)

type serveAutoTitleAttempt struct {
	basisSeq int
	count    int
}

// scheduleAutoTitle starts a best-effort title request after a successful,
// persisted serve run. It never delays or changes the response run itself.
func (s *serveServer) scheduleAutoTitle(sessionID, providerKey string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || s == nil || s.store == nil || s.cfgRef == nil || !s.cfgRef.Serve.AutoTitle {
		return
	}

	s.autoTitleMu.Lock()
	if s.autoTitleStopping {
		s.autoTitleMu.Unlock()
		return
	}
	if _, running := s.autoTitleFlights[sessionID]; running {
		s.autoTitleMu.Unlock()
		return
	}
	if s.autoTitleAttempts[sessionID].count >= serveAutoTitleMaxAttempts {
		s.autoTitleMu.Unlock()
		return
	}
	if s.autoTitleFlights == nil {
		s.autoTitleFlights = make(map[string]struct{})
	}
	if s.autoTitleAttempts == nil {
		s.autoTitleAttempts = make(map[string]serveAutoTitleAttempt)
	}
	if s.autoTitleSlots == nil {
		s.autoTitleSlots = make(chan struct{}, serveAutoTitleConcurrency)
	}
	if s.autoTitleCtx == nil {
		s.autoTitleCtx, s.autoTitleCancel = context.WithCancel(context.Background())
	}
	baseCtx := s.autoTitleCtx
	s.autoTitleFlights[sessionID] = struct{}{}
	s.autoTitleWG.Add(1)
	s.autoTitleMu.Unlock()

	go func() {
		defer s.autoTitleWG.Done()
		defer func() {
			s.autoTitleMu.Lock()
			delete(s.autoTitleFlights, sessionID)
			s.autoTitleMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(baseCtx, serveAutoTitleTimeout)
		defer cancel()
		s.runAutoTitle(ctx, sessionID, providerKey)
	}()
}

func (s *serveServer) runAutoTitle(ctx context.Context, sessionID, providerKey string) {
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || !serveSessionNeedsAutoTitle(sess) {
		return
	}
	messages, err := s.store.GetMessages(ctx, sessionID, 80, 0)
	if err != nil || len(messages) == 0 {
		return
	}
	basisSeq := messages[len(messages)-1].Sequence

	s.autoTitleMu.Lock()
	attempt := s.autoTitleAttempts[sessionID]
	if attempt.count >= serveAutoTitleMaxAttempts || (attempt.count > 0 && basisSeq <= attempt.basisSeq) {
		s.autoTitleMu.Unlock()
		return
	}
	attempt.count++
	attempt.basisSeq = basisSeq
	s.autoTitleAttempts[sessionID] = attempt
	slots := s.autoTitleSlots
	s.autoTitleMu.Unlock()

	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return
	}

	provider, err := s.newAutoTitleProvider(providerKey)
	if err != nil || provider == nil {
		return
	}
	candidate, err := sessiontitle.Generate(ctx, provider, sess, messages)
	if err != nil {
		// Persist the skip as a cross-restart damper. Updated conversations may
		// retry, while the in-memory attempt cap prevents per-turn amplification.
		_ = s.store.MarkTitleSkipped(ctx, sessionID, time.Now().UTC())
		return
	}

	latest, err := s.store.Get(ctx, sessionID)
	if err != nil || !serveSessionNeedsAutoTitle(latest) {
		return
	}
	generatedAt := time.Now().UTC()
	if err := session.UpdateGeneratedTitle(ctx, s.store, latest, candidate.ShortTitle, candidate.LongTitle, generatedAt, basisSeq); err != nil {
		log.Printf("[serve] automatic title save failed for %s: %v", sessionID, err)
		return
	}

	latest.GeneratedShortTitle = candidate.ShortTitle
	latest.GeneratedLongTitle = candidate.LongTitle
	latest.TitleGeneratedAt = generatedAt
	latest.TitleBasisMsgSeq = basisSeq
	if latest.TitleSource != session.TitleSourceUser && strings.TrimSpace(latest.Name) == "" {
		latest.TitleSource = session.TitleSourceGenerated
	}
	if s.sessionMgr != nil {
		if runtime, ok := s.sessionMgr.Get(sessionID); ok {
			trySyncRuntimeSessionMetadata(runtime, latest)
		}
	}
	s.publishEvent(serveEventInput{Type: serveEventSessionMetadataChanged, SessionID: sessionID, Reason: "generated_title"})
}

func serveSessionNeedsAutoTitle(sess *session.Session) bool {
	return sess != nil &&
		strings.TrimSpace(sess.Name) == "" &&
		strings.TrimSpace(sess.GeneratedShortTitle) == "" &&
		strings.TrimSpace(sess.GeneratedLongTitle) == ""
}

func (s *serveServer) newAutoTitleProvider(providerKey string) (llm.Provider, error) {
	providerKey = strings.TrimSpace(providerKey)
	if s.autoTitleProviderFactory != nil {
		return s.autoTitleProviderFactory(providerKey)
	}
	if s.cfgRef == nil {
		return nil, nil
	}
	if providerKey == "" {
		providerKey = strings.TrimSpace(s.cfgRef.DefaultProvider)
	}
	provider, firstErr := llm.NewFastProvider(s.cfgRef, providerKey)
	if provider != nil {
		return provider, nil
	}
	fallback := strings.TrimSpace(s.cfgRef.DefaultProvider)
	if fallback == "" || fallback == providerKey {
		return nil, firstErr
	}
	provider, fallbackErr := llm.NewFastProvider(s.cfgRef, fallback)
	if provider != nil {
		return provider, nil
	}
	if firstErr != nil && fallbackErr != nil {
		return nil, fmt.Errorf("fast title providers %q and %q unavailable: %v; %v", providerKey, fallback, firstErr, fallbackErr)
	}
	if fallbackErr != nil {
		return nil, fallbackErr
	}
	return nil, firstErr
}

func (s *serveServer) stopAutoTitles() {
	if s == nil {
		return
	}
	s.autoTitleMu.Lock()
	s.autoTitleStopping = true
	if s.autoTitleCancel != nil {
		s.autoTitleCancel()
	}
	s.autoTitleMu.Unlock()
	s.autoTitleWG.Wait()
}
