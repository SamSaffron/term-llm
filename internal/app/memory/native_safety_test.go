package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	memorydb "github.com/term-llm/term-llm/internal/memory"
)

const nativeSyntheticSecret = "sk-synthetic_credential_1234567890"

func TestNativeMemoryRejectsFragmentAndImageSecrets(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, update := range []bool{false, true} {
		collector := newMemoryExtractionCollector()
		args, _ := json.Marshal(map[string]string{"path": "fragments/notes/test.md", "content": "api_key: " + nativeSyntheticSecret})
		var tool core.Tool = &memoryCreateFragmentTool{store: store, agent: "assistant", collector: collector}
		if update {
			tool = &memoryUpdateFragmentTool{store: store, agent: "assistant", collector: collector}
		}
		if _, err := tool.Execute(ctx, args); err == nil {
			t.Fatal("detected secret accepted for fragment write")
		} else if strings.Contains(err.Error(), nativeSyntheticSecret) {
			t.Fatal("rejection leaked content")
		}
	}
	env := testEnv("", "assistant")
	if err := env.upsertImageFragment(ctx, store, "assistant", "images/test.md", "# Image\napi_key: "+nativeSyntheticSecret); err == nil {
		t.Fatal("image path bypassed secret rejection")
	}
}

func TestNativeMemorySanitizesProviderInputAndDisablesRawDebug(t *testing.T) {
	provider := mockprovider.NewMockProvider("native").AddTextResponse("done")
	eng := engine.NewEngine(provider, engine.NewToolRegistry())
	opts := MineOptions{Env: Env{Deps: Deps{DebugRaw: func() bool { return true }}}, ToolMaxTurns: 2}
	if _, err := opts.runExtractionRequestWithSystem(context.Background(), eng, "extract", defaultProtection.sanitize("api_key: "+nativeSyntheticSecret), nil); err != nil {
		t.Fatal(err)
	}
	reqs := provider.RecordedRequests()
	if len(reqs) != 1 || strings.Contains(reqs[0].Messages[1].Parts[0].Text, nativeSyntheticSecret) {
		t.Fatal("provider received detected secret")
	}
	if reqs[0].DebugRaw {
		t.Fatal("raw debug enabled for unchecked model output")
	}
}

func TestNativePromotionRejectsInvalidOutputBeforeMutation(t *testing.T) {
	for _, output := range []string{"# Bad\n\napi_key: " + nativeSyntheticSecret, strings.Repeat("long ", 200), ""} {
		t.Run(strings.Split(output, "\n")[0][:min(len(strings.Split(output, "\n")[0]), 12)], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			agent := filepath.Join(dir, "agent")
			if err := os.MkdirAll(filepath.Join(agent, "memory"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agent, "agent.yaml"), []byte("name: fixture\ndescription: fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			recent := filepath.Join(agent, "memory", "recent.md")
			if err := os.WriteFile(recent, []byte("known good"), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(dir, "memory.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: agent, Path: "fragments/notes/safe.md", Content: "safe fact"}); err != nil {
				t.Fatal(err)
			}
			watermark := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			if err := store.SetMeta(ctx, memoryPromoteMetaKey(agent), watermark); err != nil {
				t.Fatal(err)
			}
			provider := mockprovider.NewMockProvider("native").AddTextResponse(output)
			_, err = runMemoryPromoteFlow(ctx, testEnv("", ""), &config.Config{}, engine.NewEngine(provider, nil), store, memoryPromoteOptions{Agent: agent, RecentMaxBytes: 200})
			if err == nil {
				t.Fatal("invalid promotion accepted")
			}
			data, readErr := os.ReadFile(recent)
			meta, metaErr := store.GetMeta(ctx, memoryPromoteMetaKey(agent))
			if readErr != nil || metaErr != nil || string(data) != "known good" || meta != watermark {
				t.Fatal("rejection changed recent or watermark")
			}
		})
	}
}

func TestNativePromotionAcceptsExactSafeOutput(t *testing.T) {
	p := defaultProtection
	for _, text := range []string{"# Recent\n\n- A durable observation.", "# Recent\n\n## Project\n\nSafe state."} {
		if err := p.recent(text, len(text)); err != nil {
			t.Fatal("safe native summary rejected")
		}
	}
	for _, path := range []string{" fragments/credentials/test.md ", `fragments\credentials\test.md`, "fragments/./credentials/test.md"} {
		if defaultProtection.excluded(path) {
			t.Fatal("default policy unexpectedly excludes credential locations")
		}
	}
}

type nativeRecordingDebugLogger struct{ calls atomic.Int32 }

func (l *nativeRecordingDebugLogger) LogRequest(string, string, core.Request) { l.calls.Add(1) }
func (l *nativeRecordingDebugLogger) LogTurnRequest(int, string, string, core.Request) {
	l.calls.Add(1)
}
func (l *nativeRecordingDebugLogger) LogEvent(core.Event) { l.calls.Add(1) }

func TestNativeProviderDiagnosticsCannotEchoUncheckedOutput(t *testing.T) {
	provider := mockprovider.NewMockProvider("native").AddTextResponse(nativeSyntheticSecret)
	eng := engine.NewEngine(provider, nil)
	logger := &nativeRecordingDebugLogger{}
	eng.SetDebugLogger(logger)
	opts := MineOptions{Env: Env{Deps: Deps{DebugRaw: func() bool { return true }}}}
	if _, err := opts.runExtractionRequestWithSystem(context.Background(), eng, "extract", "safe input", nil); err != nil {
		t.Fatal(err)
	}
	if logger.calls.Load() != 0 || provider.RecordedRequests()[0].DebugRaw {
		t.Fatal("unchecked output reached raw or structured debug logger")
	}
	provider.AddError(errors.New(nativeSyntheticSecret))
	if _, err := opts.runExtractionRequestWithSystem(context.Background(), eng, "extract", "safe input", nil); err == nil || strings.Contains(err.Error(), nativeSyntheticSecret) {
		t.Fatal("provider error body leaked")
	}
}

func TestFableOwnerOptoutPermitsExplicitRawDebug(t *testing.T) {
	p, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Disable: []string{"all"}}})
	if err != nil {
		t.Fatal(err)
	}
	provider := mockprovider.NewMockProvider("owner-optout").AddTextResponse("safe diagnostic output")
	eng := engine.NewEngine(provider, nil)
	logger := &nativeRecordingDebugLogger{}
	eng.SetDebugLogger(logger)
	opts := MineOptions{Env: Env{protection: p, Deps: Deps{DebugRaw: func() bool { return true }}}}
	if _, err := opts.runExtractionRequestWithSystem(context.Background(), eng, "extract", "safe input", nil); err != nil {
		t.Fatal(err)
	}
	if !provider.RecordedRequests()[0].DebugRaw || logger.calls.Load() == 0 {
		t.Fatal("explicit owner optout did not restore raw diagnostics")
	}
}
func TestFableProviderDetailsRemainUsefulAndRedacted(t *testing.T) {
	err := defaultProtection.providerError("extraction", errors.New("HTTP 429 rate limit; api_key: "+nativeSyntheticSecret))
	if !strings.Contains(err.Error(), "429 rate limit") || strings.Contains(err.Error(), nativeSyntheticSecret) {
		t.Fatal("diagnostic detail lost or secret leaked")
	}
}
func TestFableQualityHeuristicsAreNotHardSecurityRules(t *testing.T) {
	for _, text := range []string{"A short plain summary.", "Rate limit handling is now exponential.", "# Configuration\n\n```yaml\nretries: 3\n```", "# Empty"} {
		if err := defaultProtection.recent(text, 1000); err != nil {
			t.Fatal("editorial heuristic still blocks owner memory")
		}
	}
}
