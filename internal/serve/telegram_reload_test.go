package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/restart"
)

func TestTelegramPollingReloadCancelsOnlyIdlePollAndResumes(t *testing.T) {
	old := restart.Default
	restart.Default = &restart.Coordinator{Timeout: time.Second}
	defer func() { restart.Default = old }()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	publisher := process.Start("telegram-test", "")
	defer publisher.Stop()
	polls := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"fixture","username":"fixture_bot"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			polls <- struct{}{}
			<-r.Context().Done()
			return
		}
		t.Errorf("unexpected API method: %s", r.URL.Path)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("fixture", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	mgr := &telegramSessionMgr{sessions: make(map[int64]*telegramSession)}
	go func() { done <- mgr.runPolling(ctx, bot) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("poller failed to stop")
		}
	}()
	select {
	case <-polls:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	executions := make(chan struct{}, 1)
	stop, err := restart.Default.Bind(ctx, func(context.Context) error { executions <- struct{}{}; return errors.New("fixture exec failure") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	restart.Default.Request()
	select {
	case <-executions:
	case <-time.After(2 * time.Second):
		t.Fatal("idle poll blocked reload")
	}
	select {
	case <-polls:
	case <-time.After(2 * time.Second):
		t.Fatal("polling did not resume after failed exec")
	}
	if status := restart.Default.Status(); status.Phase != "ready" || status.Error != "fixture exec failure" {
		t.Fatal(status)
	}
	if len(process.HandoffEnviron()) != 0 {
		t.Fatal("rollback retained Telegram state hint")
	}
}
