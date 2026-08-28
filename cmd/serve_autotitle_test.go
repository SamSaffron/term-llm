package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func newServeAutoTitleTestStore(t *testing.T, id string) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	sess := &session.Session{
		ID: id, Provider: "mock", ProviderKey: "mock", Model: "mock-model",
		Mode: session.ModeChat, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		llm.UserText("Investigate why the GitHub Actions dependency cache is stale"),
		llm.AssistantText("The cache key omits the lockfile hash, so dependency changes reuse an old cache."),
	} {
		if err := store.AddMessage(context.Background(), id, session.NewMessage(id, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestServeAutoTitleGeneratesAndPersists(t *testing.T) {
	const sessionID = "web-auto-title"
	store := newServeAutoTitleTestStore(t, sessionID)
	provider := llm.NewMockProvider("title").AddTextResponse(`{"short_title":"Fix Actions Cache Key","long_title":"Fix stale GitHub Actions dependency caching","confidence":0.95}`)
	server := &serveServer{
		cfgRef: &config.Config{Serve: config.ServeConfig{AutoTitle: true}},
		store:  store,
		autoTitleProviderFactory: func(providerKey string) (llm.Provider, error) {
			if providerKey != "mock" {
				t.Fatalf("provider key = %q, want mock", providerKey)
			}
			return provider, nil
		},
	}

	server.scheduleAutoTitle(sessionID, "mock")
	server.autoTitleWG.Wait()

	got, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedShortTitle != "Fix Actions Cache Key" || got.GeneratedLongTitle != "Fix stale GitHub Actions dependency caching" {
		t.Fatalf("generated titles = %q / %q", got.GeneratedShortTitle, got.GeneratedLongTitle)
	}
	if got.TitleSource != session.TitleSourceGenerated {
		t.Fatalf("title source = %q, want generated", got.TitleSource)
	}
	if provider.CurrentTurn() != 1 {
		t.Fatalf("provider turns = %d, want 1", provider.CurrentTurn())
	}
}

func TestServeAutoTitleDisabledOrManualNameSkipsProvider(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		manual  string
	}{
		{name: "disabled", enabled: false},
		{name: "manual name", enabled: true, manual: "My durable name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const sessionID = "web-auto-title-skip"
			store := newServeAutoTitleTestStore(t, sessionID)
			if test.manual != "" {
				sess, _ := store.Get(context.Background(), sessionID)
				sess.Name = test.manual
				sess.TitleSource = session.TitleSourceUser
				if err := store.Update(context.Background(), sess); err != nil {
					t.Fatal(err)
				}
			}
			provider := llm.NewMockProvider("title").AddTextResponse(`{"short_title":"Should Not Run","long_title":"This title should never be generated","confidence":0.9}`)
			server := &serveServer{
				cfgRef:                   &config.Config{Serve: config.ServeConfig{AutoTitle: test.enabled}},
				store:                    store,
				autoTitleProviderFactory: func(string) (llm.Provider, error) { return provider, nil },
			}

			server.scheduleAutoTitle(sessionID, "mock")
			server.autoTitleWG.Wait()
			if provider.CurrentTurn() != 0 {
				t.Fatalf("provider turns = %d, want 0", provider.CurrentTurn())
			}
		})
	}
}

func TestServeAutoTitleDeduplicatesConcurrentSchedules(t *testing.T) {
	const sessionID = "web-auto-title-dedupe"
	store := newServeAutoTitleTestStore(t, sessionID)
	provider := llm.NewMockProvider("title")
	provider.AddTurn(llm.MockTurn{
		Text:  `{"short_title":"Fix Actions Cache Key","long_title":"Fix stale GitHub Actions dependency caching","confidence":0.95}`,
		Delay: 50 * time.Millisecond,
	})
	server := &serveServer{
		cfgRef:                   &config.Config{Serve: config.ServeConfig{AutoTitle: true}},
		store:                    store,
		autoTitleProviderFactory: func(string) (llm.Provider, error) { return provider, nil },
	}

	server.scheduleAutoTitle(sessionID, "mock")
	server.scheduleAutoTitle(sessionID, "mock")
	server.autoTitleWG.Wait()
	if provider.CurrentTurn() != 1 {
		t.Fatalf("provider turns = %d, want 1", provider.CurrentTurn())
	}
}

func TestServeAutoTitleCapsFailedAttempts(t *testing.T) {
	const sessionID = "web-auto-title-attempt-cap"
	store := newServeAutoTitleTestStore(t, sessionID)
	provider := llm.NewMockProvider("title").AddTextResponse("not json").AddTextResponse("still not json")
	server := &serveServer{
		cfgRef:                   &config.Config{Serve: config.ServeConfig{AutoTitle: true}},
		store:                    store,
		autoTitleProviderFactory: func(string) (llm.Provider, error) { return provider, nil },
	}

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			message := session.NewMessage(sessionID, llm.UserText("more context for the title"), -1)
			if err := store.AddMessage(context.Background(), sessionID, message); err != nil {
				t.Fatal(err)
			}
		}
		server.scheduleAutoTitle(sessionID, "mock")
		server.autoTitleWG.Wait()
	}
	if provider.CurrentTurn() != serveAutoTitleMaxAttempts {
		t.Fatalf("provider turns = %d, want capped at %d", provider.CurrentTurn(), serveAutoTitleMaxAttempts)
	}
}

func TestServeAutoTitleStopCancelsAndDrainsWorker(t *testing.T) {
	const sessionID = "web-auto-title-stop"
	store := newServeAutoTitleTestStore(t, sessionID)
	provider := llm.NewMockProvider("title")
	provider.AddTurn(llm.MockTurn{Text: `{"short_title":"Late Generated Title","long_title":"A title that should be cancelled during shutdown","confidence":0.95}`, Delay: 5 * time.Second})
	server := &serveServer{
		cfgRef:                   &config.Config{Serve: config.ServeConfig{AutoTitle: true}},
		store:                    store,
		autoTitleProviderFactory: func(string) (llm.Provider, error) { return provider, nil },
	}

	server.scheduleAutoTitle(sessionID, "mock")
	deadline := time.Now().Add(time.Second)
	for provider.CurrentTurn() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	server.stopAutoTitles()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stopAutoTitles took %v after cancellation", elapsed)
	}
	server.scheduleAutoTitle(sessionID, "mock")
	if provider.CurrentTurn() != 1 {
		t.Fatalf("provider turns after stop = %d, want 1", provider.CurrentTurn())
	}
}

func TestServeAutoTitleManualRenameWinsInFlight(t *testing.T) {
	const sessionID = "web-auto-title-race"
	store := newServeAutoTitleTestStore(t, sessionID)
	provider := llm.NewMockProvider("title")
	provider.AddTurn(llm.MockTurn{
		Text:  `{"short_title":"Generated Cache Title","long_title":"Generated detail about stale dependency caching","confidence":0.95}`,
		Delay: 100 * time.Millisecond,
	})
	server := &serveServer{
		cfgRef:                   &config.Config{Serve: config.ServeConfig{AutoTitle: true}},
		store:                    store,
		autoTitleProviderFactory: func(string) (llm.Provider, error) { return provider, nil },
	}

	server.scheduleAutoTitle(sessionID, "mock")
	deadline := time.Now().Add(time.Second)
	for provider.CurrentTurn() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	sess, _ := store.Get(context.Background(), sessionID)
	sess.Name = "Manual cache investigation"
	sess.TitleSource = session.TitleSourceUser
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	server.autoTitleWG.Wait()

	got, _ := store.Get(context.Background(), sessionID)
	if got.PreferredShortTitle() != "Manual cache investigation" || got.TitleSource != session.TitleSourceUser {
		t.Fatalf("manual title lost: preferred=%q source=%q", got.PreferredShortTitle(), got.TitleSource)
	}
	if got.GeneratedShortTitle != "" {
		t.Fatalf("in-flight generated title was saved after manual rename: %q", got.GeneratedShortTitle)
	}
}
