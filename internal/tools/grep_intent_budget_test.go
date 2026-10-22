package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/grepsearch"
)

const reviewedBudget = 50 * time.Millisecond
const reviewedMargin = 250 * time.Millisecond

// This subprocess stands in for rg: ordinary cap=2 completes immediately,
// whereas cap=150 finds the same prefix and then stalls during expansion.
func TestIntentSlowRipgrepProcess(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_SLOW_RG") != "1" {
		return
	}
	root := os.Getenv("TERM_LLM_TEST_RG_ROOT")
	cap := 150
	for i, arg := range os.Args {
		if arg == "--max-count" && i+1 < len(os.Args) {
			cap, _ = strconv.Atoi(os.Args[i+1])
		}
	}
	for i := 1; i <= 2; i++ {
		event := rgMatchJSON(filepath.Join(root, fmt.Sprintf("f%d.txt", i)), 1, fmt.Sprintf("needle score=0.%d", 10-i))
		if os.Getenv("TERM_LLM_TEST_RG_PENDING") == "1" && i == 2 && cap > 2 {
			event = strings.SplitAfterN(event, "\n", 2)[0]
		}
		fmt.Print(event)
	}
	if cap <= 2 {
		fmt.Print(rgMatchJSON(filepath.Join(root, "f3.txt"), 1, "needle"))
		os.Exit(0)
	}
	<-time.After(2 * time.Second)
	os.Exit(0)
}

func TestIntentBudgetCoversSlowRipgrepExpansion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable shell adapter")
	}
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%v", pending), func(t *testing.T) {
			root := scoredFiles(t, .9, .8, .7)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\nexec '%s' -test.run='^TestIntentSlowRipgrepProcess$' -- \"$@\"\n", executable)
			if err := os.WriteFile(filepath.Join(dir, "rg"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			t.Setenv("TERM_LLM_TEST_SLOW_RG", "1")
			t.Setenv("TERM_LLM_TEST_RG_ROOT", root)
			t.Setenv("TERM_LLM_TEST_RG_PENDING", strconv.FormatBool(pending))
			if pending {
				t.Setenv("TERM_LLM_TEST_RG_PENDING", "1")
			}
			backend := newScoringFake()
			tool, factory := newIntentTool(t, backend, func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
			ordinaryStart := time.Now()
			ordinary := execGrep(t, tool, root, map[string]any{"max_results": 2})
			ordinaryTime := time.Since(ordinaryStart)
			started := time.Now()
			got := execGrep(t, tool, root, map[string]any{"max_results": 2, "intent": "writes"})
			if elapsed := time.Since(started); elapsed > ordinaryTime+reviewedBudget+reviewedMargin {
				t.Fatalf("expanded grep took %v, ordinary=%v budget=%v", elapsed, ordinaryTime, reviewedBudget)
			}
			if want := skipNote("timeout") + ordinary; got != want {
				t.Fatalf("fallback\n got %q\nwant %q", got, want)
			}
			if backend.calls() != 0 || factory.opens.Load() != 0 {
				t.Fatal("classification started after expansion exhausted the budget")
			}
		})
	}
}

func TestIntentBudgetCoversGoExpansionAndCancelsScanner(t *testing.T) {
	requireGoFallback(t)
	root := scoredFiles(t, .9, .8)
	tool, factory := newIntentTool(t, newScoringFake(), func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
	canceled := make(chan struct{})
	var scans atomic.Int32
	tool.intentScan = func(ctx context.Context, path string, re *regexp.Regexp, max int, visit func([]string, int) error) error {
		if scans.Add(1) == 1 {
			return grepsearch.Scan(ctx, path, re, max, visit)
		}
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	ordinary := execGrep(t, NewGrepTool(nil, DefaultOutputLimits()), root, map[string]any{"max_results": 1})
	start := time.Now()
	got := execGrep(t, tool, root, map[string]any{"max_results": 1, "intent": "writes"})
	if elapsed := time.Since(start); elapsed > reviewedBudget+reviewedMargin {
		t.Fatalf("Go expansion took %v", elapsed)
	}
	if got != skipNote("timeout")+ordinary {
		t.Fatalf("fallback=%q ordinary=%q", got, ordinary)
	}
	select {
	case <-canceled:
	case <-time.After(reviewedMargin):
		t.Fatal("expanded file scan did not observe cancellation")
	}
	if factory.opens.Load() != 0 {
		t.Fatal("opened classifier despite expansion timeout")
	}
}

func TestIntentBudgetStartsAfterOrdinaryWork(t *testing.T) {
	requireGoFallback(t)
	root := scoredFiles(t, .9, .8)
	tool, _ := newIntentTool(t, newScoringFake(), func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
	entered, release := make(chan struct{}), make(chan struct{})
	var scans atomic.Int32
	tool.intentScan = func(ctx context.Context, path string, re *regexp.Regexp, max int, visit func([]string, int) error) error {
		if scans.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return grepsearch.Scan(ctx, path, re, max, visit)
	}
	done := make(chan string, 1)
	go func() { done <- execGrep(t, tool, root, map[string]any{"max_results": 2, "intent": "writes"}) }()
	<-entered
	select {
	case text := <-done:
		close(release)
		t.Fatalf("budget expired during ordinary work: %q", text)
	case <-time.After(reviewedBudget + 25*time.Millisecond):
	}
	close(release)
	select {
	case text := <-done:
		if !strings.Contains(text, "intent rerank: showing") {
			t.Fatalf("result=%q", text)
		}
	case <-time.After(reviewedBudget + reviewedMargin):
		t.Fatal("did not finish after ordinary scan resumed")
	}
}

func TestIntentExecuteTimeoutsTripBreaker(t *testing.T) {
	requireGoFallback(t)
	root := scoredFiles(t, .9, .8)
	release := make(chan struct{})
	defer close(release)
	backend := &rerankFake{fn: func(context.Context, classify.Request) (*classify.Response, error) {
		<-release
		return nil, errors.New("late")
	}}
	tool, _ := newIntentTool(t, backend, func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
	for i := 0; i < 3; i++ {
		start := time.Now()
		got := execGrep(t, tool, root, map[string]any{"intent": "writes"})
		if elapsed := time.Since(start); elapsed > reviewedBudget+reviewedMargin {
			t.Fatalf("timeout %d took %v", i, elapsed)
		}
		if !strings.HasPrefix(got, skipNote("timeout")) {
			t.Fatalf("call %d = %q", i, got)
		}
	}
	before := backend.calls()
	tool.intentScan = func(context.Context, string, *regexp.Regexp, int, func([]string, int) error) error {
		t.Error("breaker allowed expanded search")
		return errors.New("unexpected expansion")
	}
	got := execGrep(t, tool, root, map[string]any{"intent": "writes"})
	if !strings.HasPrefix(got, skipNote("temporarily disabled after repeated failures")) {
		t.Fatalf("fourth call=%q", got)
	}
	if backend.calls() != before {
		t.Fatal("breaker allowed another classify call")
	}
}

func TestIntentFallbackNeverRerunsMultilineEnrichment(t *testing.T) {
	root := t.TempDir()
	stream := rgMatchJSON(filepath.Join(root, "a.go"), 1, "needle") + rgMatchJSON(filepath.Join(root, "b.go"), 1, "needle")
	rg := installFakeRG(t, []byte(stream))
	tool, _ := newIntentTool(t, failingFake(errors.New("failure")), nil)
	got := execGrepRaw(t, context.Background(), tool, map[string]any{"path": root, "pattern": "needle", "intent": "writes", "multiline": true, "max_results": 2})
	if !strings.HasPrefix(got, skipNote("classifier error")) {
		t.Fatalf("output=%q", got)
	}
	if rg.invocations() != 1 {
		t.Fatalf("fallback spawned %d searches, want 1", rg.invocations())
	}
}

func TestIntentPreCanceledParentHasNoSkipNote(t *testing.T) {
	requireGoFallback(t)
	root := scoredFiles(t, .9, .8)
	tool, factory := newIntentTool(t, newScoringFake(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := execGrepRaw(t, ctx, tool, map[string]any{"path": root, "pattern": "needle", "intent": "writes"})
	if got != "grep timed out after 1 minute; try a more specific pattern or path" {
		t.Fatalf("parent cancel output=%q", got)
	}
	if factory.opens.Load() != 0 {
		t.Fatal("canceled parent opened classifier")
	}
}

func TestIntentExpansionTimeoutsAlsoTripBreaker(t *testing.T) {
	requireGoFallback(t)
	root := scoredFiles(t, .9, .8)
	tool, factory := newIntentTool(t, newScoringFake(), func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
	var scans atomic.Int32
	tool.intentScan = func(ctx context.Context, path string, re *regexp.Regexp, max int, visit func([]string, int) error) error {
		scans.Add(1)
		if filepath.Base(path) == "f1.txt" {
			return grepsearch.Scan(ctx, path, re, max, visit)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	for i := 0; i < 3; i++ {
		got := execGrep(t, tool, root, map[string]any{"intent": "writes", "max_results": 1})
		if !strings.HasPrefix(got, skipNote("timeout")) {
			t.Fatalf("expansion timeout %d=%q", i, got)
		}
	}
	before := scans.Load()
	got := execGrep(t, tool, root, map[string]any{"intent": "writes", "max_results": 1})
	if !strings.HasPrefix(got, skipNote("temporarily disabled after repeated failures")) || scans.Load() != before {
		t.Fatalf("breaker output=%q scans=%d -> %d", got, before, scans.Load())
	}
	if factory.opens.Load() != 0 {
		t.Fatal("expansion timeout opened backend")
	}
}
