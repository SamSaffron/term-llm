package serve

import (
	"context"
	"errors"
	"sync"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/restart"
)

func (s *serveServer) reloadTransport() *restart.HTTPTransport {
	s.reloadHTTPOnce.Do(func() { s.reloadHTTP = &restart.HTTPTransport{Coordinator: restart.Default} })
	return s.reloadHTTP
}

// contextWithShutdown returns a derived context that is cancelled when either
// the parent context is done or shutdownCh is closed. This lets streaming
// handlers exit promptly on server shutdown rather than holding server.Shutdown
// open until the full timeout expires.
func (s *serveServer) contextWithShutdown(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if s.shutdownCh == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-s.shutdownCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (s *serveServer) Stop(ctx context.Context) error {
	if s.linkedDevices != nil {
		_ = s.linkedDevices.Close()
	}
	if s.reloadRunsUnregister != nil {
		s.reloadRunsUnregister()
		s.reloadRunsUnregister = nil
	}
	// Signal all SSE handlers and direct child runs to return immediately so
	// server shutdown cannot outlive the session store they persist into.
	s.shutdownOnce.Do(func() {
		if s.shutdownCh != nil {
			close(s.shutdownCh)
		}
	})
	defer s.agentWakeWG.Wait()
	s.closeLiveSessions(ctx)
	if err := s.agentOwner.Shutdown(ctx); err != nil {
		return err
	}
	s.agentWakeWG.Wait()
	s.closeShellManager()
	s.stopEventWatcher()
	// Synchronize with a concurrently starting lifecycle loop, or permanently
	// suppress a late start once shutdown has begun.
	s.responseLifecycleOnce.Do(func() {})
	if s.responseLifecycleCancel != nil {
		s.responseLifecycleCancel()
		s.responseLifecycleWG.Wait()
	}
	s.closeEventBroker()
	if s.server == nil {
		s.stopAutoTitles()
		return errors.Join(s.stopServeSkillRuns(ctx), s.stopCommitWorkflows(ctx))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 7)
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}
	if s.jobsV2 != nil {
		run(func() error { return s.jobsV2.CloseContext(ctx) })
	}
	if s.responseRuns != nil {
		run(func() error {
			s.responseRuns.CloseContext(ctx)
			return nil
		})
	}
	run(func() error {
		done := make(chan struct{})
		go func() {
			s.completionPushWG.Wait()
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if s.widgetsMgr != nil {
		run(func() error {
			s.widgetsMgr.CloseContext(ctx)
			return nil
		})
	}
	run(func() error { return s.stopServeSkillRuns(ctx) })
	run(func() error { return s.stopCommitWorkflows(ctx) })
	run(func() error {
		s.stopAutoTitles()
		return nil
	})
	run(func() error { return s.server.Shutdown(ctx) })

	s.modelsMu.Lock()
	for _, p := range s.modelsProviders {
		if cleaner, ok := p.(interface{ CleanupMCP() }); ok {
			cleaner.CleanupMCP()
		}
	}
	s.modelsProviders = nil
	s.modelsCache = nil
	s.modelsMu.Unlock()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Shutdown methods may return as soon as ctx expires even though their
		// owners are still unwinding. Keep the process-wide file history store
		// open in that case so late tool recorders can finish safely.
		if err := ctx.Err(); err != nil {
			return err
		}
		appruntime.CloseFileTrackingStore()
		close(errCh)
		for err := range errCh {
			if err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
