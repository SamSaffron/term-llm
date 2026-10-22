package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/backends"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/greprerank"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

const (
	rerankTestKey    = "sk-config-secret-key"
	rerankTestSecret = "sk-live-SECRET-from-provider"
)

// ---- fake classify backend and factory seam ------------------------------

type rerankFake struct {
	mu   sync.Mutex
	reqs []classify.Request
	fn   func(ctx context.Context, req classify.Request) (*classify.Response, error)
}

func (f *rerankFake) Classify(ctx context.Context, req classify.Request) (*classify.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return f.fn(ctx, req)
}

func (f *rerankFake) ListModels(context.Context) (*classify.ModelsResponse, error) { return nil, nil }

func (f *rerankFake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *rerankFake) requests() []classify.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]classify.Request(nil), f.reqs...)
}

var (
	rerankHeaderRE = regexp.MustCompile(`(?m)^(\d+)\. `)
	rerankScoreRE  = regexp.MustCompile(`score=([0-9.]+)`)
)

// scoreFromBlocks answers each question with the first "score=X" token found
// in the corresponding result block of the request state.
func scoreFromBlocks(_ context.Context, req classify.Request) (*classify.Response, error) {
	var state map[string]string
	if err := json.Unmarshal(req.State, &state); err != nil {
		return nil, err
	}
	results := state["results"]
	idx := rerankHeaderRE.FindAllStringSubmatchIndex(results, -1)
	answers := map[string]classify.Answer{}
	for i, loc := range idx {
		end := len(results)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		n := results[loc[2]:loc[3]]
		v := 0.0
		if m := rerankScoreRE.FindStringSubmatch(results[loc[1]:end]); m != nil {
			v, _ = strconv.ParseFloat(m[1], 64)
		}
		answers["r"+n] = classify.Answer{Type: classify.TypeNoul, Noul: &v}
	}
	return &classify.Response{Answers: answers}, nil
}

func newScoringFake() *rerankFake { return &rerankFake{fn: scoreFromBlocks} }

func failingFake(err error) *rerankFake {
	return &rerankFake{fn: func(context.Context, classify.Request) (*classify.Response, error) { return nil, err }}
}

type rerankFactory struct {
	opens atomic.Int32
	mu    sync.Mutex
	conns []backends.Connection
	err   error
	block chan struct{} // when non-nil, open blocks until closed
	b     classify.Backend
}

func (f *rerankFactory) connections() []backends.Connection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]backends.Connection(nil), f.conns...)
}

// installRerankFactory replaces the seam. It must be called before
// NewGrepReranker, which captures the factory at construction.
func installRerankFactory(t *testing.T, b classify.Backend) *rerankFactory {
	t.Helper()
	f := &rerankFactory{b: b}
	orig := newGrepRerankBackend
	newGrepRerankBackend = func(conn backends.Connection) (classify.Backend, error) {
		f.opens.Add(1)
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		block, err := f.block, f.err
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		if err != nil {
			return nil, err
		}
		return f.b, nil
	}
	t.Cleanup(func() { newGrepRerankBackend = orig })
	return f
}

// ---- config helpers -------------------------------------------------------

// rerankTestConfig loads real defaults in an isolated config dir with no
// classification credentials in the environment.
func rerankTestConfig(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg-data"))
	for _, name := range []string{"TYPESAFE_API_KEY", "OPENAI_API_KEY", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_AUTH_TOKEN", "CLOUDFLARE_ACCOUNT_ID"} {
		t.Setenv(name, "")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Classify.Providers == nil {
		cfg.Classify.Providers = map[string]config.ClassifyProviderConfig{}
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func withTypesafeKey(cfg *config.Config) {
	p := cfg.Classify.Providers["typesafe"]
	p.APIKey = rerankTestKey
	cfg.Classify.Providers["typesafe"] = p
}

func chain(fns ...func(*config.Config)) func(*config.Config) {
	return func(cfg *config.Config) {
		for _, fn := range fns {
			fn(cfg)
		}
	}
}

func hasIntent(spec core.ToolSpec) bool {
	props, _ := spec.Schema["properties"].(map[string]interface{})
	_, ok := props["intent"]
	return ok
}

// newIntentTool builds a grep tool whose reranker uses the fake backend.
func newIntentTool(t *testing.T, b classify.Backend, mutate func(*config.Config)) (*GrepTool, *rerankFactory) {
	t.Helper()
	cfg := rerankTestConfig(t, chain(withTypesafeKey, func(c *config.Config) {
		if mutate != nil {
			mutate(c)
		}
	}))
	f := installRerankFactory(t, b)
	tool := NewGrepTool(nil, DefaultOutputLimits()).WithReranker(NewGrepReranker(cfg))
	if tool.reranker == nil {
		t.Fatal("reranker unexpectedly unavailable")
	}
	return tool, f
}

// ---- fixtures -------------------------------------------------------------

type rerankFile struct{ name, content string }

// writeRerankFiles creates files with distinct mtimes; earlier entries are
// newer, which is the order the Go fallback (and mtime sort) searches them.
func writeRerankFiles(t *testing.T, files ...rerankFile) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.content), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(-time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func scoredFiles(t *testing.T, scores ...float64) string {
	t.Helper()
	files := make([]rerankFile, len(scores))
	for i, s := range scores {
		files[i] = rerankFile{fmt.Sprintf("f%d.txt", i+1), fmt.Sprintf("needle score=%v\n", s)}
	}
	return writeRerankFiles(t, files...)
}

func execGrepRaw(t *testing.T, ctx context.Context, tool *GrepTool, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(ctx, raw)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out.Content
}

func execGrep(t *testing.T, tool *GrepTool, dir string, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"pattern": "needle", "path": dir, "context_lines": 1}
	for k, v := range extra {
		args[k] = v
	}
	return execGrepRaw(t, context.Background(), tool, args)
}

// stripDir makes output independent of the temp directory.
func stripDir(out, dir string) string {
	return strings.ReplaceAll(out, dir+string(filepath.Separator), "")
}

func requireGoFallback(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func order(t *testing.T, out string, names ...string) {
	t.Helper()
	prev := -1
	for _, n := range names {
		i := strings.Index(out, string(filepath.Separator)+n+":")
		if i < 0 {
			t.Fatalf("%s missing from output:\n%s", n, out)
		}
		if i <= prev {
			t.Fatalf("%s out of order in output:\n%s", n, out)
		}
		prev = i
	}
}

func scoresIn(out string) []string {
	var got []string
	for _, m := range rerankScoreRE.FindAllStringSubmatch(out, -1) {
		got = append(got, m[1])
	}
	return got
}

func rankedNote(shown, kept, culled, total int) string {
	return fmt.Sprintf("[intent rerank: showing %d of %d relevant matches (%d of %d candidates below 0.50 dropped); candidate search capped at 150 — broaden pattern/path or omit intent for unfiltered results]\n", shown, kept, culled, total)
}

func skipNote(category string) string {
	return "[intent rerank skipped (" + category + "); showing unranked results]\n"
}

// ---- schema availability --------------------------------------------------

func TestGrepRerankAvailability(t *testing.T) {
	setProvider := func(name string, p config.ClassifyProviderConfig) func(*config.Config) {
		return func(c *config.Config) { c.Classify.Providers[name] = p }
	}
	env := func(kv ...string) func(*testing.T) {
		return func(t *testing.T) {
			for i := 0; i < len(kv); i += 2 {
				t.Setenv(kv[i], kv[i+1])
			}
		}
	}
	rerank := func(f func(*config.GrepRerankConfig)) func(*config.Config) {
		return func(c *config.Config) { f(&c.Tools.GrepRerank) }
	}
	tests := []struct {
		name   string
		setup  func(*testing.T)
		mutate func(*config.Config)
		want   bool
	}{
		{name: "stock config without key", want: false},
		{name: "env key on default endpoint", setup: env("TYPESAFE_API_KEY", "env-key"), want: true},
		{name: "env key whitespace only", setup: env("TYPESAFE_API_KEY", "   "), want: false},
		{name: "explicit api_key", mutate: withTypesafeKey, want: true},
		{name: "default endpoint with trailing slash and env key", setup: env("TYPESAFE_API_KEY", "env-key"),
			mutate: setProvider("typesafe", config.ClassifyProviderConfig{BaseURL: config.DefaultTypeSafeBaseURL + "/"}), want: true},
		{name: "custom endpoint with only env key", setup: env("TYPESAFE_API_KEY", "env-key"),
			mutate: setProvider("typesafe", config.ClassifyProviderConfig{BaseURL: "https://classify.example.invalid/v1"}), want: false},
		{name: "custom endpoint with explicit key", setup: env("TYPESAFE_API_KEY", "env-key"),
			mutate: setProvider("typesafe", config.ClassifyProviderConfig{BaseURL: "https://classify.example.invalid/v1", APIKey: "own-key"}), want: true},
		{name: "disabled", setup: env("TYPESAFE_API_KEY", "env-key"),
			mutate: rerank(func(c *config.GrepRerankConfig) { c.Enabled = false }), want: false},
		{name: "unknown provider", setup: env("TYPESAFE_API_KEY", "env-key"),
			mutate: rerank(func(c *config.GrepRerankConfig) { c.Provider = "nope" }), want: false},
		{name: "unknown default provider", setup: env("TYPESAFE_API_KEY", "env-key"),
			mutate: func(c *config.Config) { c.Classify.DefaultProvider = "nope" }, want: false},
		{name: "unsupported provider type", mutate: setProvider("weird", config.ClassifyProviderConfig{Type: "bogus", APIKey: "k"}), want: false},
		{name: "named provider selected explicitly",
			mutate: chain(setProvider("mine", config.ClassifyProviderConfig{Type: config.ClassifyProviderOpenAI, APIKey: "k"}),
				rerank(func(c *config.GrepRerankConfig) { c.Provider = "mine" })), want: true},
		{name: "classify default provider used", setup: env("OPENAI_API_KEY", "oa"),
			mutate: func(c *config.Config) { c.Classify.DefaultProvider = "openai" }, want: true},
		{name: "builtin openai selected by name", setup: env("OPENAI_API_KEY", "oa"),
			mutate: rerank(func(c *config.GrepRerankConfig) { c.Provider = "openai" }), want: true},
		// No provider scanning: other usable providers must never be adopted.
		{name: "other providers configured but selected one is not", setup: env("OPENAI_API_KEY", "oa", "CLOUDFLARE_API_TOKEN", "cf", "CLOUDFLARE_ACCOUNT_ID", "acct"),
			mutate: setProvider("mine", config.ClassifyProviderConfig{Type: config.ClassifyProviderOpenAI, APIKey: "k"}), want: false},
		{name: "explicit provider has no key while default has", setup: env("TYPESAFE_API_KEY", "env-key"),
			mutate: rerank(func(c *config.GrepRerankConfig) { c.Provider = "openai" }), want: false},
		{name: "cloudflare token without account id", setup: env("CLOUDFLARE_API_TOKEN", "cf"),
			mutate: rerank(func(c *config.GrepRerankConfig) { c.Provider = "cloudflare" }), want: false},
		{name: "cloudflare account id without token", setup: env("CLOUDFLARE_ACCOUNT_ID", "acct"),
			mutate: rerank(func(c *config.GrepRerankConfig) { c.Provider = "cloudflare" }), want: false},
		{name: "cloudflare token and account id", setup: env("CLOUDFLARE_API_TOKEN", "cf", "CLOUDFLARE_ACCOUNT_ID", "acct"),
			mutate: rerank(func(c *config.GrepRerankConfig) { c.Provider = "cloudflare" }), want: true},
		{name: "invalid threshold", mutate: chain(withTypesafeKey, rerank(func(c *config.GrepRerankConfig) { c.Threshold = 1.5 })), want: false},
		{name: "NaN threshold", mutate: chain(withTypesafeKey, rerank(func(c *config.GrepRerankConfig) { c.Threshold = math.NaN() })), want: false},
		{name: "zero timeout", mutate: chain(withTypesafeKey, rerank(func(c *config.GrepRerankConfig) { c.TimeoutMS = 0 })), want: false},
		{name: "excessive timeout", mutate: chain(withTypesafeKey, rerank(func(c *config.GrepRerankConfig) { c.TimeoutMS = 10001 })), want: false},
		{name: "too few candidates", mutate: chain(withTypesafeKey, rerank(func(c *config.GrepRerankConfig) { c.MaxCandidates = 1 })), want: false},
		{name: "too many candidates", mutate: chain(withTypesafeKey, rerank(func(c *config.GrepRerankConfig) { c.MaxCandidates = 501 })), want: false},
		{name: "boundary settings are valid", mutate: chain(withTypesafeKey, rerank(func(c *config.GrepRerankConfig) {
			c.Threshold, c.TimeoutMS, c.MaxCandidates = 0, 10000, 2
		})), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := rerankTestConfig(t, nil)
			if tt.setup != nil {
				tt.setup(t)
			}
			if tt.mutate != nil {
				tt.mutate(cfg)
			}
			f := installRerankFactory(t, newScoringFake())
			r := NewGrepReranker(cfg)
			if (r != nil) != tt.want {
				t.Fatalf("NewGrepReranker available = %v, want %v", r != nil, tt.want)
			}
			spec := NewGrepTool(nil, DefaultOutputLimits()).WithReranker(r).Spec()
			if hasIntent(spec) != tt.want {
				t.Fatalf("intent in schema = %v, want %v", hasIntent(spec), tt.want)
			}
			if tt.want != strings.Contains(spec.Description, "intent") {
				t.Fatalf("description/schema disagree: %q", spec.Description)
			}
			if n := f.opens.Load(); n != 0 {
				t.Fatalf("backend opened %d times at construction", n)
			}
		})
	}
}

func TestGrepRerankNilConfig(t *testing.T) {
	if r := NewGrepReranker(nil); r != nil {
		t.Fatal("NewGrepReranker(nil) must be nil")
	}
	want, _ := json.Marshal(NewGrepTool(nil, DefaultOutputLimits()).Spec())
	got, _ := json.Marshal(NewGrepTool(nil, DefaultOutputLimits()).WithReranker(nil).Spec())
	if string(got) != string(want) {
		t.Fatal("nil reranker changed spec")
	}
}

// Construction must be cheap: deferred credentials ($(...)) are not run until
// an intent call actually needs the backend.
func TestGrepRerankConstructionDoesNotResolveCredentials(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cfg := rerankTestConfig(t, func(c *config.Config) {
		p := c.Classify.Providers["typesafe"]
		p.APIKey = "$(touch " + marker + "; echo k)"
		c.Classify.Providers["typesafe"] = p
	})
	f := installRerankFactory(t, newScoringFake())
	if NewGrepReranker(cfg) == nil {
		t.Fatal("deferred key should count as configured")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("credential command ran during construction")
	}
	if f.opens.Load() != 0 {
		t.Fatal("factory ran during construction")
	}
}

func TestGrepRerankStaticDescriptorsIgnoreEnvironment(t *testing.T) {
	cfg := rerankTestConfig(t, withTypesafeKey)
	_ = cfg
	t.Setenv("TYPESAFE_API_KEY", "env-key")
	t.Setenv("OPENAI_API_KEY", "env-key")
	installRerankFactory(t, newScoringFake())

	spec, ok := ToolSpec(GrepToolName)
	if !ok {
		t.Fatal("grep spec missing")
	}
	if hasIntent(spec) || strings.Contains(spec.Description, "intent") {
		t.Fatalf("static ToolSpec exposes intent: %q", spec.Description)
	}
	want := NewGrepTool(nil, DefaultOutputLimits()).Spec().Description
	found := false
	for _, d := range BuiltinToolDescriptors() {
		if d.Name == GrepToolName {
			found = true
			if d.Description != want {
				t.Fatalf("descriptor description = %q, want %q", d.Description, want)
			}
		}
	}
	if !found {
		t.Fatal("grep descriptor missing")
	}
}

func TestGrepRerankRegistryAndSetLimitsPreserveReranker(t *testing.T) {
	cfg := rerankTestConfig(t, withTypesafeKey)
	f := installRerankFactory(t, newScoringFake())
	reg, err := NewLocalToolRegistry(&ToolConfig{Enabled: []string{GrepToolName}}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	grepSpec := func() core.ToolSpec {
		tool, ok := reg.Get(GrepToolName)
		if !ok {
			t.Fatal("grep tool not registered")
		}
		return tool.Spec()
	}
	if !hasIntent(grepSpec()) {
		t.Fatal("registry grep lacks intent when configured")
	}
	before, _ := reg.Get(GrepToolName)
	limits := DefaultOutputLimits()
	limits.MaxResults = 7
	reg.SetLimits(limits)
	after, _ := reg.Get(GrepToolName)
	if before == after {
		t.Fatal("SetLimits should have recreated grep (test premise)")
	}
	if !hasIntent(grepSpec()) {
		t.Fatal("SetLimits dropped the reranker")
	}
	if after.(*GrepTool).reranker != before.(*GrepTool).reranker {
		t.Fatal("SetLimits must keep the registry-owned reranker instance")
	}
	if f.opens.Load() != 0 {
		t.Fatal("registry construction opened the backend")
	}

	plain, err := NewLocalToolRegistry(&ToolConfig{Enabled: []string{GrepToolName}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain.SetLimits(limits)
	tool, _ := plain.Get(GrepToolName)
	if hasIntent(tool.Spec()) {
		t.Fatal("unconfigured registry exposes intent after SetLimits")
	}
}

func TestGrepRerankPreviewUnchanged(t *testing.T) {
	args := json.RawMessage(`{"pattern":"needle","path":"x","intent":"find writes"}`)
	plain := NewGrepTool(nil, DefaultOutputLimits()).Preview(args)
	tool, _ := newIntentTool(t, newScoringFake(), nil)
	if got := tool.Preview(args); !strings.HasPrefix(got, "/needle/ in x") {
		t.Fatalf("preview = %q (unconfigured %q)", got, plain)
	}
}

// ---- no-intent behaviour --------------------------------------------------

func TestGrepRerankConfiguredWithoutIntentIsIdentical(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.9, 0.1, 0.5)
	fake := newScoringFake()
	configured, f := newIntentTool(t, fake, nil)
	plain := NewGrepTool(nil, DefaultOutputLimits())

	for _, extra := range []map[string]any{
		nil,
		{"intent": ""},
		{"intent": "   \t"},
		{"max_results": 2},
		{"max_results": 400}, // above max_candidates: intent inactive
		{"max_results": 400, "intent": "find writes"},
		{"intent": "find writes", "files_with_matches": true},
	} {
		base := map[string]any{}
		for k, v := range extra {
			if k != "intent" {
				base[k] = v
			}
		}
		want := execGrep(t, plain, dir, base)
		if got := execGrep(t, configured, dir, extra); got != want {
			t.Fatalf("extra %v:\n got %q\nwant %q", extra, got, want)
		}
	}
	if fake.calls() != 0 || f.opens.Load() != 0 {
		t.Fatalf("backend used without active intent: calls=%d opens=%d", fake.calls(), f.opens.Load())
	}
}

// Fake rg that records its arguments and invocation count and then replays a
// canned ripgrep JSON stream. It uses only shell builtins.
type fakeRG struct{ args, calls string }

func installFakeRG(t *testing.T, jsonOut []byte) fakeRG {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script rg is not supported on Windows")
	}
	dir := t.TempDir()
	rg := fakeRG{args: filepath.Join(dir, "args"), calls: filepath.Join(dir, "calls")}
	data := filepath.Join(dir, "data.jsonl")
	if err := os.WriteFile(data, jsonOut, 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%s'\necho x >> '%s'\nwhile IFS= read -r line || [ -n \"$line\" ]; do printf '%%s\\n' \"$line\"; done < '%s'\n", rg.args, rg.calls, data)
	if err := os.WriteFile(filepath.Join(dir, "rg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return rg
}

func (r fakeRG) argv(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.args)
	if err != nil {
		t.Fatalf("fake rg was not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func (r fakeRG) invocations() int {
	data, _ := os.ReadFile(r.calls)
	return strings.Count(string(data), "x")
}

func argAfter(t *testing.T, argv []string, flag string) string {
	t.Helper()
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatalf("flag %s missing from %v", flag, argv)
	return ""
}

func TestGrepRerankNoIntentRipgrepArgsAndOutputIdentical(t *testing.T) {
	rg := installFakeRG(t, buildSyntheticRipgrepJSON(5))
	fake := newScoringFake()
	configured, f := newIntentTool(t, fake, nil)
	plain := NewGrepTool(nil, DefaultOutputLimits())
	dir := t.TempDir()

	for _, extra := range []map[string]any{nil, {"max_results": 7}, {"intent": " "}, {"max_results": 7, "context_lines": 3}, {"max_results": 999, "intent": "x"}} {
		args := map[string]any{"pattern": "needle", "path": dir}
		for k, v := range extra {
			args[k] = v
		}
		plainArgs := map[string]any{}
		for k, v := range args {
			if k != "intent" {
				plainArgs[k] = v
			}
		}
		want := execGrepRaw(t, context.Background(), plain, plainArgs)
		wantArgv := rg.argv(t)
		got := execGrepRaw(t, context.Background(), configured, args)
		gotArgv := rg.argv(t)
		if strings.Join(gotArgv, "\x00") != strings.Join(wantArgv, "\x00") {
			t.Fatalf("extra %v: rg args differ\n got %v\nwant %v", extra, gotArgv, wantArgv)
		}
		if got != want {
			t.Fatalf("extra %v: output differs\n got %q\nwant %q", extra, got, want)
		}
		wantCap := strconv.Itoa(DefaultOutputLimits().MaxResults)
		if v, ok := extra["max_results"]; ok {
			wantCap = strconv.Itoa(v.(int))
		}
		if c := argAfter(t, gotArgv, "--max-count"); c != wantCap {
			t.Fatalf("extra %v: --max-count = %s, want %s", extra, c, wantCap)
		}
		if !strings.Contains(got, "needle 0000") {
			t.Fatalf("expected synthetic matches in output:\n%s", got)
		}
	}
	if fake.calls() != 0 || f.opens.Load() != 0 {
		t.Fatalf("backend used: calls=%d opens=%d", fake.calls(), f.opens.Load())
	}
}

// ---- ranking: actual rg, fake rg, and Go fallback -------------------------

type rankScenario struct {
	name   string
	scores []float64
	extra  map[string]any
	want   []string // file names in expected output order
	note   string
	// wantScores are the score tokens expected in rendered order.
	wantScores []string
}

func rankScenarios() []rankScenario {
	return []rankScenario{
		{name: "order ties and threshold equality", scores: []float64{0.2, 0.9, 0.5, 0.9, 0.7},
			want: []string{"f2.txt", "f4.txt", "f5.txt", "f3.txt"}, note: rankedNote(4, 4, 1, 5), wantScores: []string{"0.9", "0.9", "0.7", "0.5"}},
		{name: "all below threshold keeps top three", scores: []float64{0.1, 0.3, 0.2, 0.05},
			want: []string{"f2.txt", "f3.txt", "f1.txt"}, note: "[intent rerank: no match scored >= 0.50; showing top 3 by relevance — consider a different pattern]\n"},
		{name: "max_results trims ranked list", scores: []float64{0.9, 0.8, 0.7, 0.6, 0.1}, extra: map[string]any{"max_results": 2},
			want: []string{"f1.txt", "f2.txt"}, note: rankedNote(2, 4, 1, 5)},
		{name: "all relevant stable order", scores: []float64{0.6, 0.6, 0.6}, want: []string{"f1.txt", "f2.txt", "f3.txt"}, note: rankedNote(3, 3, 0, 3)},
	}
}

func runRankScenario(t *testing.T, sc rankScenario, tool *GrepTool, fake *rerankFake) {
	t.Helper()
	dir := scoredFiles(t, sc.scores...)
	out := stripDir(execGrep(t, tool, dir, mergeIntent(sc.extra)), dir)
	if !strings.HasPrefix(out, sc.note) {
		t.Fatalf("note mismatch:\n got %q\nwant prefix %q", out, sc.note)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, ".txt:") {
			names = append(names, strings.TrimSuffix(line, ":"))
		}
	}
	if strings.Join(names, ",") != strings.Join(sc.want, ",") {
		t.Fatalf("order = %v, want %v\n%s", names, sc.want, out)
	}
	if sc.wantScores != nil {
		if got := scoresIn(out); strings.Join(got, ",") != strings.Join(sc.wantScores, ",") {
			t.Fatalf("scores = %v, want %v", got, sc.wantScores)
		}
	}
	if strings.Contains(out, " matches in ") || strings.Contains(out, " match in ") {
		t.Fatalf("ranked output must not use the regrouping formatter:\n%s", out)
	}
	if n := fake.calls(); n != 1 {
		t.Fatalf("backend calls = %d, want 1", n)
	}
}

func mergeIntent(extra map[string]any) map[string]any {
	m := map[string]any{"intent": "find the writes"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestGrepRerankRanksGoFallback(t *testing.T) {
	for _, sc := range rankScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			requireGoFallback(t)
			fake := newScoringFake()
			tool, _ := newIntentTool(t, fake, nil)
			runRankScenario(t, sc, tool, fake)
		})
	}
}

func TestGrepRerankRanksWithRealRipgrep(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("ripgrep not available")
	}
	for _, sc := range rankScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			fake := newScoringFake()
			tool, _ := newIntentTool(t, fake, nil)
			// rg walks the tree in no guaranteed order, so make candidate
			// order irrelevant: these scenarios rely on a total order except
			// for ties, which are only asserted on the Go path.
			if strings.Contains(sc.name, "ties") || strings.Contains(sc.name, "stable") {
				t.Skip("tie order depends on ripgrep traversal order")
			}
			runRankScenario(t, sc, tool, fake)
		})
	}
}

func rgMatchJSON(path string, line int, text string) string {
	p, _ := json.Marshal(path)
	l, _ := json.Marshal(text + "\n")
	return fmt.Sprintf(`{"type":"match","data":{"path":{"text":%s},"lines":{"text":%s},"line_number":%d,"absolute_offset":0,"submatches":[]}}`+"\n"+
		`{"type":"end","data":{"path":{"text":%s}}}`+"\n", p, l, line, p)
}

func TestGrepRerankRanksWithFakeRipgrepAndCapsSearch(t *testing.T) {
	dir := t.TempDir()
	var stream strings.Builder
	scores := []float64{0.2, 0.9, 0.5, 0.9, 0.7}
	for i, s := range scores {
		stream.WriteString(rgMatchJSON(filepath.Join(dir, fmt.Sprintf("f%d.txt", i+1)), 1, fmt.Sprintf("needle score=%v", s)))
	}
	rg := installFakeRG(t, []byte(stream.String()))
	fake := newScoringFake()
	tool, _ := newIntentTool(t, fake, nil)

	out := execGrepRaw(t, context.Background(), tool, map[string]any{"pattern": "needle", "path": dir, "max_results": 3, "intent": "writes"})
	if !strings.HasPrefix(out, rankedNote(3, 4, 1, 5)) {
		t.Fatalf("unexpected note:\n%s", out)
	}
	order(t, out, "f2.txt", "f4.txt", "f5.txt")
	if strings.Contains(out, "f3.txt") || strings.Contains(out, "f1.txt") {
		t.Fatalf("unexpected files in output:\n%s", out)
	}
	argv := rg.argv(t)
	if got := argAfter(t, argv, "--max-count"); got != "150" {
		t.Fatalf("--max-count = %s, want candidate cap 150 (args %v)", got, argv)
	}
	if rg.invocations() != 1 {
		t.Fatalf("rg ran %d times, want exactly 1 (no enrich re-run after rerank)", rg.invocations())
	}
}

func TestGrepRerankFakeRipgrepFallbackUsesNormalPathAndCap(t *testing.T) {
	dir := t.TempDir()
	var stream strings.Builder
	for i := 0; i < 5; i++ {
		stream.WriteString(rgMatchJSON(filepath.Join(dir, fmt.Sprintf("f%d.txt", i+1)), 1, "needle"))
	}
	rg := installFakeRG(t, []byte(stream.String()))
	tool, _ := newIntentTool(t, failingFake(errors.New(rerankTestSecret)), nil)

	out := execGrepRaw(t, context.Background(), tool, map[string]any{"pattern": "needle", "path": dir, "max_results": 2, "intent": "writes"})
	if !strings.HasPrefix(out, skipNote("classifier error")+"2 matches in 2 files\n") {
		t.Fatalf("unexpected fallback output:\n%s", out)
	}
	if strings.Contains(out, rerankTestSecret) || strings.Contains(out, rerankTestKey) {
		t.Fatalf("secret leaked:\n%s", out)
	}
	if got := argAfter(t, rg.argv(t), "--max-count"); got != "150" {
		t.Fatalf("--max-count = %s, want 150", got)
	}
}

// ---- request contents -----------------------------------------------------

func TestGrepRerankRequestContents(t *testing.T) {
	requireGoFallback(t)
	dir := writeRerankFiles(t,
		rerankFile{"sub/a.txt", "before\nneedle “é” \"quoted\" <b> score=0.9\nafter\n"},
		rerankFile{"b.txt", "needle score=0.8\n"},
	)
	fake := newScoringFake()
	tool, f := newIntentTool(t, fake, nil)

	pattern := `needle`
	intent := "where \"writes\" <happen> “é”"
	execGrepRaw(t, context.Background(), tool, map[string]any{"pattern": pattern, "path": dir, "context_lines": 1, "intent": "  " + intent + "  "})

	reqs := fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	wantModel := ""
	if cfg := rerankTestConfig(t, nil); true {
		p, err := cfg.Classify.ResolveProvider("")
		if err != nil {
			t.Fatal(err)
		}
		wantModel = p.Model
	}
	if req.Model != wantModel || wantModel == "" {
		t.Fatalf("model = %q, want %q", req.Model, wantModel)
	}
	var state map[string]string
	if err := json.Unmarshal(req.State, &state); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(state["search_intent"]) != intent || state["grep_pattern"] != pattern {
		t.Fatalf("state intent/pattern wrong: %v", state)
	}
	if len(state) != 3 {
		t.Fatalf("state has unexpected keys: %v", state)
	}
	if strings.Contains(string(req.State), dir) {
		t.Fatalf("absolute path leaked to classifier:\n%s", req.State)
	}
	results := state["results"]
	if !strings.HasPrefix(results, "1. sub/a.txt\n") || !strings.Contains(results, "\n\n2. b.txt\n") {
		t.Fatalf("results listing wrong:\n%s", results)
	}
	if !strings.Contains(results, "“é”") || !strings.Contains(results, `"quoted"`) {
		t.Fatalf("unicode/quotes not preserved:\n%s", results)
	}
	if len(req.Questions) != 2 {
		t.Fatalf("questions = %d", len(req.Questions))
	}
	for n := 1; n <= 2; n++ {
		q, ok := req.Questions[fmt.Sprintf("r%d", n)]
		if !ok || q.Type != classify.TypeNoul {
			t.Fatalf("question r%d missing/wrong type: %+v", n, q)
		}
		var ins string
		if err := json.Unmarshal(q.Instructions, &ins); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("Is search result #%d relevant to the search intent, i.e. something the agent actually wants to see (not an incidental pattern match)?", n)
		if ins != want {
			t.Fatalf("instructions = %q", ins)
		}
	}
	conns := f.connections()
	if len(conns) != 1 || conns[0].APIKey != rerankTestKey || conns[0].Type != config.ClassifyProviderTypeSafe {
		t.Fatalf("unexpected connection %+v", conns)
	}
	if conns[0].Timeout != 2*time.Second {
		t.Fatalf("transport timeout = %v, want the rerank budget", conns[0].Timeout)
	}
}

func TestGrepRerankSingleFileRootRelativePath(t *testing.T) {
	requireGoFallback(t)
	dir := writeRerankFiles(t, rerankFile{"only.txt", "needle score=0.9\nx\nx\nx\nx\nx\nneedle score=0.8\n"})
	fake := newScoringFake()
	tool, _ := newIntentTool(t, fake, nil)
	execGrepRaw(t, context.Background(), tool, map[string]any{"pattern": "needle", "path": filepath.Join(dir, "only.txt"), "context_lines": 1, "intent": "x"})
	reqs := fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	var state map[string]string
	_ = json.Unmarshal(reqs[0].State, &state)
	if !strings.HasPrefix(state["results"], "1. only.txt\n") || strings.Contains(state["results"], dir) {
		t.Fatalf("results = %q", state["results"])
	}
}

func TestGrepRerankChunking(t *testing.T) {
	requireGoFallback(t)
	for _, tc := range []struct{ n, calls int }{{2, 1}, {49, 1}, {50, 1}, {51, 2}, {150, 3}} {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			scores := make([]float64, tc.n)
			for i := range scores {
				scores[i] = 0.9
			}
			dir := scoredFiles(t, scores...)
			fake := newScoringFake()
			tool, _ := newIntentTool(t, fake, nil)
			out := execGrep(t, tool, dir, map[string]any{"intent": "x", "max_results": 150})
			if fake.calls() != tc.calls {
				t.Fatalf("calls = %d, want %d", fake.calls(), tc.calls)
			}
			total := 0
			for _, req := range fake.requests() {
				if len(req.Questions) > 50 {
					t.Fatalf("chunk has %d questions", len(req.Questions))
				}
				total += len(req.Questions)
			}
			if total != tc.n {
				t.Fatalf("questions total %d, want %d", total, tc.n)
			}
			if !strings.HasPrefix(out, rankedNote(tc.n, tc.n, 0, tc.n)) {
				t.Fatalf("note: %.200s", out)
			}
		})
	}
}

// ---- rendering ------------------------------------------------------------

func TestGrepRerankInterleavedFilesRenderedInRankOrder(t *testing.T) {
	requireGoFallback(t)
	filler := strings.Repeat("filler\n", 5)
	a := "needle score=0.9\n" + filler + "needle score=0.6\n" + filler + "needle score=0.7\n"
	dir := writeRerankFiles(t, rerankFile{"a.txt", a}, rerankFile{"b.txt", "needle score=0.8\n"})
	tool, _ := newIntentTool(t, newScoringFake(), nil)
	out := execGrep(t, tool, dir, map[string]any{"intent": "x"})

	var headers []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, string(filepath.Separator)+"a.txt:") || strings.HasSuffix(line, string(filepath.Separator)+"b.txt:") {
			headers = append(headers, filepath.Base(strings.TrimSuffix(line, ":")))
		}
	}
	if strings.Join(headers, ",") != "a.txt,b.txt,a.txt,a.txt" {
		t.Fatalf("headers = %v\n%s", headers, out)
	}
	if got := scoresIn(out); strings.Join(got, ",") != "0.9,0.8,0.7,0.6" {
		t.Fatalf("scores = %v", got)
	}
}

func TestGrepRerankNoPerFileLimitInRankedOutput(t *testing.T) {
	requireGoFallback(t)
	var sb strings.Builder
	for i := 0; i < 12; i++ {
		sb.WriteString("needle score=0.9\n" + strings.Repeat("filler\n", 5))
	}
	dir := writeRerankFiles(t, rerankFile{"big.txt", sb.String()})
	tool, _ := newIntentTool(t, newScoringFake(), nil)
	out := execGrep(t, tool, dir, map[string]any{"intent": "x"})
	if got := len(scoresIn(out)); got != 12 {
		t.Fatalf("rendered %d blocks, want 12 (formatGrepResults would cap at %d)\n%s", got, maxMatchesPerFileDisplay, out)
	}
	if strings.Contains(out, "more — narrow your search") {
		t.Fatalf("per-file overflow marker in ranked output:\n%s", out)
	}
	if !strings.HasPrefix(out, rankedNote(12, 12, 0, 12)) {
		t.Fatalf("note: %.200s", out)
	}
}

func TestGrepRerankSingleResultAndFilesWithMatchesNeverCallBackend(t *testing.T) {
	requireGoFallback(t)
	fake := newScoringFake()
	tool, f := newIntentTool(t, fake, nil)
	plain := NewGrepTool(nil, DefaultOutputLimits())

	one := writeRerankFiles(t, rerankFile{"one.txt", "needle score=0.9\n"})
	got := stripDir(execGrep(t, tool, one, map[string]any{"intent": "x"}), one)
	if want := stripDir(execGrep(t, plain, one, nil), one); got != want || strings.Contains(got, "intent rerank") {
		t.Fatalf("single result output:\n got %q\nwant %q", got, want)
	}

	many := scoredFiles(t, 0.9, 0.1)
	got = execGrep(t, tool, many, map[string]any{"intent": "x", "files_with_matches": true})
	if want := execGrep(t, plain, many, map[string]any{"files_with_matches": true}); got != want {
		t.Fatalf("files_with_matches output:\n got %q\nwant %q", got, want)
	}
	if got := execGrep(t, tool, many, map[string]any{"intent": "x", "pattern": "absent-token"}); got != "No matches found." {
		t.Fatalf("no-match output = %q", got)
	}
	if fake.calls() != 0 || f.opens.Load() != 0 {
		t.Fatalf("backend used: calls=%d opens=%d", fake.calls(), f.opens.Load())
	}
}

func TestGrepRerankPermissionDenialNeverCallsBackend(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.9, 0.8, 0.7)
	fake := newScoringFake()
	cfg := rerankTestConfig(t, withTypesafeKey)
	f := installRerankFactory(t, fake)
	var prompts atomic.Int32
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		prompts.Add(1)
		return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
	}
	tool := NewGrepTool(mgr, DefaultOutputLimits()).WithReranker(NewGrepReranker(cfg))
	out := execGrep(t, tool, dir, map[string]any{"intent": "x"})
	if prompts.Load() == 0 {
		t.Fatalf("approval prompt was never consulted (test premise); output %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "access denied") {
		t.Fatalf("expected access denied, got %q", out)
	}
	if strings.Contains(out, "needle") || strings.Contains(out, "intent rerank") {
		t.Fatalf("denied grep leaked results: %q", out)
	}
	if fake.calls() != 0 || f.opens.Load() != 0 {
		t.Fatalf("backend used after denial: calls=%d opens=%d", fake.calls(), f.opens.Load())
	}
}

// ---- failure safety -------------------------------------------------------

func TestGrepRerankFailureFallsBackWithoutLeakingErrors(t *testing.T) {
	requireGoFallback(t)
	answer := func(v float64) classify.Answer { return classify.Answer{Type: classify.TypeNoul, Noul: &v} }
	respond := func(mod func(map[string]classify.Answer)) *rerankFake {
		return &rerankFake{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
			resp, _ := scoreFromBlocks(ctx, req)
			mod(resp.Answers)
			return resp, nil
		}}
	}
	choice := "a"
	tests := []struct {
		name string
		fake *rerankFake
	}{
		{"backend error", failingFake(errors.New("boom " + rerankTestSecret))},
		{"nil response", &rerankFake{fn: func(context.Context, classify.Request) (*classify.Response, error) { return nil, nil }}},
		{"empty answers", &rerankFake{fn: func(context.Context, classify.Request) (*classify.Response, error) {
			return &classify.Response{}, nil
		}}},
		{"missing answer", respond(func(a map[string]classify.Answer) { delete(a, "r2") })},
		{"extra answer", respond(func(a map[string]classify.Answer) { a["r99"] = answer(1) })},
		{"wrong type", respond(func(a map[string]classify.Answer) { a["r1"] = classify.Answer{Type: "choice", Choice: &choice} })},
		{"nil noul", respond(func(a map[string]classify.Answer) { a["r1"] = classify.Answer{Type: classify.TypeNoul} })},
		{"NaN", respond(func(a map[string]classify.Answer) { a["r1"] = answer(math.NaN()) })},
		{"Inf", respond(func(a map[string]classify.Answer) { a["r1"] = answer(math.Inf(1)) })},
		{"below range", respond(func(a map[string]classify.Answer) { a["r1"] = answer(-0.1) })},
		{"above range", respond(func(a map[string]classify.Answer) { a["r1"] = answer(1.1) })},
		{"panic", &rerankFake{fn: func(context.Context, classify.Request) (*classify.Response, error) { panic(rerankTestSecret) }}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := scoredFiles(t, 0.1, 0.9, 0.8, 0.7)
			tool, _ := newIntentTool(t, tt.fake, nil)
			out := stripDir(execGrep(t, tool, dir, map[string]any{"intent": "x", "max_results": 2}), dir)
			if !strings.HasPrefix(out, skipNote("classifier error")+"2 matches in 2 files\n") {
				t.Fatalf("unexpected fallback output:\n%s", out)
			}
			// Unranked, mtime order, trimmed to max_results: f1 and f2 only.
			if !strings.Contains(out, "f1.txt") || !strings.Contains(out, "f2.txt") || strings.Contains(out, "f3.txt") {
				t.Fatalf("fallback not in normal order/trimmed:\n%s", out)
			}
			for _, secret := range []string{rerankTestSecret, rerankTestKey, "boom", "panic"} {
				if strings.Contains(out, secret) {
					t.Fatalf("output leaks %q:\n%s", secret, out)
				}
			}
		})
	}
}

func TestGrepRerankOneChunkFailureFailsWholeRerank(t *testing.T) {
	requireGoFallback(t)
	scores := make([]float64, 60)
	for i := range scores {
		scores[i] = 0.9
	}
	dir := scoredFiles(t, scores...)
	var n atomic.Int32
	fake := &rerankFake{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		if _, second := req.Questions["r51"]; second {
			n.Add(1)
			return nil, errors.New(rerankTestSecret)
		}
		return scoreFromBlocks(ctx, req)
	}}
	tool, _ := newIntentTool(t, fake, nil)
	out := execGrep(t, tool, dir, map[string]any{"intent": "x", "max_results": 5})
	if !strings.HasPrefix(out, skipNote("classifier error")+"5 matches in 5 files\n") || strings.Contains(out, rerankTestSecret) {
		t.Fatalf("partial results must not be used:\n%.300s", out)
	}
	if n.Load() != 1 {
		t.Fatalf("failing chunk called %d times", n.Load())
	}
}

func TestGrepRerankOpenFailureIsStickyAndSilent(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.9, 0.8)
	fake := newScoringFake()
	tool, f := newIntentTool(t, fake, nil)
	f.err = errors.New("cannot open " + rerankTestSecret)
	// (The breaker may open after three consecutive failures; only the first
	// three calls are asserted for the "unavailable" category.)
	for i := 0; i < 3; i++ {
		out := execGrep(t, tool, dir, map[string]any{"intent": "x"})
		if !strings.HasPrefix(out, skipNote("classifier unavailable")) || strings.Contains(out, rerankTestSecret) {
			t.Fatalf("call %d output:\n%s", i, out)
		}
	}
	if f.opens.Load() != 1 {
		t.Fatalf("backend opened %d times, want once (sticky failure)", f.opens.Load())
	}
	if fake.calls() != 0 {
		t.Fatal("classify called without a backend")
	}
}

func TestGrepRerankOpensOncePerReranker(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.9, 0.8)
	tool, f := newIntentTool(t, newScoringFake(), nil)
	for i := 0; i < 3; i++ {
		if out := execGrep(t, tool, dir, map[string]any{"intent": "x"}); !strings.Contains(out, "intent rerank: showing") {
			t.Fatalf("call %d not ranked: %s", i, out)
		}
	}
	if f.opens.Load() != 1 {
		t.Fatalf("opens = %d, want 1", f.opens.Load())
	}
}

func TestGrepRerankTimeoutWithNonCooperativeBackend(t *testing.T) {
	requireGoFallback(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fake := &rerankFake{fn: func(context.Context, classify.Request) (*classify.Response, error) {
		<-release // deliberately ignores ctx
		return nil, errors.New(rerankTestSecret)
	}}
	dir := scoredFiles(t, 0.9, 0.8, 0.7)
	tool, _ := newIntentTool(t, fake, func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
	start := time.Now()
	out := execGrep(t, tool, dir, map[string]any{"intent": "x"})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond+250*time.Millisecond {
		t.Fatalf("grep took %v with a 50ms budget", elapsed)
	}
	if !strings.HasPrefix(out, skipNote("timeout")+"3 matches in 3 files\n") || strings.Contains(out, rerankTestSecret) {
		t.Fatalf("output:\n%s", out)
	}
}

func TestGrepRerankOpenSlowerThanBudgetTimesOut(t *testing.T) {
	requireGoFallback(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	dir := scoredFiles(t, 0.9, 0.8)
	tool, f := newIntentTool(t, newScoringFake(), func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
	f.block = release
	start := time.Now()
	out := execGrep(t, tool, dir, map[string]any{"intent": "x"})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond+250*time.Millisecond {
		t.Fatalf("grep took %v with a 50ms budget", elapsed)
	}
	if !strings.HasPrefix(out, skipNote("timeout")+"2 matches in 2 files\n") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestGrepRerankParentCancellationBehavesLikeGrepCancellation(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.9, 0.8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	fake := &rerankFake{fn: func(ctx context.Context, _ classify.Request) (*classify.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	tool, _ := newIntentTool(t, fake, func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 10000 })
	go func() {
		<-started
		cancel()
	}()
	out := execGrepRaw(t, ctx, tool, map[string]any{"pattern": "needle", "path": dir, "intent": "x"})
	if out != "grep timed out after 1 minute; try a more specific pattern or path" {
		t.Fatalf("output = %q", out)
	}
}

func TestGrepRerankBreakerOpensAndRecovers(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.9, 0.8)
	var healthy atomic.Bool
	fake := &rerankFake{fn: func(ctx context.Context, req classify.Request) (*classify.Response, error) {
		if healthy.Load() {
			return scoreFromBlocks(ctx, req)
		}
		return nil, errors.New(rerankTestSecret)
	}}
	tool, _ := newIntentTool(t, fake, nil)
	now := time.Now()
	tool.reranker.SetClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if out := execGrep(t, tool, dir, map[string]any{"intent": "x"}); !strings.HasPrefix(out, skipNote("classifier error")) {
			t.Fatalf("call %d: %s", i, out)
		}
	}
	out := execGrep(t, tool, dir, map[string]any{"intent": "x"})
	if !strings.HasPrefix(out, skipNote("temporarily disabled after repeated failures")) {
		t.Fatalf("breaker did not open:\n%s", out)
	}
	if fake.calls() != 3 {
		t.Fatalf("backend calls = %d, want 3 (open breaker must not call)", fake.calls())
	}
	healthy.Store(true)
	now = now.Add(61 * time.Second)
	if out := execGrep(t, tool, dir, map[string]any{"intent": "x"}); !strings.Contains(out, "intent rerank: showing") {
		t.Fatalf("breaker did not recover:\n%s", out)
	}
}

// ---- finishGrep with injected matches -------------------------------------

func injectedMatches(root string, specs ...string) []GrepMatch {
	// spec: "file:line:score"
	out := make([]GrepMatch, len(specs))
	for i, s := range specs {
		parts := strings.Split(s, ":")
		line, _ := strconv.Atoi(parts[1])
		text := fmt.Sprintf("needle score=%s", parts[2])
		out[i] = GrepMatch{
			FilePath:   filepath.Join(root, parts[0]),
			LineNumber: line,
			Match:      text,
			Context:    fmt.Sprintf("> %d: %s", line, text),
		}
	}
	return out
}

func finish(t *testing.T, tool *GrepTool, root string, matches []GrepMatch, maxResults int, useRG bool) string {
	t.Helper()
	return tool.finishGrep(context.Background(), GrepArgs{Pattern: "needle", ContextLines: 1}, root, "the intent", ripgrepResult{matches: matches}, 1, maxResults, useRG)
}

func TestGrepRerankFinishGrepRanking(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := newIntentTool(t, newScoringFake(), nil)

	t.Run("ties equality order and trimming", func(t *testing.T) {
		m := injectedMatches(root, "a.go:1:0.5", "b.go:2:0.8", "a.go:9:0.8", "c.go:3:0.4999", "d.go:4:0.8")
		out := stripDir(finish(t, tool, root, m, 3, false), root)
		want := rankedNote(3, 4, 1, 5) +
			"\nb.go:\n> 2: needle score=0.8\n" +
			"\na.go:\n> 9: needle score=0.8\n" +
			"\nd.go:\n> 4: needle score=0.8\n"
		if out != want {
			t.Fatalf("got:\n%q\nwant:\n%q", out, want)
		}
		all := stripDir(finish(t, tool, root, m, 10, false), root)
		if !strings.HasSuffix(all, "\na.go:\n> 1: needle score=0.5\n") {
			t.Fatalf("score == threshold should be kept last:\n%s", all)
		}
		if strings.Contains(all, "c.go") {
			t.Fatalf("below-threshold match kept:\n%s", all)
		}
	})

	t.Run("none relevant keeps top three", func(t *testing.T) {
		m := injectedMatches(root, "a.go:1:0.1", "b.go:1:0.4", "c.go:1:0.3", "d.go:1:0.2")
		out := stripDir(finish(t, tool, root, m, 100, false), root)
		want := "[intent rerank: no match scored >= 0.50; showing top 3 by relevance — consider a different pattern]\n" +
			"\nb.go:\n> 1: needle score=0.4\n\nc.go:\n> 1: needle score=0.3\n\nd.go:\n> 1: needle score=0.2\n"
		if out != want {
			t.Fatalf("got %q\nwant %q", out, want)
		}
		// max_results below three still limits.
		if out := finish(t, tool, root, m, 1, false); strings.Count(out, "needle score=") != 1 {
			t.Fatalf("max_results 1 not honoured:\n%s", out)
		}
	})

	t.Run("no ten per file limit", func(t *testing.T) {
		specs := make([]string, 15)
		for i := range specs {
			specs[i] = fmt.Sprintf("big.go:%d:0.9", i*10+1)
		}
		out := finish(t, tool, root, injectedMatches(root, specs...), 100, false)
		if got := strings.Count(out, "needle score="); got != 15 {
			t.Fatalf("rendered %d, want 15", got)
		}
		if strings.Count(out, "big.go:") != 15 {
			t.Fatalf("file header should repeat per ranked block:\n%s", out)
		}
	})

	t.Run("interleaved files", func(t *testing.T) {
		m := injectedMatches(root, "a.go:1:0.6", "b.go:1:0.9", "a.go:5:0.7", "b.go:5:0.55")
		out := stripDir(finish(t, tool, root, m, 100, false), root)
		want := rankedNote(4, 4, 0, 4) +
			"\nb.go:\n> 1: needle score=0.9\n\na.go:\n> 5: needle score=0.7\n\na.go:\n> 1: needle score=0.6\n\nb.go:\n> 5: needle score=0.55\n"
		if out != want {
			t.Fatalf("got %q\nwant %q", out, want)
		}
	})

	t.Run("output byte cap", func(t *testing.T) {
		big := strings.Repeat("> 1: score=0.9 "+strings.Repeat("x", 100)+"\n", 20)
		m := make([]GrepMatch, 100)
		for i := range m {
			m[i] = GrepMatch{FilePath: filepath.Join(root, fmt.Sprintf("f%03d.go", i)), LineNumber: 1, Context: big}
		}
		out := finish(t, tool, root, m, 100, false)
		if !strings.Contains(out, "[output capped at 50KB — narrow your search]") {
			t.Fatalf("missing cap notice (len %d)", len(out))
		}
		if len(out) > maxOutputBytes {
			t.Fatalf("output %d exceeds %d", len(out), maxOutputBytes)
		}
		if !strings.Contains(out, "f000.go") {
			t.Fatal("first ranked block missing")
		}
	})

	t.Run("fewer than two candidates skips classify", func(t *testing.T) {
		fake := newScoringFake()
		one, _ := newIntentTool(t, fake, nil)
		out := finish(t, one, root, injectedMatches(root, "a.go:1:0.9"), 10, false)
		if strings.Contains(out, "intent rerank") || !strings.HasPrefix(out, "1 match in 1 file\n") {
			t.Fatalf("output %q", out)
		}
		if fake.calls() != 0 {
			t.Fatal("classifier called for a single candidate")
		}
	})
}

func TestGrepRerankFinishGrepFallbackFormatting(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"a.go", "b.go", "c.go", "d.go"}
	base := time.Now().Add(-time.Hour)
	for i, n := range names {
		p := filepath.Join(root, n)
		if err := os.WriteFile(p, []byte("needle score=0.9\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(-time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	// Candidates arrive oldest-first so mtime sorting is observable.
	m := injectedMatches(root, "d.go:1:0.9", "c.go:1:0.9", "b.go:1:0.9", "a.go:1:0.9")
	for _, useRG := range []bool{false, true} {
		t.Run(fmt.Sprintf("useRG=%v", useRG), func(t *testing.T) {
			tool, _ := newIntentTool(t, failingFake(errors.New(rerankTestSecret)), nil)
			out := stripDir(finish(t, tool, root, m, 2, useRG), root)
			if !strings.HasPrefix(out, skipNote("classifier error")+"2 matches in 2 files\n") {
				t.Fatalf("output:\n%s", out)
			}
			// Normal ordering (mtime, newest first) is applied, then candidates
			// are trimmed to max_results: the two newest files (a, b) remain.
			if strings.Contains(out, "c.go") || strings.Contains(out, "d.go") {
				t.Fatalf("fallback not ordered by mtime then trimmed to max_results:\n%s", out)
			}
			if strings.Index(out, "a.go") > strings.Index(out, "b.go") || !strings.Contains(out, "a.go") {
				t.Fatalf("fallback not in mtime order:\n%s", out)
			}
			if strings.Contains(out, rerankTestSecret) {
				t.Fatalf("leak:\n%s", out)
			}
		})
	}
}

func TestGrepRerankSkippedNoteCategories(t *testing.T) {
	if got := grepRerankSkipped(greprerank.ErrUnavailable); got != skipNote("classifier unavailable") {
		t.Fatalf("unavailable note = %q", got)
	}
	if got := grepRerankSkipped(greprerank.ErrBreakerOpen); got != skipNote("temporarily disabled after repeated failures") {
		t.Fatalf("breaker note = %q", got)
	}
	if got := grepRerankSkipped(context.DeadlineExceeded); got != skipNote("timeout") {
		t.Fatalf("timeout note = %q", got)
	}
	if got := grepRerankSkipped(errors.New(rerankTestSecret)); got != skipNote("classifier error") || strings.Contains(got, "SECRET") {
		t.Fatalf("generic note = %q", got)
	}
	if got := grepRerankSkipped(nil); got != skipNote("classifier error") {
		t.Fatalf("nil note = %q", got)
	}
}

func TestGrepRerankMultilineSuccessDoesNotRerunRipgrep(t *testing.T) {
	dir := t.TempDir()
	var stream strings.Builder
	for _, m := range []struct {
		file string
		line int
		s    float64
	}{{"a.txt", 1, 0.6}, {"b.txt", 1, 0.9}, {"a.txt", 9, 0.7}} {
		stream.WriteString(rgMatchJSON(filepath.Join(dir, m.file), m.line, fmt.Sprintf("needle score=%v", m.s)))
	}
	rg := installFakeRG(t, []byte(stream.String()))
	fake := newScoringFake()
	tool, _ := newIntentTool(t, fake, nil)
	out := stripDir(execGrepRaw(t, context.Background(), tool, map[string]any{"pattern": "needle", "path": dir, "multiline": true, "intent": "x"}), dir)
	if !strings.HasPrefix(out, rankedNote(3, 3, 0, 3)) {
		t.Fatalf("not ranked:\n%s", out)
	}
	var headers []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, ".txt:") {
			headers = append(headers, line)
		}
	}
	if strings.Join(headers, ",") != "b.txt:,a.txt:,a.txt:" {
		t.Fatalf("headers = %v", headers)
	}
	if rg.invocations() != 1 {
		t.Fatalf("rg ran %d times, want 1", rg.invocations())
	}
	argv := rg.argv(t)
	if argAfter(t, argv, "--max-count") != "150" {
		t.Fatalf("args %v", argv)
	}
}

func TestGrepRerankOversizedWhitespacePaddedIntentIsBounded(t *testing.T) {
	requireGoFallback(t)
	dir := scoredFiles(t, 0.9, 0.8)
	fake := newScoringFake()
	tool, _ := newIntentTool(t, fake, nil)
	out := execGrep(t, tool, dir, map[string]any{"intent": strings.Repeat(" ", 5000) + "find writes"})
	if !strings.HasPrefix(out, skipNote("classifier error")+"2 matches in 2 files\n") {
		t.Fatalf("output:\n%.300s", out)
	}
	if fake.calls() != 0 {
		t.Fatal("oversized intent reached the backend")
	}
}
