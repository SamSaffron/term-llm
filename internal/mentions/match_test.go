package mentions

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func testSnapshot(paths ...string) *Snapshot {
	s := &Snapshot{}
	for _, path := range paths {
		s.Candidates = append(s.Candidates, makeCandidate(path, KindFile))
	}
	return s
}

func TestSearchWholeRelativePathAndRanking(t *testing.T) {
	s := testSnapshot("docs/types-notes.md", "internal/llm/types.go", "types.go", "internal/tools/read.go")
	matches := s.Search(context.Background(), "illmtyp", 10)
	if len(matches) == 0 || s.Candidates[matches[0].Candidate].Path != "internal/llm/types.go" {
		t.Fatalf("whole-path search = %#v", matches)
	}

	matches = s.Search(context.Background(), "types.go", 10)
	if got := s.Candidates[matches[0].Candidate].Path; got != "types.go" {
		t.Fatalf("exact basename ranked %q first", got)
	}
}

func TestSearchReturnsTrueTopKWithTies(t *testing.T) {
	paths := make([]string, 0, 600)
	for i := 0; i < 200; i++ {
		paths = append(paths,
			fmt.Sprintf("internal/xy/%03d/a.go", i),
			fmt.Sprintf("ab/internal/%03d/ab.go", i),
			fmt.Sprintf("src/%03d/abcd.go", i),
		)
	}
	s := testSnapshot(paths...)
	for _, query := range []string{"a", "ab", "abc", "go"} {
		full := s.Search(context.Background(), query, len(s.Candidates))
		for _, limit := range []int{1, 3, 4, 8, 50} {
			got := s.Search(context.Background(), query, limit)
			want := full[:min(limit, len(full))]
			if !reflect.DeepEqual(matchCandidateIDs(got), matchCandidateIDs(want)) {
				t.Fatalf("query=%q limit=%d candidates=%v, want %v", query, limit, matchCandidateIDs(got), matchCandidateIDs(want))
			}
		}
	}
}

func matchCandidateIDs(matches []Match) []int {
	ids := make([]int, len(matches))
	for i, match := range matches {
		ids[i] = match.Candidate
	}
	return ids
}

func TestSearchTopKAndPeriodicCancellation(t *testing.T) {
	paths := make([]string, 2000)
	for i := range paths {
		paths[i] = fmt.Sprintf("dir/file-%04d.go", i)
	}
	s := testSnapshot(paths...)
	if got := len(s.Search(context.Background(), "file", 7)); got != 7 {
		t.Fatalf("top-k len = %d", got)
	}
	ctx := newCancelAfterChecksContext(3)
	if got := s.Search(ctx, "file", 7); got != nil {
		t.Fatalf("cancelled search = %#v", got)
	}
	if ctx.checks < 3 {
		t.Fatalf("search checked cancellation only %d times", ctx.checks)
	}
}

type cancelAfterChecksContext struct {
	cancelAt int
	checks   int
	done     chan struct{}
}

func newCancelAfterChecksContext(cancelAt int) *cancelAfterChecksContext {
	return &cancelAfterChecksContext{cancelAt: cancelAt, done: make(chan struct{})}
}

func (c *cancelAfterChecksContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecksContext) Done() <-chan struct{} {
	c.checks++
	if c.checks == c.cancelAt {
		close(c.done)
	}
	return c.done
}
func (c *cancelAfterChecksContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (c *cancelAfterChecksContext) Value(any) any { return nil }

func BenchmarkSearch10KSelectiveQuery(b *testing.B)  { benchmarkSearch(b, 10_000, "pkgcomp42") }
func BenchmarkSearch100KSelectiveQuery(b *testing.B) { benchmarkSearch(b, 100_000, "pkgcomp42") }
func BenchmarkSearch10KBroadQuery(b *testing.B)      { benchmarkSearch(b, 10_000, "comp") }
func BenchmarkSearch100KBroadQuery(b *testing.B)     { benchmarkSearch(b, 100_000, "comp") }
func BenchmarkSearch100KShortQuery(b *testing.B)     { benchmarkSearch(b, 100_000, "c") }

func benchmarkSearch(b *testing.B, count int, query string) {
	paths := make([]string, count)
	for i := range paths {
		paths[i] = fmt.Sprintf("internal/package-%04d/component/file-%06d.go", i%1000, i)
	}
	s := testSnapshot(paths...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Search(context.Background(), query, 50)
	}
}
