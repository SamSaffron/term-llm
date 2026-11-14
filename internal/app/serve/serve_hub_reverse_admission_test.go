package serve

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	appserver "github.com/term-llm/term-llm/internal/app/serve/server"
)

var (
	errHubReverseBusy             = appserver.ErrReverseBusy
	errHubReverseQueueFull        = appserver.ErrReverseAdmissionFull
	errHubReverseAdmissionTimeout = appserver.ErrReverseAdmissionTimeout
)

func TestHubReverseAdmissionWaitsForCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c hubReverseConnection
		for range hubReverseMaxActive {
			if err := c.admit(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan error, 1)
		go func() { done <- c.admit(t.Context()) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("burst request rejected instead of waiting: %v", err)
		default:
		}
		<-c.active
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(c.active) != hubReverseMaxActive {
			t.Fatalf("active=%d, want %d", len(c.active), hubReverseMaxActive)
		}
		for range hubReverseMaxActive {
			<-c.active
		}
		if err := c.admit(context.Background()); err != nil {
			t.Fatalf("admission did not recover: %v", err)
		}
		<-c.active
	})
}

func TestHubReverseAdmissionReleaseRacesStop(t *testing.T) {
	for _, ending := range []string{"cancel", "close"} {
		t.Run(ending, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Race release against stopping a blocked admission. A caller
				// that wins admission owns its slot; a rejected caller must not
				// retain one, regardless of which ready select case wins.
				for range 64 {
					var c hubReverseConnection
					c.initIO()
					for range hubReverseMaxActive {
						c.active <- struct{}{}
					}
					ctx, cancel := context.WithCancel(t.Context())
					done := make(chan error, 1)
					go func() { done <- c.admit(ctx) }()
					synctest.Wait()
					if len(c.waiting) != 1 {
						t.Fatal("caller did not enter admission queue")
					}
					start := make(chan struct{})
					go func() { <-start; <-c.active }()
					go func() {
						<-start
						if ending == "cancel" {
							cancel()
						} else {
							c.closeWithError(errors.New("test tunnel shutdown"))
						}
					}()
					close(start)
					synctest.Wait()
					err := <-done
					cancel()
					if err == nil {
						// Admission may legitimately beat cancellation/close.
						<-c.active
					} else if ending == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatalf("raced cancellation=%v", err)
					}
					if len(c.active) != hubReverseMaxActive-1 || len(c.waiting) != 0 {
						t.Fatalf("raced stop leaked slots: active=%d waiting=%d", len(c.active), len(c.waiting))
					}
					for range hubReverseMaxActive - 1 {
						<-c.active
					}
				}
			})
		})
	}
}

func TestHubReverseAdmissionWaitingBounds(t *testing.T) {
	for _, ending := range []string{"cancel", "close", "timeout"} {
		t.Run(ending, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var c hubReverseConnection
				for range hubReverseMaxActive {
					if err := c.admit(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, hubReverseMaxWaiting)
				for range hubReverseMaxWaiting {
					go func() { done <- c.admit(ctx) }()
				}
				synctest.Wait()
				if len(c.waiting) != hubReverseMaxWaiting || len(c.active) != hubReverseMaxActive {
					t.Fatalf("active=%d waiting=%d", len(c.active), len(c.waiting))
				}
				if err := c.admit(t.Context()); !errors.Is(err, errHubReverseQueueFull) || !errors.Is(err, errHubReverseBusy) {
					t.Fatalf("full queue admission=%v", err)
				}
				start := time.Now()
				switch ending {
				case "cancel":
					cancel()
				case "close":
					c.closeWithError(errors.New("test tunnel shutdown"))
				case "timeout":
					time.Sleep(hubReverseAdmissionWait) // synctest's virtual clock
				}
				synctest.Wait()
				for range hubReverseMaxWaiting {
					err := <-done
					switch ending {
					case "cancel":
						if !errors.Is(err, context.Canceled) || errors.Is(err, errHubReverseBusy) {
							t.Fatalf("waiting cancellation=%v", err)
						}
					case "timeout":
						// Busy, and not mistakable for the caller's own deadline.
						if !errors.Is(err, errHubReverseAdmissionTimeout) || !errors.Is(err, errHubReverseBusy) || errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("waiting expiry=%v", err)
						}
					case "close":
						if err == nil {
							t.Fatal("closed tunnel admitted waiting request")
						}
					}
				}
				if ending == "timeout" && time.Since(start) != hubReverseAdmissionWait {
					t.Fatalf("queue expiry after %v", time.Since(start))
				}
				if len(c.waiting) != 0 || len(c.active) != hubReverseMaxActive {
					t.Fatalf("after wait: active=%d waiting=%d", len(c.active), len(c.waiting))
				}
				// These active slots were acquired directly, so their owners (not
				// closeWithError's pending map) release them.
				for range hubReverseMaxActive {
					<-c.active
				}
				if ending != "close" {
					if err := c.admit(t.Context()); err != nil {
						t.Fatalf("admission failed to recover: %v", err)
					}
					<-c.active
				} else if err := c.admit(t.Context()); err == nil {
					t.Fatal("closed tunnel admitted new request")
				}
			})
		})
	}
}

func TestHubReverseAdmissionCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var c hubReverseConnection
		c.initIO()
		for range hubReverseMaxActive {
			c.active <- struct{}{}
		}
		ctx, cancel := context.WithTimeout(t.Context(), hubReverseAdmissionWait/4)
		defer cancel()
		start := time.Now()
		if err := c.admit(ctx); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errHubReverseBusy) {
			t.Fatalf("caller deadline=%v", err)
		}
		if time.Since(start) != hubReverseAdmissionWait/4 || len(c.waiting) != 0 {
			t.Fatalf("deadline after %v, waiting=%d", time.Since(start), len(c.waiting))
		}
		for range hubReverseMaxActive {
			<-c.active
		}
		// Already-canceled contexts must not take even an immediately free slot.
		if err := c.admit(ctx); !errors.Is(err, context.DeadlineExceeded) || len(c.active) != 0 {
			t.Fatalf("expired caller admitted: err=%v active=%d", err, len(c.active))
		}
	})
}
