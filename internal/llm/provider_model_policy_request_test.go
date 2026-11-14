package llm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestProviderAllowedModelsRequestOverrides(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"claude-bin": {Model: "claude-opus-5-5-high", AllowedModels: []string{"claude-opus-5-5-high"}}}}
	for _, create := range []func(*config.Config, string, string) (core.Provider, error){NewProviderByName, NewProviderByNameNoRetry} {
		p, err := create(cfg, "claude-bin", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, req := range []core.Request{{Model: "opus"}, {Model: "claude-sonnet-4-6-high"}, {ReasoningEffort: "max"}, {Model: "claude-opus-5-5-high", ReasoningEffort: "max"}} {
			if _, err := p.Stream(context.Background(), req); err == nil || !strings.Contains(err.Error(), "allowed_models") {
				t.Fatalf("request override bypassed allowlist: %+v: %v", req, err)
			}
		}
		if _, err := p.(core.NativeCompactor).NativeCompact(context.Background(), core.NativeCompactRequest{Model: "opus"}); err == nil || !strings.Contains(err.Error(), "allowed_models") {
			t.Fatalf("native compaction override: %v", err)
		}
		for _, helper := range []core.Provider{isolatedConversationProvider(p)} {
			if _, err := helper.Stream(context.Background(), core.Request{Model: "opus"}); err == nil || !strings.Contains(err.Error(), "allowed_models") {
				t.Fatalf("helper lost policy: %v", err)
			}
		}
		if helper, ok := forkConversationProvider(p); ok {
			if _, err := helper.Stream(context.Background(), core.Request{Model: "opus"}); err == nil || !strings.Contains(err.Error(), "allowed_models") {
				t.Fatalf("fork lost policy: %v", err)
			}
		}
	}
}

func TestProviderAllowedModelsClaudeEffortWire(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Claude uses a POSIX shell")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$POLICY_TEST_CAPTURE"
printf 'effort=%s\n' "$CLAUDE_CODE_EFFORT_LEVEL" >> "$POLICY_TEST_CAPTURE"
IFS= read -r _
printf '%s\n' '{"type":"system","session_id":"policy-test","model":"claude-opus-5-5","tools":[]}'
printf '%s\n' '{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"claude-bin": {Model: "claude-opus-5-5", AllowedModels: []string{"claude-opus-5-5", "claude-sonnet-5-5"}, Env: map[string]string{"POLICY_TEST_CAPTURE": capture}}}}
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			t.Run(model+"-"+effort, func(t *testing.T) {
				p, err := NewProviderByNameNoRetry(cfg, "claude-bin", model+"-"+effort)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				stream, err := p.Stream(ctx, core.Request{Ephemeral: true, Messages: []core.Message{core.UserText("test")}})
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				for {
					_, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				wire := string(data)
				if !strings.Contains(wire, "--model\n"+model+"\n") || !strings.Contains(wire, "effort="+effort+"\n") {
					t.Fatalf("model/effort changed at subprocess boundary: %s", wire)
				}
			})
		}
	}
}

func TestProviderAllowedModelsRequestPolicySnapshot(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"debug": {Model: "allowed", AllowedModels: []string{"allowed"}}}}
	p, err := NewProviderByName(cfg, "debug", "")
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.Providers["debug"]
	pc.AllowedModels[0] = "denied"
	pc.Model = "denied"
	cfg.Providers["debug"] = pc
	if _, err := p.Stream(context.Background(), core.Request{Model: "denied"}); err == nil {
		t.Fatal("later config mutation changed admitted provider policy")
	}
	wrapped := p.(*RetryProvider)
	if err := wrapped.checkModel("", ""); err != nil {
		t.Fatal("later default override changed provider fallback:", err)
	}
}

func TestProviderAllowedModelsNoRetryRetainsGuardWithoutRetrying(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"debug": {Model: "allowed", AllowedModels: []string{"allowed"}}}}
	p, err := NewProviderByNameNoRetry(cfg, "debug", "")
	if err != nil {
		t.Fatal(err)
	}
	guard, ok := p.(*RetryProvider)
	if !ok || guard.checkModel == nil || guard.config.MaxAttempts != 1 {
		t.Fatalf("no-retry provider must retain admission and exactly one attempt: %T", p)
	}
}
