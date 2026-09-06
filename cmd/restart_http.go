package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/restart"
)

// processExecOwner is the transport/control-plane adapter: remote model work is
// owned by its node, not by the Hub's forwarding socket. Only admitted local
// mutation handlers must settle; passive connections reconnect after exec.
type processExecOwner struct {
	next             uint64
	ctx              context.Context
	mode, executable string
	grace            time.Duration
	gate             restart.Gate
	requested        atomic.Bool
	mu               sync.Mutex
	cancelling       bool
	cancels          map[uint64]context.CancelFunc
	exec             func(string, []string, []string) error
}

func (o *processExecOwner) enter(parent context.Context) (context.Context, func(), error) {
	return o.track(parent, true)
}
func (o *processExecOwner) child(parent context.Context) (context.Context, func(), error) {
	return o.track(parent, false)
}
func (o *processExecOwner) track(parent context.Context, root bool) (context.Context, func(), error) {
	var release func()
	if root {
		var ok bool
		release, ok = o.gate.Enter()
		if !ok {
			return nil, nil, errors.New("process restarting")
		}
	} else {
		release = o.gate.TrackChild()
	}
	ctx, cancel := context.WithCancel(parent)
	o.mu.Lock()
	o.next++
	id := o.next
	o.cancels[id] = cancel
	if o.cancelling {
		cancel()
	}
	o.mu.Unlock()
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); o.mu.Lock(); delete(o.cancels, id); o.mu.Unlock(); release() }) }, nil
}

func (o *processExecOwner) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		ctx, release, err := o.enter(r.Context())
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "process restarting; retry", http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (o *processExecOwner) request() {
	if !o.requested.CompareAndSwap(false, true) {
		return
	}
	reopen := o.gate.Pause()
	process.State(o.mode, "draining", "")
	go func() {
		defer o.requested.Store(false)
		defer func() { o.mu.Lock(); o.cancelling = false; o.mu.Unlock(); reopen() }()
		timer := time.NewTimer(o.grace)
		defer timer.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		cancelling := false
		for {
			if o.ctx.Err() != nil {
				return
			}
			if o.gate.Drained() {
				process.State(o.mode, "replacing", "")
				err := o.exec(o.executable, append([]string(nil), os.Args...), webExecEnviron("", ""))
				if err != nil {
					process.State(o.mode, "failed", err.Error())
				}
				return
			}
			select {
			case <-o.ctx.Done():
				return
			case <-tick.C:
			case <-timer.C:
				if cancelling {
					process.State(o.mode, "failed", "cancelled mutation handlers did not settle")
					return
				}
				cancelling = true
				process.State(o.mode, "cancelling", "restart grace elapsed")
				o.mu.Lock()
				o.cancelling = true
				for _, cancel := range o.cancels {
					cancel()
				}
				o.mu.Unlock()
				timer.Reset(o.grace)
			}
		}
	}()
}

func serveHTTPWithRestart(ctx context.Context, mode string, srv *http.Server) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	owner := &processExecOwner{ctx: ctx, mode: mode, executable: executable, grace: 10 * time.Second, cancels: make(map[uint64]context.CancelFunc), exec: execWebProcess}
	srv.Handler = owner.handler(srv.Handler)
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	unbind := restart.Default.Bind(owner.request)
	defer unbind()
	finished := make(chan error, 1)
	go func() { finished <- srv.Serve(listener) }()
	process.State(mode, "ready", "")
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(stop); err != nil {
			_ = srv.Close()
		}
		if err := <-finished; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		return nil
	}
}
