package modelpolicy

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestLockPreservedByComposition(t *testing.T) {
	locked := Policy{LockSession: true}.With("owner", []string{"debug:allowed"})
	if !locked.LockSession || !locked.With("child", []string{"debug:*"}).LockSession {
		t.Fatal("With dropped the lock")
	}
	if !(Policy{}).Append(locked).LockSession || !locked.Append(Policy{}).LockSession {
		t.Fatal("Append dropped the lock")
	}
	if (Policy{}).With("a", []string{"debug:x"}).LockSession {
		t.Fatal("ordinary policy became locked")
	}
}

func TestLockSnapshotsProviderAllowlist(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"debug": {AllowedModels: []string{"allowed", "second"}},
	}}
	effective := Policy{}.With("owner", []string{"debug:*"})
	locked := Lock(cfg, effective)
	if !locked.LockSession || len(locked.Rules) != 2 {
		t.Fatalf("Lock() = %+v, want original rule plus snapshot", locked)
	}
	// Widening the provider later must not widen the locked conversation.
	widened := &config.Config{Providers: map[string]config.ProviderConfig{"debug": {}}}
	if err := locked.CheckWithConfig(widened, "owner", "debug", "later-added"); err == nil {
		t.Fatal("locked policy allowed a model added after locking")
	}
	if err := locked.CheckWithConfig(widened, "owner", "debug", "allowed"); err != nil {
		t.Fatalf("locked policy denied snapshot model: %v", err)
	}
	if again := Lock(cfg, locked); len(again.Rules) != len(locked.Rules) {
		t.Fatalf("re-locking grew rules: %+v", again.Rules)
	}
	if plain := Lock(nil, effective); !plain.LockSession || len(plain.Rules) != 1 {
		t.Fatalf("Lock(nil) = %+v", plain)
	}
}
