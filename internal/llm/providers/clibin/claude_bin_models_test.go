package clibin

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// claudeListModelsFixture is trimmed from Claude Code 2.1.303's response to
// the stream-json list_models control request.
const claudeListModelsFixture = `{"type":"system","subtype":"init"}
{"type":"control_response","response":{"subtype":"success","request_id":"other","response":{"models":[{"value":"ignored"}]}}}
{"type":"control_response","response":{"subtype":"success","request_id":"term-llm-list-models","response":{"models":[` +
	`{"value":"default","resolvedModel":"claude-mythos-5-1","displayName":"Default (recommended)","description":"Mythos 5.1","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},` +
	`{"value":"opus","resolvedModel":"claude-opus-5-5","displayName":"Opus 5.5","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},` +
	`{"value":"claude-haiku-4-5-20251001","resolvedModel":"claude-haiku-4-5-20251001","displayName":"Haiku 4.5","supportsThinkingOff":true},` +
	`{"value":"claude-sonnet-4-6","resolvedModel":"claude-sonnet-4-6","displayName":"Sonnet 4.6","supportsEffort":true,"supportedEffortLevels":["low","medium","high","max"]}` +
	`]}}}
`

func TestParseClaudeListModelsOutput(t *testing.T) {
	models, err := parseClaudeListModelsOutput([]byte(claudeListModelsFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []struct {
		id      string
		name    string
		efforts []string
	}{
		{"opus", "Opus 5.5", []string{"low", "medium", "high", "xhigh", "max"}},
		{"claude-haiku-4-5-20251001", "Haiku 4.5", nil},
		{"claude-sonnet-4-6", "Sonnet 4.6", []string{"low", "medium", "high", "max"}},
		// "default" is replaced by the model it resolves to.
		{"claude-mythos-5-1", "Mythos 5.1", []string{"low", "medium", "high", "xhigh", "max"}},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %d entries", modelIDs(models), len(want))
	}
	for i, w := range want {
		got := models[i]
		if got.ID != w.id || got.DisplayName != w.name || !slices.Equal(got.ReasoningEfforts, w.efforts) {
			t.Errorf("models[%d] = %q/%q/%v, want %q/%q/%v", i, got.ID, got.DisplayName, got.ReasoningEfforts, w.id, w.name, w.efforts)
		}
	}
}

func TestParseClaudeListModelsOutputDefaultAlreadyListed(t *testing.T) {
	output := `{"type":"control_response","response":{"subtype":"success","request_id":"term-llm-list-models","response":{"models":[` +
		`{"value":"default","resolvedModel":"claude-opus-5-5","description":"Opus 5.5"},{"value":"claude-opus-5-5","displayName":"Opus 5.5"}]}}}`
	models, err := parseClaudeListModelsOutput([]byte(output))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ids := modelIDs(models); !slices.Equal(ids, []string{"claude-opus-5-5"}) {
		t.Fatalf("models = %v, want only claude-opus-5-5", ids)
	}
}

func TestParseClaudeListModelsOutputErrors(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{"rejected", `{"type":"control_response","response":{"subtype":"error","request_id":"term-llm-list-models","error":"Unsupported control request subtype: list_models"}}`, "Unsupported control request"},
		{"empty", `{"type":"control_response","response":{"subtype":"success","request_id":"term-llm-list-models","response":{"models":[]}}}`, "no models"},
		{"no response", "not json\n{\"type\":\"result\"}\n", "did not answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseClaudeListModelsOutput([]byte(tc.output))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestClaudeBinListModelsCachesCatalog(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	original := claudeModelsCommandOutput
	t.Cleanup(func() { claudeModelsCommandOutput = original })

	calls := 0
	claudeModelsCommandOutput = func(context.Context, *ClaudeBinProvider) ([]byte, error) {
		calls++
		return []byte(claudeListModelsFixture), nil
	}
	if err := RefreshClaudeBinModelsIfStale(context.Background(), "", nil); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if err := RefreshClaudeBinModelsIfStale(context.Background(), "", nil); err != nil {
		t.Fatalf("fresh-cache refresh: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model command calls = %d, want 1", calls)
	}
	if ids := GetCachedClaudeBinModels(); !containsModelID(ids, "claude-mythos-5-1") {
		t.Fatalf("cached models = %v, want claude-mythos-5-1", ids)
	}
	for _, m := range GetCachedClaudeBinModelInfos() {
		if m.ID == "claude-sonnet-4-6" && !slices.Equal(m.ReasoningEfforts, []string{"low", "medium", "high", "max"}) {
			t.Fatalf("cached sonnet-4-6 efforts = %v", m.ReasoningEfforts)
		}
	}
}

func TestClaudeBinListModelsCommandFailure(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	original := claudeModelsCommandOutput
	t.Cleanup(func() { claudeModelsCommandOutput = original })
	claudeModelsCommandOutput = func(context.Context, *ClaudeBinProvider) ([]byte, error) {
		return nil, errors.New("exec: \"claude\": executable file not found in $PATH")
	}
	_, err := NewClaudeBinProvider("", nil).ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("err = %v, want command error", err)
	}
	if ids := GetCachedClaudeBinModels(); len(ids) != 0 {
		t.Fatalf("failed listing cached %v", ids)
	}
}

func TestClaudeBinModelListEnvKeepsFeatureFlags(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "high")
	p := NewClaudeBinProvider("opus", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"})
	env := envSliceMap(p.buildModelListEnv())
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("ANTHROPIC_API_KEY should be dropped when OAuth is preferred")
	}
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "token" {
		t.Fatalf("provider env not applied: %v", env["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	// Feature flags gate the account catalog, so listing must not disable
	// them; every other isolation default still applies.
	for key, want := range forcedClaudeCodeIsolationEnv {
		got, ok := env[key]
		if claudeCatalogGateEnv[key] {
			if ok {
				t.Fatalf("model listing env sets catalog gate %s", key)
			}
			continue
		}
		if got != want {
			t.Fatalf("model listing env %s = %q, want %q", key, got, want)
		}
	}
}

func TestClaudeBinModelListEnvOverridesInheritedIsolation(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "0")
	t.Setenv("DISABLE_GROWTHBOOK", "1") // the user's own choice is kept
	env := envSliceMap(NewClaudeBinProvider("", nil).buildModelListEnv())
	if env["CLAUDE_CODE_DISABLE_CLAUDE_MDS"] != "1" {
		t.Fatalf("CLAUDE_CODE_DISABLE_CLAUDE_MDS = %q, want forced 1", env["CLAUDE_CODE_DISABLE_CLAUDE_MDS"])
	}
	if env["DISABLE_GROWTHBOOK"] != "1" {
		t.Fatalf("inherited DISABLE_GROWTHBOOK dropped")
	}
}

func TestRefreshClaudeBinModelsBacksOffAfterFailure(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	original := claudeModelsCommandOutput
	t.Cleanup(func() { claudeModelsCommandOutput = original })
	calls := 0
	claudeModelsCommandOutput = func(context.Context, *ClaudeBinProvider) ([]byte, error) {
		calls++
		return nil, errors.New("signal: killed")
	}
	if err := RefreshClaudeBinModelsIfStale(context.Background(), "", nil); err == nil {
		t.Fatal("expected first refresh to fail")
	}
	if err := RefreshClaudeBinModelsIfStale(context.Background(), "", nil); err != nil {
		t.Fatalf("second refresh within retry interval: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model command calls = %d, want 1", calls)
	}
}

func TestClaudeBinListModelsPrefersClaudeRejection(t *testing.T) {
	original := claudeModelsCommandOutput
	t.Cleanup(func() { claudeModelsCommandOutput = original })
	claudeModelsCommandOutput = func(context.Context, *ClaudeBinProvider) ([]byte, error) {
		return []byte(`{"type":"control_response","response":{"subtype":"error","request_id":"term-llm-list-models","error":"Unsupported control request subtype: list_models"}}`), errors.New("exit status 1")
	}
	_, err := NewClaudeBinProvider("", nil).ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Unsupported control request") {
		t.Fatalf("err = %v, want Claude's rejection message", err)
	}
}

func TestClaudeListedModelsRejectUnsafeIDs(t *testing.T) {
	models := claudeListedModelsToInfos([]claudeListedModel{
		{Value: "opus"},
		{Value: "bad\tid"},
		{Value: "bad\nid"},
		{Value: "bad id"},
		{Value: strings.Repeat("x", claudeModelIDMaxLen+1)},
	})
	if ids := modelIDs(models); !slices.Equal(ids, []string{"opus"}) {
		t.Fatalf("models = %q, want only opus", ids)
	}
}

func TestParseClaudeEffort(t *testing.T) {
	tests := []struct {
		input      string
		wantModel  string
		wantEffort string
	}{
		{"opus-max", "opus", "max"},
		{"opus-MAX", "opus", "max"},
		{"Claude-Opus-4-7-High", "Claude-Opus-4-7", "high"},
		{"opus-xhigh", "opus", "xhigh"},
		{"opus-low", "opus", "low"},
		{"opus-medium", "opus", "medium"},
		{"opus-high", "opus", "high"},
		{"opus", "opus", ""},
		{"sonnet-high", "sonnet", "high"},
		{"sonnet-xhigh", "sonnet", "xhigh"},
		{"sonnet", "sonnet", ""},
		{"fable-max", "fable", "max"},
		{"fable", "fable", ""},
		{"haiku", "haiku", ""},
		{"claude-mythos-5-1-max", "claude-mythos-5-1", "max"},
		{"claude-mythos-5-1-xhigh", "claude-mythos-5-1", "xhigh"},
		{"claude-mythos-5-1", "claude-mythos-5-1", ""},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", ""},
		{"max", "max", ""},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			model, effort := parseClaudeEffort(tt.input)
			if model != tt.wantModel || effort != tt.wantEffort {
				t.Errorf("parseClaudeEffort(%q) = (%q, %q), want (%q, %q)", tt.input, model, effort, tt.wantModel, tt.wantEffort)
			}
		})
	}
}

func TestMapModelToClaudeArg(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opus", "opus"},
		{"sonnet", "sonnet"},
		{"haiku", "haiku"},
		{"fable", "fable"},
		{"claude-fable-5", "claude-fable-5"},
		{"claude-mythos-5-1", "claude-mythos-5-1"},
		{"Claude-Opus-4-7", "claude-opus-4-7"},
		{"sonnet[1m]", "sonnet[1m]"},
		{"claude-fable-5-1[1m]", "claude-fable-5-1[1m]"},
		// Legacy shorthand still maps to the alias it names.
		{"opus-4", "opus"},
		// Unknown names reach Claude Code instead of silently becoming sonnet.
		{"mythos", "mythos"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapModelToClaudeArg(tt.input); got != tt.want {
				t.Errorf("mapModelToClaudeArg(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestClaudeBinModelListEnvIgnoresSimpleMode(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SIMPLE", "1")
	env := envSliceMap(NewClaudeBinProvider("opus", nil).buildModelListEnv())
	if got, ok := env["CLAUDE_CODE_SIMPLE"]; ok {
		t.Fatalf("inherited CLAUDE_CODE_SIMPLE survived model listing as %q", got)
	}
}
