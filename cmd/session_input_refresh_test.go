package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func inputTestStore(t *testing.T, path string) session.Store {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
func TestSessionInputCoordinatorConcurrentRetryAndInterning(t *testing.T) {
	c := newSessionInputCoordinator()
	store := inputTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	binding := inputBinding("agent", "dir")
	failed, err := c.acquire(ctx, store, "session", binding)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.acquire(cancelled, store, "session", binding); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var owners atomic.Int32
	var wg sync.WaitGroup
	selections := make(chan *sessionInputSelection, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticket, err := c.acquire(ctx, store, "session", binding)
			if err != nil {
				t.Error(err)
				return
			}
			defer ticket.fail()
			if ticket.owner {
				owners.Add(1)
				selections <- ticket.finish(sessionInputSelection{Prompt: "same", Tools: "shell"})
			} else {
				selections <- ticket.selected()
			}
		}()
	}
	failed.fail()
	wg.Wait()
	close(selections)
	if owners.Load() != 1 {
		t.Fatalf("owners = %d", owners.Load())
	}
	var first *sessionInputSelection
	for pair := range selections {
		if first == nil {
			first = pair
		}
		if pair != first {
			t.Fatal("not a shared immutable selection")
		}
	}
	other, _ := c.acquire(ctx, store, "other", binding)
	if other.finish(*first) != first {
		t.Fatal("equal pairs not interned")
	}
	if _, err := c.acquire(ctx, store, "session", inputBinding("other", "dir")); err == nil {
		t.Fatal("binding mismatch accepted")
	}
	c.invalidate(store, "session")
	changed, err := c.acquire(ctx, store, "session", inputBinding("other", "dir"))
	if err != nil || !changed.owner {
		t.Fatalf("invalidation: %v", err)
	}
	changed.fail()
}
func TestSessionInputCoordinatorNamespacesAndFreshFailure(t *testing.T) {
	c := newSessionInputCoordinator()
	path := filepath.Join(t.TempDir(), "test.db")
	a := inputTestStore(t, path)
	b := inputTestStore(t, path)
	memA := inputTestStore(t, ":memory:")
	memB := inputTestStore(t, ":memory:")
	ctx := context.Background()
	binding := inputBinding("", "")
	ticket, _ := c.acquire(ctx, a, "session", binding)
	selected := ticket.finish(sessionInputSelection{Prompt: "original"})
	hit, _ := c.acquire(ctx, &session.LoggingStore{Store: b}, "session", binding)
	if hit.owner || hit.selected() != selected {
		t.Fatal("file/decorator identity differs")
	}
	for _, store := range []session.Store{memA, memB} {
		ticket, _ := c.acquire(ctx, store, "session", binding)
		if !ticket.owner {
			t.Fatal("memory databases aliased")
		}
		ticket.finish(sessionInputSelection{})
	}
	fresh, _ := c.acquireFresh(ctx, a, "session", binding)
	fresh.fail()
	if c.ready(a, "session") != selected {
		t.Fatal("failed candidate lost old selection")
	}
	pending, _ := c.acquire(ctx, a, "pending", binding)
	defer pending.fail()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := c.acquire(waitCtx, a, "pending", binding); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestSessionInputToolEquality(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{{"shell,view_image,shell", " view_image, shell ", true}, {"all", "*", true}, {"", " , ", true}, {"shell", "", false}} {
		if equalSessionTools(tc.a, tc.b) != tc.equal {
			t.Fatalf("%q vs %q", tc.a, tc.b)
		}
	}
}
