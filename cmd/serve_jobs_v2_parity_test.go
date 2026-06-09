package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func sliceHasString(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}

// TestJobsV2LLMConfigParityRoundTrip verifies every new (and existing) field
// survives a JSON marshal/unmarshal round-trip under the documented key names.
func TestJobsV2LLMConfigParityRoundTrip(t *testing.T) {
	persist := false
	notifyOrigin := &jobsV2NotifyOrigin{Origin: "web", SessionID: "sess-parent"}
	full := jobsV2LLMConfig{
		AgentName:       "developer",
		Instructions:    "implement the spec",
		Progressive:     true,
		StopWhen:        "done",
		ContinueWith:    "keep going",
		PersistSession:  &persist,
		SessionID:       "sess-1",
		NotifyWhenDone:  true,
		NotifyOrigin:    notifyOrigin,
		Provider:        "claude-bin:opus-max",
		Model:           "opus-max",
		Cwd:             "/work/tree",
		ReadDir:         []string{"/a", "/b"},
		WriteDir:        []string{"/c"},
		Tools:           "all",
		MaxTurns:        400,
		MaxOutputTokens: 8192,
		Search:          true,
		SystemMessage:   "be terse",
		Skills:          "all",
	}

	data, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got jobsV2LLMConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(full, got) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", got, full)
	}

	for _, key := range []string{
		"provider", "model", "cwd", "notify_when_done", "notify_origin", "read_dir", "write_dir", "tools",
		"max_turns", "max_output_tokens", "search", "system_message", "skills",
	} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Errorf("marshaled config missing documented key %q: %s", key, data)
		}
	}
}

// TestJobsV2LLMConfigBackwardCompatDefaults asserts that an existing job
// definition (only the original fields) deserializes unchanged, every new parity
// field defaults to its zero value, and omitempty keeps them out of the
// re-serialized form so the on-disk format is unchanged.
func TestJobsV2LLMConfigBackwardCompatDefaults(t *testing.T) {
	const legacy = `{"agent_name":"planner","instructions":"plan","progressive":true}`

	var cfg jobsV2LLMConfig
	if err := json.Unmarshal([]byte(legacy), &cfg); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if cfg.AgentName != "planner" || cfg.Instructions != "plan" || !cfg.Progressive {
		t.Fatalf("legacy fields not preserved: %+v", cfg)
	}
	if cfg.Provider != "" || cfg.Model != "" || cfg.Cwd != "" || cfg.Tools != "" ||
		cfg.MaxTurns != 0 || cfg.MaxOutputTokens != 0 || cfg.Search || cfg.NotifyWhenDone ||
		cfg.SystemMessage != "" || cfg.Skills != "" || cfg.NotifyOrigin != nil ||
		cfg.ReadDir != nil || cfg.WriteDir != nil {
		t.Fatalf("new parity fields should default to zero, got: %+v", cfg)
	}
	// Session persistence default (nil → enabled) must be unchanged.
	if !cfg.sessionPersistenceEnabled() {
		t.Fatal("sessionPersistenceEnabled() = false, want true by default")
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		"provider", "model", "read_dir", "write_dir", "tools",
		"max_turns", "max_output_tokens", "system_message", "skills", "notify_when_done", "notify_origin",
	} {
		if strings.Contains(string(out), `"`+key+`"`) {
			t.Errorf("omitempty broken: re-serialized legacy config unexpectedly contains %q: %s", key, out)
		}
	}
}

// TestApplyJobLLMProviderModel covers provider/model override correctness
// (including the provider:model form and serve-flag fallback) AND asserts the
// shared base config is never mutated (deep-copy isolation, as spawn_runner does).
func TestApplyJobLLMProviderModel(t *testing.T) {
	newBase := func() *config.Config {
		return &config.Config{
			DefaultProvider: "openai",
			Providers: map[string]config.ProviderConfig{
				"openai":     {Type: config.ProviderTypeOpenAI, Model: "gpt-x"},
				"claude-bin": {Type: config.ProviderTypeAnthropic, Model: "opus"},
			},
		}
	}

	cases := []struct {
		name          string
		cfg           jobsV2LLMConfig
		serveProvider string
		wantProvider  string
		wantModel     string
	}{
		{
			name:         "provider:model form",
			cfg:          jobsV2LLMConfig{Provider: "claude-bin:opus-max"},
			wantProvider: "claude-bin",
			wantModel:    "opus-max",
		},
		{
			name:         "provider only keeps its configured model",
			cfg:          jobsV2LLMConfig{Provider: "claude-bin"},
			wantProvider: "claude-bin",
			wantModel:    "opus",
		},
		{
			name:         "model only overrides the active provider's model",
			cfg:          jobsV2LLMConfig{Model: "gpt-y"},
			wantProvider: "openai",
			wantModel:    "gpt-y",
		},
		{
			name:         "provider and model combine (model wins as most specific)",
			cfg:          jobsV2LLMConfig{Provider: "claude-bin", Model: "sonnet"},
			wantProvider: "claude-bin",
			wantModel:    "sonnet",
		},
		{
			name:          "omitted provider falls back to the serve-level flag",
			cfg:           jobsV2LLMConfig{},
			serveProvider: "claude-bin:opus-max",
			wantProvider:  "claude-bin",
			wantModel:     "opus-max",
		},
		{
			name:         "no overrides preserves the base default",
			cfg:          jobsV2LLMConfig{},
			wantProvider: "openai",
			wantModel:    "gpt-x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := newBase()
			jobCfg := cloneConfigForServeJob(base)

			if err := applyJobLLMProviderModel(jobCfg, tc.cfg, tc.serveProvider, "", ""); err != nil {
				t.Fatalf("applyJobLLMProviderModel: %v", err)
			}

			if jobCfg.DefaultProvider != tc.wantProvider {
				t.Errorf("DefaultProvider = %q, want %q", jobCfg.DefaultProvider, tc.wantProvider)
			}
			if got := jobCfg.Providers[tc.wantProvider].Model; got != tc.wantModel {
				t.Errorf("active model = %q, want %q", got, tc.wantModel)
			}

			// Deep-copy isolation: the shared base config must be untouched.
			if base.DefaultProvider != "openai" {
				t.Errorf("base DefaultProvider mutated to %q", base.DefaultProvider)
			}
			if base.Providers["openai"].Model != "gpt-x" {
				t.Errorf("base openai model mutated to %q", base.Providers["openai"].Model)
			}
			if base.Providers["claude-bin"].Model != "opus" {
				t.Errorf("base claude-bin model mutated to %q", base.Providers["claude-bin"].Model)
			}
		})
	}
}

// TestCloneConfigForServeJobDeepCopiesProviderPointers asserts the clone does not
// alias the base config's per-provider slice/pointer fields, so per-run overrides
// can never leak into the shared base (mirrors spawn_runner.go's deep copy).
func TestCloneConfigForServeJobDeepCopiesProviderPointers(t *testing.T) {
	yes := true
	base := &config.Config{
		DefaultProvider: "p",
		Providers: map[string]config.ProviderConfig{
			"p": {Model: "m", Models: []string{"m"}, UseNativeSearch: &yes},
		},
	}

	clone := cloneConfigForServeJob(base)

	cp := clone.Providers["p"]
	if len(cp.Models) == 0 || cp.UseNativeSearch == nil {
		t.Fatalf("clone lost provider fields: %+v", cp)
	}
	cp.Models[0] = "MUTATED"
	*cp.UseNativeSearch = false
	clone.Providers["p"] = cp

	if base.Providers["p"].Models[0] != "m" {
		t.Errorf("base Models aliased the clone: %v", base.Providers["p"].Models)
	}
	if base.Providers["p"].UseNativeSearch == nil || *base.Providers["p"].UseNativeSearch != true {
		t.Errorf("base UseNativeSearch aliased the clone: %v", base.Providers["p"].UseNativeSearch)
	}
}

// TestResolveJobLLMSettingsThreadsAskOptions verifies the parity options reach the
// resolved settings / user prompt, and that omitted options fall back to the
// serve-level defaults (backward compatibility).
func TestResolveJobLLMSettingsThreadsAskOptions(t *testing.T) {
	cases := []struct {
		name   string
		cfg    jobsV2LLMConfig
		def    jobLLMServeDefaults
		verify func(t *testing.T, s SessionSettings, userPrompt string)
	}{
		{
			name: "max_turns overrides the default",
			cfg:  jobsV2LLMConfig{Instructions: "hi", MaxTurns: 400},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if s.MaxTurns != 400 {
					t.Errorf("MaxTurns = %d, want 400", s.MaxTurns)
				}
			},
		},
		{
			name: "max_turns omitted falls back to default 50",
			cfg:  jobsV2LLMConfig{Instructions: "hi"},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if s.MaxTurns != 50 {
					t.Errorf("MaxTurns = %d, want default 50", s.MaxTurns)
				}
			},
		},
		{
			name: "max_output_tokens reaches settings",
			cfg:  jobsV2LLMConfig{Instructions: "hi", MaxOutputTokens: 8192},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if s.MaxOutputTokens != 8192 {
					t.Errorf("MaxOutputTokens = %d, want 8192", s.MaxOutputTokens)
				}
			},
		},
		{
			name: "tools override",
			cfg:  jobsV2LLMConfig{Instructions: "hi", Tools: "shell,read_file"},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if s.Tools != "shell,read_file" {
					t.Errorf("Tools = %q, want %q", s.Tools, "shell,read_file")
				}
			},
		},
		{
			name: "tools omitted falls back to serve default",
			cfg:  jobsV2LLMConfig{Instructions: "hi"},
			def:  jobLLMServeDefaults{Tools: "glob"},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if s.Tools != "glob" {
					t.Errorf("Tools = %q, want serve default %q", s.Tools, "glob")
				}
			},
		},
		{
			name: "search enabled by the job",
			cfg:  jobsV2LLMConfig{Instructions: "hi", Search: true},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if !s.Search {
					t.Error("Search = false, want true")
				}
			},
		},
		{
			name: "search inherited from serve default",
			cfg:  jobsV2LLMConfig{Instructions: "hi"},
			def:  jobLLMServeDefaults{Search: true},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if !s.Search {
					t.Error("Search = false, want true (serve default)")
				}
			},
		},
		{
			name: "read_dir and write_dir extend the serve dirs",
			cfg:  jobsV2LLMConfig{Instructions: "hi", ReadDir: []string{"/job/read"}, WriteDir: []string{"/job/write"}},
			def:  jobLLMServeDefaults{ReadDirs: []string{"/serve/read"}, WriteDirs: []string{"/serve/write"}},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if !sliceHasString(s.ReadDirs, "/job/read") || !sliceHasString(s.ReadDirs, "/serve/read") {
					t.Errorf("ReadDirs = %v, want both serve and job read dirs", s.ReadDirs)
				}
				if !sliceHasString(s.WriteDirs, "/job/write") || !sliceHasString(s.WriteDirs, "/serve/write") {
					t.Errorf("WriteDirs = %v, want both serve and job write dirs", s.WriteDirs)
				}
			},
		},
		{
			name: "system_message override reaches the system prompt",
			cfg:  jobsV2LLMConfig{Instructions: "hi", SystemMessage: "CUSTOM-SYS"},
			verify: func(t *testing.T, s SessionSettings, _ string) {
				if !strings.Contains(s.SystemPrompt, "CUSTOM-SYS") {
					t.Errorf("SystemPrompt = %q, want it to contain the custom system message", s.SystemPrompt)
				}
			},
		},
		{
			name: "instructions become the user prompt verbatim",
			cfg:  jobsV2LLMConfig{Instructions: "summarize"},
			verify: func(t *testing.T, _ SessionSettings, userPrompt string) {
				if userPrompt != "summarize" {
					t.Errorf("userPrompt = %q, want the instructions verbatim", userPrompt)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings, userPrompt, err := resolveJobLLMSettings(&config.Config{}, nil, tc.cfg, tc.def)
			if err != nil {
				t.Fatalf("resolveJobLLMSettings: %v", err)
			}
			tc.verify(t, settings, userPrompt)
		})
	}
}

// TestJobsV2LLMSkillsFlagThreaded verifies the job's `skills` field is honored as
// the skills selector with precedence over the agent's skills — the contract the
// serve-jobs executor relies on when it passes cfg.Skills to SetupSkills (instead
// of the hard-coded "" used before this change). The disable path is fully
// deterministic, so a nil result proves the job value flowed through: were
// cfg.Skills ignored, the agent's "all" would have enabled skills.
func TestJobsV2LLMSkillsFlagThreaded(t *testing.T) {
	cases := []struct {
		name        string
		jobSkills   string
		agentSkills string
		cfg         config.SkillsConfig
		wantNil     bool
	}{
		{
			name:        "job none overrides agent all",
			jobSkills:   "none",
			agentSkills: "all",
			cfg:         config.SkillsConfig{Enabled: true},
			wantNil:     true,
		},
		{
			name:        "empty job falls back to agent none",
			jobSkills:   "",
			agentSkills: "none",
			cfg:         config.SkillsConfig{Enabled: true},
			wantNil:     true,
		},
		{
			name:      "empty job and agent uses config default (disabled)",
			jobSkills: "",
			cfg:       config.SkillsConfig{Enabled: false},
			wantNil:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			setup := SetupSkills(&cfg, tc.jobSkills, tc.agentSkills, io.Discard)
			if tc.wantNil && setup != nil {
				t.Fatalf("expected nil skills setup (job=%q agent=%q), got non-nil", tc.jobSkills, tc.agentSkills)
			}
		})
	}
}

// TestResolveJobLLMSettingsCwdRoutesToolRootsWithoutChdir asserts that cwd is
// routed into the read/write tool roots and the shell working dir, and that
// resolving never performs a process-wide os.Chdir.
func TestResolveJobLLMSettingsCwdRoutesToolRootsWithoutChdir(t *testing.T) {
	runCwd := t.TempDir()

	wd0, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	settings, _, err := resolveJobLLMSettings(&config.Config{}, nil, jobsV2LLMConfig{
		Instructions: "do work",
		Cwd:          runCwd,
	}, jobLLMServeDefaults{})
	if err != nil {
		t.Fatalf("resolveJobLLMSettings: %v", err)
	}

	if settings.ShellWorkingDir != runCwd {
		t.Errorf("ShellWorkingDir = %q, want cwd %q", settings.ShellWorkingDir, runCwd)
	}
	if !sliceHasString(settings.ReadDirs, runCwd) {
		t.Errorf("ReadDirs = %v, want it to include cwd %q", settings.ReadDirs, runCwd)
	}
	if !sliceHasString(settings.WriteDirs, runCwd) {
		t.Errorf("WriteDirs = %v, want it to include cwd %q", settings.WriteDirs, runCwd)
	}

	wd1, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if wd0 != wd1 {
		t.Fatalf("process working dir changed (os.Chdir leaked): %q -> %q", wd0, wd1)
	}
}

// TestJobsV2LLMJobCwdRootsShellExecutionWithoutChdir is the focused integration
// test from the design: a program-equivalent task expressed as a native `llm`
// job with cwd set produces output rooted in that dir — and the run does not
// mutate the process working directory (no os.Chdir).
//
// It drives the real serve-jobs executor with the hermetic debug provider and a
// temp agent that enables the shell tool. The debug provider turns the
// instruction "shell touch <file>" into a shell tool call with no working_dir, so
// the file is created only if the shell tool is rooted at cwd via exec.Cmd.Dir.
func TestJobsV2LLMJobCwdRootsShellExecutionWithoutChdir(t *testing.T) {
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"),
		[]byte("name: cwd-probe\ndescription: probe shell cwd\ntools:\n  enabled: [shell]\n"), 0o644); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}

	runCwd := t.TempDir()
	const sentinel = "rooted_here.txt"

	// Auto-approve shell so the run is non-interactive.
	prevYolo := serveYolo
	serveYolo = true
	defer func() { serveYolo = prevYolo }()

	wd0, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	persist := false
	executor := newServeJobsExecutor(&config.Config{})
	if _, err := executor(context.Background(), jobsV2LLMConfig{
		AgentName:      agentDir, // contains a path separator → loaded from disk
		Instructions:   "shell touch " + sentinel,
		Provider:       "debug",
		Cwd:            runCwd,
		PersistSession: &persist,
	}, nil); err != nil {
		t.Fatalf("executor returned error: %v", err)
	}

	wd1, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if wd0 != wd1 {
		t.Fatalf("process working dir changed across run (os.Chdir leaked): %q -> %q", wd0, wd1)
	}

	if _, err := os.Stat(filepath.Join(runCwd, sentinel)); err != nil {
		t.Fatalf("expected shell rooted in cwd %q to create %q: %v", runCwd, sentinel, err)
	}
}
