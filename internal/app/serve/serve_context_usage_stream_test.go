package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

// Providers that manage their own context (claude-bin) report occupancy while
// their tool loop runs. The web stream must carry each reading as it arrives,
// before response.completed, and the completion snapshot must use it too.
func TestStreamResponses_ForwardsProviderReportedContextMidRun(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := mockprovider.NewMockProvider("mock").
			WithCapabilities(core.Capabilities{ManagesOwnContext: true})
		provider.AddTurn(mockprovider.MockTurn{
			Text:    "done",
			Context: &core.ContextUsage{UsedTokens: 25670, Window: 1_000_000},
			Usage:   core.Usage{InputTokens: 6, CachedInputTokens: 63699, CacheWriteTokens: 13071, OutputTokens: 184},
		})
		rt := &serveRuntime{provider: provider, engine: engine.NewEngine(provider, nil), defaultModel: "mock-model"}
		rt.Touch()
		return rt, nil
	}
	srv := &serveServer{sessionMgr: newServeSessionManager(time.Minute, 100, factory)}
	defer srv.sessionMgr.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var (
		order     []string
		midRun    map[string]any
		completed map[string]any
		current   string
	)
	scanner := bufio.NewScanner(rr.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			current = strings.TrimPrefix(line, "event: ")
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		switch current {
		case "response.context_usage", "response.completed":
			order = append(order, current)
			var payload map[string]any
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatalf("parse %s: %v", current, err)
			}
			if current == "response.completed" {
				completed = payload
			} else if midRun == nil {
				midRun = payload
			}
		}
	}
	if len(order) == 0 || order[0] != "response.context_usage" || order[len(order)-1] != "response.completed" {
		t.Fatalf("event order = %v, want response.context_usage before response.completed", order)
	}
	assertContext := func(name string, usage any) {
		t.Helper()
		got, _ := usage.(map[string]any)
		if got["used_tokens"] != float64(25670) || got["input_limit"] != float64(1_000_000) {
			t.Fatalf("%s context_usage = %v, want 25670/1000000 (summed billing usage must not leak in)", name, usage)
		}
	}
	assertContext("mid-run", midRun["context_usage"])
	assertContext("completed", completed["response"].(map[string]any)["context_usage"])
}

// A session refresh mid-run must not replace the live provider reading with
// the row persisted at the previous turn end (null for claude-bin), or the web
// meter flickers between the reading and "no snapshot".
func TestSelectedWebSessionPrefersLiveProviderReportedContext(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sessionID := session.NewID()
	if err := store.Create(ctx, &session.Session{ID: sessionID, Provider: "claude-bin", Model: "opus", CachedInputTokens: 900, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ManagesOwnContext: true})
	provider.AddTurn(mockprovider.MockTurn{Text: "working", Context: &core.ContextUsage{UsedTokens: 25670, Window: 1_000_000}})
	eng := engine.NewEngine(provider, nil)
	stream, err := eng.Stream(ctx, core.Request{Messages: []core.Message{core.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	mgr := newServeSessionManager(time.Minute, 100, func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{provider: provider, engine: eng, defaultModel: "opus"}, nil
	})
	defer mgr.Close()
	if _, err := mgr.GetOrCreate(ctx, sessionID); err != nil {
		t.Fatal(err)
	}

	srv := &serveServer{store: store, sessionMgr: mgr}
	selected, err := srv.selectedWebSession(ctx, sessionID, nil)
	if err != nil || selected == nil {
		t.Fatalf("selected = %+v, err = %v", selected, err)
	}
	got := selected.ContextUsage
	if got == nil || got.UsedTokens != 25670 || got.InputLimit != 1_000_000 || got.CachedInputTokens != 900 {
		t.Fatalf("selected context_usage = %+v, want live 25670/1000000 with persisted cached 900", got)
	}
	if total, _ := eng.ContextEstimateBaseline(); total != 25670 {
		t.Fatalf("ContextEstimateBaseline() = %d, want the reported 25670 so it persists", total)
	}
}
