package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestServeTelegramNotificationUsesCapturedSessionStore(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "override"
		if disabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			defaultPath := filepath.Join(t.TempDir(), "default.db")
			overridePath := filepath.Join(t.TempDir(), "override.db")
			srv := &serveServer{cfgRef: &config.Config{Sessions: config.SessionsConfig{Enabled: true, Path: defaultPath}}}
			srv.sessionStore = appruntime.SessionStoreOverrides{DBPath: overridePath, NoSession: disabled}
			srv.logTelegramNotifySession(context.Background(), 42, "completed job")
			if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
				t.Fatalf("notification touched default DB: %v", err)
			}
			if disabled {
				if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
					t.Fatalf("disabled notification touched override DB: %v", err)
				}
				return
			}
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: overridePath})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			summaries, err := store.List(context.Background(), session.ListOptions{Name: "telegram:42", Limit: 1})
			if err != nil || len(summaries) != 1 {
				t.Fatalf("override notifications = %v, %v", summaries, err)
			}
			messages, err := store.GetMessages(context.Background(), summaries[0].ID, 0, 0)
			if err != nil || len(messages) != 1 || core.MessageText(messages[0].ToLLMMessage()) != "completed job" {
				t.Fatalf("notification messages = %v, %v", messages, err)
			}
		})
	}
}
