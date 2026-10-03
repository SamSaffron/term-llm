package tea

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
)

func TestListenForSignalsReturnsAfterContextCancelDuringPendingSend(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				p := &Program{ctx: ctx, msgs: make(chan Msg)}
				signals := make(chan os.Signal)
				done := make(chan struct{})
				go func() {
					defer close(done)
					p.listenForSignals(signals)
				}()

				// No event loop reads msgs. Wait until the handler has consumed
				// the signal and is blocked sending its message before canceling.
				signals <- sig
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("signal handler returned before delivery or cancellation")
				default:
				}

				cancel()
				synctest.Wait()
				select {
				case <-done:
				default:
					// Unblock the old implementation so failure leaves no leaked
					// goroutine or synctest deadlock obscuring the regression.
					<-p.msgs
					<-done
					t.Fatal("signal handler remained blocked on msgs after cancellation")
				}
			})
		})
	}
}

func TestListenForSignalsDeliversMessage(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				p := &Program{ctx: ctx, msgs: make(chan Msg)}
				signals := make(chan os.Signal)
				done := make(chan struct{})
				go func() {
					defer close(done)
					p.listenForSignals(signals)
				}()

				atomic.StoreUint32(&p.ignoreSignals, 1)
				signals <- sig
				synctest.Wait()
				select {
				case msg := <-p.msgs:
					t.Fatalf("ignored signal delivered %T", msg)
				case <-done:
					t.Fatal("ignored signal stopped handler")
				default:
				}

				atomic.StoreUint32(&p.ignoreSignals, 0)
				signals <- sig
				msg := <-p.msgs
				if sig == syscall.SIGINT {
					if _, ok := msg.(InterruptMsg); !ok {
						t.Fatalf("SIGINT delivered %T, want InterruptMsg", msg)
					}
				} else if _, ok := msg.(QuitMsg); !ok {
					t.Fatalf("SIGTERM delivered %T, want QuitMsg", msg)
				}
				<-done
			})
		})
	}
}
