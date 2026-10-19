package cmd

import (
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionStoreHooksHonorRootOverrides(t *testing.T) {
	oldNoSession, oldDB := noSession, sessionDBPath
	t.Cleanup(func() { noSession, sessionDBPath = oldNoSession, oldDB })
	noSession, sessionDBPath = true, " explicit.db "
	cfg := &config.Config{Sessions: config.SessionsConfig{Enabled: true, Path: "configured.db", MaxAgeDays: 7, MaxCount: 12, StripImageBase64: true}}
	hooks := map[string]func(*config.Config) session.Config{"memory": memoryEnv().Deps.SessionStoreConfig, "sessions": sessionsApp().Deps.SessionStoreConfig}
	want := session.Config{Enabled: false, Path: "explicit.db", MaxAgeDays: 7, MaxCount: 12, StripImageBase64: true}
	for name, hook := range hooks {
		t.Run(name, func(t *testing.T) {
			if got := hook(cfg); !reflect.DeepEqual(got, want) {
				t.Fatalf("hook config %#v, want %#v", got, want)
			}
		})
	}
}
