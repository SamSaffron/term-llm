package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

// TestMain doubles as a fake llama-server when RERANK_FAKE_SERVER is set.
func TestMain(m *testing.M) {
	if os.Getenv("RERANK_FAKE_SERVER") == "1" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeServer() {
	port := ""
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if f := os.Getenv("RERANK_FAKE_LOG"); f != "" {
		fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fh.WriteString(strings.Join(os.Args[1:], " ") + "\n")
		fh.Close()
	}
	if msg := os.Getenv("RERANK_FAKE_STDERR"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
	if d, err := time.ParseDuration(os.Getenv("RERANK_FAKE_DELAY")); err == nil {
		time.Sleep(d)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/v1/rerank", rerankHandler)
	_ = http.ListenAndServe("127.0.0.1:"+port, mux)
	os.Exit(1)
}

// rerankHandler scores each document by its length.
func rerankHandler(w http.ResponseWriter, r *http.Request) {
	var req rerankRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	type res struct {
		Index int     `json:"index"`
		Score float64 `json:"relevance_score"`
	}
	if d, err := time.ParseDuration(os.Getenv("RERANK_FAKE_RERANK_DELAY")); err == nil {
		time.Sleep(d)
	}
	var out []res
	chars := 0
	for i := len(req.Documents) - 1; i >= 0; i-- { // out of order on purpose
		out = append(out, res{i, float64(len(req.Documents[i]))})
		chars += len(req.Documents[i])
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"results": out, "usage": map[string]int{"prompt_tokens": chars / 4}})
}

func TestHTTPProvider(t *testing.T) {
	var auth, model string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		var req rerankRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		model = req.Model
		r.Body = io.NopCloser(bytes.NewReader(mustJSON(req)))
		rerankHandler(w, r)
	}))
	defer srv.Close()
	p, err := NewProvider(config.RerankConfig{Provider: "http", BaseURL: srv.URL + "/v1", Model: "m", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Rerank(context.Background(), "q", []string{"a", "abc", "ab"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{{1, 3}, {2, 2}, {0, 1}}
	if len(res) != 3 || res[0] != want[0] || res[1] != want[1] || res[2] != want[2] {
		t.Fatalf("results = %v", res)
	}
	if res, err := p.Rerank(context.Background(), "q", []string{"a", "abc", "ab"}, 2); err != nil || len(res) != 2 {
		t.Fatalf("topN: %v %v", res, err)
	}
	if auth != "Bearer secret" || model != "m" {
		t.Fatalf("auth=%q model=%q", auth, model)
	}
	custom, err := NewProvider(config.RerankConfig{Provider: "http", BaseURL: srv.URL, Path: "/custom/rank"})
	if err != nil || custom.(*httpProvider).url != srv.URL+"/custom/rank" {
		t.Fatalf("custom path = %v, %v", custom, err)
	}
}

func TestHTTPProviderErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bad") != "" {
			_, _ = w.Write([]byte(`{"results":[{"index":5,"relevance_score":1}]}`))
			return
		}
		http.Error(w, "nope", 500)
	}))
	defer srv.Close()
	p, _ := NewProvider(config.RerankConfig{Provider: "http", BaseURL: srv.URL + "/rerank"})
	if _, err := p.Rerank(context.Background(), "q", []string{"a"}, 0); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	p, _ = NewProvider(config.RerankConfig{Provider: "http", BaseURL: srv.URL + "/reranking"})
	if _, err := p.Rerank(context.Background(), "q", []string{"a"}, 0); err == nil {
		t.Fatal("expected error")
	}
	if _, err := NewProvider(config.RerankConfig{Provider: "http"}); err == nil {
		t.Fatal("expected missing base_url error")
	}
	if _, err := NewProvider(config.RerankConfig{Provider: "bogus"}); err == nil {
		t.Fatal("expected unknown provider error")
	}
}

func TestRerankURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:1":              "http://h:1/v1/rerank",
		"http://h:1/":             "http://h:1/v1/rerank",
		"http://h:1/v1":           "http://h:1/v1/rerank",
		"http://h:1/v1/rerank":    "http://h:1/v1/rerank",
		"http://h:1/rerank":       "http://h:1/rerank",
		"http://h:1/v1/reranking": "http://h:1/v1/reranking",
	} {
		if got := rerankURL(in); got != want {
			t.Errorf("rerankURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	if defaultTimeout != 20*time.Second || defaultStartupTimeout != 10*time.Minute || defaultIdleTimeout != 10*time.Minute {
		t.Fatal("unexpected default timeouts")
	}
	p, err := NewProvider(config.RerankConfig{Provider: "http", BaseURL: "http://127.0.0.1:1"})
	if err != nil || p.(*httpProvider).client.Timeout != 20*time.Second {
		t.Fatalf("HTTP default timeout: provider=%v err=%v", p, err)
	}
}

func TestDisabled(t *testing.T) {
	p, err := NewProvider(config.RerankConfig{Provider: "none"})
	if p != nil || err != nil {
		t.Fatalf("got %v, %v", p, err)
	}
	t.Setenv("PATH", t.TempDir())
	p, err = NewProvider(config.RerankConfig{})
	if p != nil || err != nil {
		t.Fatalf("auto without binary: %v, %v", p, err)
	}
	if _, err := NewProvider(config.RerankConfig{Provider: "llamacpp"}); err == nil {
		t.Fatal("expected missing binary error")
	}
}

func fakeCfg(t *testing.T, logFile string, idle string) config.RerankConfig {
	t.Helper()
	t.Setenv("RERANK_FAKE_SERVER", "1")
	t.Setenv("RERANK_FAKE_LOG", logFile)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return config.RerankConfig{
		Provider: "llamacpp",
		Model:    "org/model",
		Llamacpp: config.RerankLlamacppConfig{Binary: exe, Args: []string{"-c", "8192", "--extra"}, StartupTimeout: "10s", IdleTimeout: idle},
	}
}

func startCount(t *testing.T, logFile string) int {
	b, _ := os.ReadFile(logFile)
	return strings.Count(string(b), "\n")
}

func TestLlamacppSharedLazyAndIdle(t *testing.T) {
	logFile := t.TempDir() + "/log"
	cfg := fakeCfg(t, logFile, "300ms")
	p1, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := NewProvider(cfg)
	if startCount(t, logFile) != 0 {
		t.Fatal("server started eagerly")
	}
	ctx := context.Background()
	if _, err := p1.Rerank(ctx, "q", []string{"a", "bb"}, 0); err != nil {
		t.Fatal(err)
	}
	res, err := p2.Rerank(ctx, "q", []string{"a", "bb"}, 0)
	if err != nil || len(res) != 2 || res[0] != (Result{1, 2}) {
		t.Fatalf("%v %v", res, err)
	}
	if n := startCount(t, logFile); n != 1 {
		t.Fatalf("starts = %d, want 1", n)
	}
	b, _ := os.ReadFile(logFile)
	for _, want := range []string{"--reranking", "-hf org/model", "-c 32768", "-np 4", "-b 2048", "-ub 2048", "-c 8192", "--host 127.0.0.1", "--extra", "--port"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("args %q missing %q", b, want)
		}
	}
	if strings.Index(string(b), "-c 32768") >= strings.Index(string(b), "-c 8192") {
		t.Fatalf("user flags must follow defaults for overrides: %s", b)
	}
	srv := p1.(*llamacppProvider).srv
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		stopped := srv.cmd == nil
		srv.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle timeout did not stop server")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := p1.Rerank(ctx, "q", []string{"a"}, 0); err != nil {
		t.Fatal(err)
	}
	if n := startCount(t, logFile); n != 2 {
		t.Fatalf("starts after idle = %d, want 2", n)
	}
	Shutdown()
	srv.mu.Lock()
	running := srv.cmd != nil
	srv.mu.Unlock()
	if running {
		t.Fatal("server still running after Shutdown")
	}
}

func TestLlamacppStartupFailure(t *testing.T) {
	t.Setenv("RERANK_FAKE_SERVER", "")
	cfg := config.RerankConfig{Provider: "llamacpp", Model: "x", Llamacpp: config.RerankLlamacppConfig{Binary: "/bin/false", StartupTimeout: "5s"}}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Rerank(context.Background(), "q", []string{"a"}, 0); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("err = %v", err)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestWaitHealthyRetriesOn503(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, `{"error":"Loading model"}`, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := waitHealthy(context.Background(), srv.URL, 5*time.Second, make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestShutdownStopsServer(t *testing.T) {
	cfg := fakeCfg(t, t.TempDir()+"/log", "")
	p, _ := NewProvider(cfg)
	if _, err := p.Rerank(context.Background(), "q", []string{"a"}, 0); err != nil {
		t.Fatal(err)
	}
	srv := p.(*llamacppProvider).srv
	Shutdown()
	Shutdown()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.cmd != nil {
		t.Fatal("server still running after Shutdown")
	}
}

func TestLlamacppWarmingUpFallsBackThenServes(t *testing.T) {
	logFile := t.TempDir() + "/log"
	cfg := fakeCfg(t, logFile, "")
	t.Setenv("RERANK_FAKE_DELAY", "1s")
	cfg.Timeout = "200ms"
	cfg.Llamacpp.Args = append(cfg.Llamacpp.Args, "--warmup-test")
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer Shutdown()
	start := time.Now()
	if _, err := p.Rerank(context.Background(), "q", []string{"a"}, 0); !errors.Is(err, ErrWarmingUp) {
		t.Fatalf("first call err = %v, want ErrWarmingUp", err)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("warming-up call blocked for %v", time.Since(start))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := p.Rerank(context.Background(), "q", []string{"a", "bb"}, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrWarmingUp) || time.Now().After(deadline) {
			t.Fatalf("err = %v", err)
		}
	}
	if n := startCount(t, logFile); n != 1 {
		t.Fatalf("starts = %d, want 1 (warm-up must reuse the in-flight start)", n)
	}
}

func TestLlamacppStartupFailureRememberedWithStderr(t *testing.T) {
	logFile := t.TempDir() + "/log"
	cfg := fakeCfg(t, logFile, "")
	t.Setenv("RERANK_FAKE_STDERR", "boom: model not found")
	cfg.Llamacpp.Args = append(cfg.Llamacpp.Args, "--failure-test")
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer Shutdown()
	_, err = p.Rerank(context.Background(), "q", []string{"a"}, 0)
	if err == nil || !strings.Contains(err.Error(), "boom: model not found") {
		t.Fatalf("err = %v, want stderr tail", err)
	}
	_, err2 := p.Rerank(context.Background(), "q", []string{"a"}, 0)
	if err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("second err = %v, want remembered %v", err2, err)
	}
	if n := startCount(t, logFile); n != 1 {
		t.Fatalf("starts = %d, want 1 during backoff", n)
	}
}

func TestLlamacppTimeoutKeepsBusyThenAdaptsBatch(t *testing.T) {
	cfg := fakeCfg(t, t.TempDir()+"/log", "")
	t.Setenv("RERANK_FAKE_RERANK_DELAY", "600ms")
	cfg.Timeout = "300ms"
	cfg.Llamacpp.Args = append(cfg.Llamacpp.Args, "--busy-test")
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer Shutdown()
	if s := p.(Sizer).MaxDocuments(1200); s != 0 {
		t.Fatalf("MaxDocuments before any measurement = %d, want 0", s)
	}
	docs := make([]string, 40)
	for i := range docs {
		docs[i] = strings.Repeat("x", 1200)
	}
	// Wait out server start-up, then time a request out.
	var firstErr error
	for i := 0; i < 50; i++ {
		_, firstErr = p.Rerank(context.Background(), "q", docs, 5)
		if !errors.Is(firstErr, ErrWarmingUp) {
			break
		}
	}
	if firstErr == nil || !strings.Contains(firstErr.Error(), "finishing in background") {
		t.Fatalf("first err = %v, want timeout", firstErr)
	}
	if _, err := p.Rerank(context.Background(), "q", docs, 5); !errors.Is(err, ErrBusy) {
		t.Fatalf("second err = %v, want ErrBusy while the timed-out request runs", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.(Sizer).MaxDocuments(1200) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("throughput never measured from the background request")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// ~300 tokens/doc at ~12k tokens per 0.6s => 0.75*0.3s fits ~15 docs; just
	// assert it shrank below the batch that timed out and respects the floor.
	if n := p.(Sizer).MaxDocuments(1200); n >= len(docs) || n < minAdaptiveDocuments {
		t.Fatalf("MaxDocuments = %d, want in [%d,%d)", n, minAdaptiveDocuments, len(docs))
	}
}
