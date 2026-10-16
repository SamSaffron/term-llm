package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/rerank"
)

type fakeGrepReranker struct {
	all  bool
	fail bool
	err  error
	docs []string
}

func (f *fakeGrepReranker) Name() string { return "fake" }
func (f *fakeGrepReranker) Rerank(_ context.Context, _ string, docs []string, _ int) ([]rerank.Result, error) {
	f.docs = docs
	if f.err != nil {
		return nil, f.err
	}
	if f.fail {
		return nil, errors.New("offline")
	}
	if f.all {
		out := make([]rerank.Result, len(docs))
		for i := range docs {
			out[i] = rerank.Result{Index: len(docs) - 1 - i, Score: float64(len(docs) - i)}
		}
		return out, nil
	}
	if len(docs) == 1 {
		return []rerank.Result{{Index: 0, Score: 0.9}}, nil
	}
	return []rerank.Result{{Index: len(docs) - 1, Score: 0.9}, {Index: 0, Score: 0.5}}, nil
}

func TestGrepQueryRankingAndFallback(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("requires rg")
	}
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("needle "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeGrepReranker{}
	cfg := &ToolConfig{RerankProvider: f}
	tool := NewGrepTool(nil, DefaultOutputLimits(), cfg)
	args, _ := json.Marshal(GrepArgs{Pattern: "needle", Path: dir, Query: "find a relevant passage", MaxResults: 2})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "Reranked 3 of 3 match blocks (provider fake); showing top 2.") {
		t.Fatalf("unexpected output: %s", out.Content)
	}
	if len(f.docs) != 3 || !strings.Contains(f.docs[0], ":1-1\n") || !strings.Contains(f.docs[0], "needle") {
		t.Fatalf("documents = %q", f.docs)
	}
	first := filepath.Base(strings.SplitN(f.docs[len(f.docs)-1], ":", 2)[0])
	second := filepath.Base(strings.SplitN(f.docs[0], ":", 2)[0])
	if strings.Index(out.Content, first+" (1 match)") < 0 || strings.Index(out.Content, second+" (1 match)") < 0 || strings.Index(out.Content, first+" (1 match)") > strings.Index(out.Content, second+" (1 match)") {
		t.Fatalf("rank order not preserved: %s", out.Content)
	}
	f.fail = true
	out, err = tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "Reranking failed (offline); showing normal grep results") || !strings.Contains(out.Content, "needle") {
		t.Fatalf("fallback: %v %s", err, out.Content)
	}
	f.fail = false
	f.err = context.DeadlineExceeded
	out, err = tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "Reranking failed (context deadline exceeded); showing normal grep results") || !strings.Contains(out.Content, "needle") {
		t.Fatalf("timeout fallback: %v %s", err, out.Content)
	}
	f.err = nil
	cfg.RerankProvider = nil
	cfg.Rerank.Provider = "none"
	out, err = tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "Reranking unavailable") {
		t.Fatalf("disabled fallback: %v %s", err, out.Content)
	}
	filesArgs, _ := json.Marshal(GrepArgs{Pattern: "needle", Path: dir, Query: "intent", FilesWithMatches: true})
	out, err = tool.Execute(context.Background(), filesArgs)
	if err != nil || !strings.Contains(out.Content, "not applied to files_with_matches") {
		t.Fatalf("files fallback: %v %s", err, out.Content)
	}
}

func TestGrepBlockEndLine(t *testing.T) {
	m := GrepMatch{LineNumber: 12, Context: "  11: before\n> 12: match\n  13: after"}
	if got := grepBlockEndLine(m); got != 13 {
		t.Fatalf("end line = %d, want 13", got)
	}
}

func TestGrepRerankDocumentLimit(t *testing.T) {
	if got := NewGrepTool(nil, DefaultOutputLimits()).rerankPool(); got != 64 {
		t.Fatalf("default pool = %d, want 64", got)
	}
	match := GrepMatch{FilePath: "src/file.go", LineNumber: 5, Context: "> 5: " + strings.Repeat("界", 2000)}
	doc := grepRerankDocument(match)
	if got := len([]rune(doc)); got != 1200 {
		t.Fatalf("document chars = %d, want 1200", got)
	}
	if !strings.HasPrefix(doc, "src/file.go:5-5\n> 5: ") || !strings.HasSuffix(doc, "界") {
		t.Fatalf("document header/content or UTF-8 truncated incorrectly: %q", doc[:min(len(doc), 100)])
	}
	if len([]rune(grepRerankDocument(GrepMatch{FilePath: "x", Context: "short"}))) >= 1200 {
		t.Fatal("short document unexpectedly padded")
	}
}

func TestGrepBM25Prefilter(t *testing.T) {
	matches := []GrepMatch{{FilePath: "x", Context: "plain needle"}, {FilePath: "y", Context: "database needle index"}, {FilePath: "z", Context: "needle"}}
	got := prefilterGrepMatches("database index", matches, 1)
	if len(got) != 1 || got[0].FilePath != "y" {
		t.Fatalf("prefilter = %+v", got)
	}
	got = prefilterGrepMatches("unseen", matches, 2)
	if got[0].FilePath != "x" || got[1].FilePath != "y" {
		t.Fatalf("tie order = %+v", got)
	}
}

func TestGrepQueryPrefiltersBeforeReranking(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("requires rg")
	}
	dir := t.TempDir()
	for _, item := range []struct{ name, content string }{
		{"a.txt", "needle ordinary\n"},
		{"b.txt", "needle ordinary\n"},
		{"c.txt", "needle relevant database index\n"},
	} {
		if err := os.WriteFile(filepath.Join(dir, item.name), []byte(item.content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeGrepReranker{}
	tool := NewGrepTool(nil, DefaultOutputLimits(), &ToolConfig{Rerank: config.RerankConfig{Pool: 1}, RerankProvider: f})
	args, _ := json.Marshal(GrepArgs{Pattern: "needle", Path: dir, Query: "database index", MaxResults: 1})
	out, err := tool.Execute(context.Background(), args)
	if err != nil || len(f.docs) != 1 || !strings.Contains(f.docs[0], "c.txt") || !strings.Contains(out.Content, "Reranked 1 of 3") {
		t.Fatalf("prefilter failed: output=%s docs=%q err=%v", out.Content, f.docs, err)
	}
}

func TestGrepQueryDefaultsToTenAndWarmingUpNote(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("requires rg")
	}
	dir := t.TempDir()
	for i := 0; i < 15; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("needle\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeGrepReranker{all: true}
	tool := NewGrepTool(nil, DefaultOutputLimits(), &ToolConfig{RerankProvider: f})
	args, _ := json.Marshal(GrepArgs{Pattern: "needle", Path: dir, Query: "intent"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "Reranked 15 of 15 match blocks (provider fake); showing top 10.") {
		t.Fatalf("default shown: %v %s", err, out.Content)
	}
	f.all, f.err = false, rerank.ErrWarmingUp
	out, err = tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "Reranker starting (first use may download the model); showing normal grep results.") {
		t.Fatalf("warming note: %v %s", err, out.Content)
	}
}

type sizedFakeReranker struct {
	fakeGrepReranker
	max int
}

func (s *sizedFakeReranker) MaxDocuments(int) int { return s.max }

func TestGrepQueryUsesAdaptiveBatchAndBusyNote(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("requires rg")
	}
	dir := t.TempDir()
	for i := 0; i < 30; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("needle\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := &sizedFakeReranker{fakeGrepReranker: fakeGrepReranker{all: true}, max: 7}
	tool := NewGrepTool(nil, DefaultOutputLimits(), &ToolConfig{RerankProvider: f})
	args, _ := json.Marshal(GrepArgs{Pattern: "needle", Path: dir, Query: "intent"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil || len(f.docs) != 7 || !strings.Contains(out.Content, "Reranked 7 of 30 match blocks") {
		t.Fatalf("adaptive batch: docs=%d err=%v %s", len(f.docs), err, out.Content)
	}
	f.err = rerank.ErrBusy
	out, err = tool.Execute(context.Background(), args)
	if err != nil || !strings.Contains(out.Content, "Reranker busy with a previous search; showing normal grep results.") {
		t.Fatalf("busy note: %v %s", err, out.Content)
	}
}
