package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestChatUserSelectionValidatedBeforeStartup(t *testing.T) {
	for _, value := range []string{"bad\nname", strings.Repeat("x", 129), ""} {
		r := testInvocation()
		r.opts.User = value
		r.opts.Changed.User = true
		if err := r.run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "--user") {
			t.Fatalf("invalid user was not a usage error: %v", err)
		}
	}
	r := testInvocation()
	r.opts.User = " nick kroll "
	r.opts.AutoSend = []string{"hello"}
	r.deps.Interactive = func() bool { return false }
	r.deps.RestoreReloadState = func(string, any) (bool, error) { return false, nil }
	stop := errors.New("stop before terminal or provider startup")
	r.deps.LoadConfig = func() (*config.Config, error) {
		if r.user.Name() != "nick kroll" {
			t.Fatalf("launch selection=%q", r.user.Name())
		}
		return nil, stop
	}
	if err := r.run(context.Background(), nil); !errors.Is(err, stop) {
		t.Fatal(err)
	}
}
