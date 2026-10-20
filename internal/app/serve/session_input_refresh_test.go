package serve

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestSessionInputCoordinatorConcurrentRetryAndInterning(t *testing.T) {
	c := NewSessionInputCoordinator()
	store := testutil.NewSessionInputStore(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	binding := InputBinding("agent", "dir")
	failed, err := c.Acquire(ctx, store, "session", binding)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Acquire(cancelled, store, "session", binding); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var owners atomic.Int32
	var wg sync.WaitGroup
	selections := make(chan *SessionInputSelection, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticket, err := c.Acquire(ctx, store, "session", binding)
			if err != nil {
				t.Error(err)
				return
			}
			defer ticket.Fail()
			if ticket.Owner() {
				owners.Add(1)
				selections <- ticket.Finish(SessionInputSelection{Prompt: "same", Tools: "shell"})
			} else {
				selections <- ticket.Selected()
			}
		}()
	}
	failed.Fail()
	wg.Wait()
	close(selections)
	if owners.Load() != 1 {
		t.Fatalf("owners = %d", owners.Load())
	}
	var first *SessionInputSelection
	for pair := range selections {
		if first == nil {
			first = pair
		}
		if pair != first {
			t.Fatal("not a shared immutable selection")
		}
	}
	other, _ := c.Acquire(ctx, store, "other", binding)
	if other.Finish(*first) != first {
		t.Fatal("equal pairs not interned")
	}
	if _, err := c.Acquire(ctx, store, "session", InputBinding("other", "dir")); err == nil {
		t.Fatal("binding mismatch accepted")
	}
	c.Invalidate(store, "session")
	changed, err := c.Acquire(ctx, store, "session", InputBinding("other", "dir"))
	if err != nil || !changed.Owner() {
		t.Fatalf("invalidation: %v", err)
	}
	changed.Fail()
}
func TestSessionInputCoordinatorNamespacesAndFreshFailure(t *testing.T) {
	c := NewSessionInputCoordinator()
	path := filepath.Join(t.TempDir(), "test.db")
	a := testutil.NewSessionInputStore(t, path)
	b := testutil.NewSessionInputStore(t, path)
	memA := testutil.NewSessionInputStore(t, ":memory:")
	memB := testutil.NewSessionInputStore(t, ":memory:")
	ctx := context.Background()
	binding := InputBinding("", "")
	ticket, _ := c.Acquire(ctx, a, "session", binding)
	selected := ticket.Finish(SessionInputSelection{Prompt: "original"})
	hit, _ := c.Acquire(ctx, &session.LoggingStore{Store: b}, "session", binding)
	if hit.Owner() || hit.Selected() != selected {
		t.Fatal("file/decorator identity differs")
	}
	for _, store := range []session.Store{memA, memB} {
		ticket, _ := c.Acquire(ctx, store, "session", binding)
		if !ticket.Owner() {
			t.Fatal("memory databases aliased")
		}
		ticket.Finish(SessionInputSelection{})
	}
	fresh, _ := c.AcquireFresh(ctx, a, "session", binding)
	fresh.Fail()
	if c.Ready(a, "session") != selected {
		t.Fatal("failed candidate lost old selection")
	}
	pending, _ := c.Acquire(ctx, a, "pending", binding)
	defer pending.Fail()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := c.Acquire(waitCtx, a, "pending", binding); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestSessionInputToolEquality(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{{"shell,view_image,shell", " view_image, shell ", true}, {"all", "*", true}, {"", " , ", true}, {"shell", "", false}} {
		if EqualSessionTools(tc.a, tc.b) != tc.equal {
			t.Fatalf("%q vs %q", tc.a, tc.b)
		}
	}
}
