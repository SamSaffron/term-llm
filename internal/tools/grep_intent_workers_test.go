package tools

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/greprerank"
)

func TestIntentBusyReturnsOrdinaryOutputWithoutExpansion(t *testing.T) {
	requireGoFallback(t)
	root := scoredFiles(t, .9, .8)
	backend := newScoringFake()
	tool, factory := newIntentTool(t, backend, nil)
	var scans atomic.Int32
	tool.intentScan = func(context.Context, string, *regexp.Regexp, int, func([]string, int) error) error {
		scans.Add(1)
		return errors.New("busy path must not expand")
	}
	// Ordinary calls are not subject to optional-worker admission at all.
	ordinary := execGrep(t, tool, root, map[string]any{"max_results": 1})
	release := make(chan struct{})
	var exited sync.WaitGroup
	defer func() {
		close(release)
		exited.Wait()
		// Observe recovery through public admission rather than a sleep. Permits
		// remain held until the actual pipeline goroutine (not just search) exits.
		deadline := time.Now().Add(time.Second)
		for {
			r := greprerank.New(nil, "", greprerank.Options{})
			a, err := r.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = greprerank.RunAttempt(context.Background(), a, func(context.Context, func(string)) (string, error) { return "prefix", nil }, func(context.Context, string) (string, error) { return "", greprerank.ErrSkipped }, func(s string) string { return s })
			if !errors.Is(err, greprerank.ErrBusy) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("search admission did not recover")
			}
			runtime.Gosched()
		}
	}()
	for range greprerank.MaxSearchWorkers {
		r := greprerank.New(nil, "", greprerank.Options{Budget: 10 * time.Millisecond})
		a, err := r.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = greprerank.RunAttempt(context.Background(), a, func(_ context.Context, publish func(string)) (string, error) {
			exited.Add(1)
			defer exited.Done()
			publish("prefix")
			<-release // deliberately ignores ctx after its caller times out
			return "expanded", nil
		}, func(context.Context, string) (string, error) { return "", nil }, func(s string) string { return s })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	started := time.Now()
	got := execGrep(t, tool, root, map[string]any{"intent": "writes", "max_results": 1})
	if elapsed := time.Since(started); elapsed > reviewedMargin {
		t.Fatalf("busy admission waited %v", elapsed)
	}
	if want := skipNote("busy") + ordinary; got != want {
		t.Fatalf("busy fallback\n got %q\nwant %q", got, want)
	}
	if plain := execGrep(t, tool, root, map[string]any{"max_results": 1}); plain != ordinary {
		t.Fatalf("saturation changed ordinary grep: %q", plain)
	}
	if scans.Load() != 0 || factory.opens.Load() != 0 || backend.calls() != 0 {
		t.Fatalf("busy call expanded or classified: scans=%d opens=%d calls=%d", scans.Load(), factory.opens.Load(), backend.calls())
	}
}
