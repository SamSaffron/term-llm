package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func runEnvApprovalShell(t *testing.T, tool *ShellTool, args any, wantError string) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), raw)
	want := wantError
	if want == "" {
		want = "exit_code: 0"
	}
	if err != nil || out.IsError != (wantError != "") || !strings.Contains(out.Content, want) {
		t.Errorf("Execute() = %#v, %v; want %q", out, err, want)
	}
}

// Decode, never execute approval text: loader-sensitive cases must not run.
func assertEnvApprovalCommand(t *testing.T, got, shell, command string, env EnvMap) {
	t.Helper()
	shell, err := exec.LookPath(shell)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"env", "--"}
	for _, key := range keys {
		want = append(want, key+"="+env[key])
	}
	want = append(want, shell, "-c", command)
	argv, err := splitShellWords(got)
	if err != nil || !reflect.DeepEqual(argv, want) || !strings.HasPrefix(got, "env -- '") || !strings.Contains(got, "'"+shell+"' -c '") {
		t.Errorf("approval = %q; argv = %#v, error = %v; want quoted %#v", got, argv, err, want)
	}
}

func assertEnvApprovalFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unapproved marker %q exists or cannot be inspected: %v", path, err)
	}
}

func TestShellToolEnvApprovalGuardianSeesCriticalOverrides(t *testing.T) {
	for _, key := range []string{"BASH_ENV", "ENV", "PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "PYTHONPATH", "NODE_OPTIONS", "PERL5OPT", "RUBYOPT", "GIT_CONFIG_GLOBAL", "BASH_FUNC_env_approval_probe%%"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			args := ShellArgs{Command: "printf executed > command-marker", WorkingDir: dir, Env: EnvMap{key: filepath.Join(dir, "override with spaces ' and $dollar; not-a-loader")}}
			mgr := newApprovalAutoTestManager(NewToolPermissions())
			mgr.SetApprovalMode(ModeAuto)
			tool := NewShellTool(mgr, nil, DefaultOutputLimits())
			reviews := 0
			mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
				reviews++
				if req.WorkDir != dir || req.ApprovalScope != "local" {
					t.Errorf("review request = %#v", req)
				}
				assertEnvApprovalCommand(t, req.Command, tool.shellPath, args.Command, args.Env)
				return PolicyDecision{Allowed: false, Rationale: "override not authorized"}, nil
			}, nil)
			mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
				t.Error("Guardian denial reached human UI")
				return ApprovalResult{Choice: ApprovalChoiceCancelled, Cancelled: true}, nil
			}
			mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
				t.Error("Guardian denial reached legacy prompt")
				return Cancel, ""
			}
			runEnvApprovalShell(t, tool, args, "PERMISSION_DENIED")
			if reviews != 1 {
				t.Errorf("Guardian reviews = %d, want 1", reviews)
			}
			assertEnvApprovalFileAbsent(t, filepath.Join(dir, "command-marker"))
		})
	}
}

func TestShellToolEnvApprovalExcludesInheritedSecrets(t *testing.T) {
	const inheritedKey = "TERM_LLM_TEST_INHERITED_API_TOKEN"
	const inheritedSecret = "test-only-inherited-secret-not-for-review"
	const shadowedSecret = "test-only-shadowed-secret-not-for-review"
	t.Setenv(inheritedKey, inheritedSecret)
	t.Setenv("MODEL_OVERRIDE", shadowedSecret)
	for _, reviewer := range []string{"Guardian", "UI", "legacy"} {
		for _, withOverride := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/override=%t", reviewer, withOverride), func(t *testing.T) {
				mgr := newApprovalAutoTestManager(NewToolPermissions())
				tool := NewShellTool(mgr, nil, DefaultOutputLimits())
				args := ShellArgs{Command: "printf benign", WorkingDir: t.TempDir()}
				if withOverride {
					args.Env = EnvMap{"MODEL_OVERRIDE": "agent-supplied-value"}
				}
				reviews := 0
				observe := func(request any, command string) {
					reviews++
					evidence := fmt.Sprintf("%#v", request)
					for _, secret := range []string{inheritedKey, inheritedSecret, shadowedSecret} {
						if strings.Contains(evidence, secret) {
							t.Error("inherited environment leaked into approval evidence")
						}
					}
					if withOverride {
						assertEnvApprovalCommand(t, command, tool.shellPath, args.Command, args.Env)
					} else if command != args.Command {
						t.Error("no-overrides approval must contain only the original command")
					}
				}
				switch reviewer {
				case "Guardian":
					mgr.SetApprovalMode(ModeAuto)
					mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
						observe(req, req.Command)
						return PolicyDecision{Allowed: false, Rationale: "test denial"}, nil
					}, nil)
				case "UI":
					mgr.PromptUIFunc = func(command string, _, _ bool, workDir string) (ApprovalResult, error) {
						observe([]string{command, workDir}, command)
						return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
					}
				case "legacy":
					mgr.PromptFunc = func(req *ApprovalRequest) (ConfirmOutcome, string) {
						observe(req, req.Command)
						return Cancel, ""
					}
				}
				runEnvApprovalShell(t, tool, args, "PERMISSION_DENIED")
				if reviews != 1 {
					t.Errorf("reviews = %d, want 1", reviews)
				}
			})
		}
	}
}

func TestShellToolEnvApprovalHumanSeesExactEscapedValues(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "UI", true: "legacy"}[legacy], func(t *testing.T) {
			dir := t.TempDir()
			args := ShellArgs{Command: "printf '%s' 'raw $command; with quotes' > command-marker", WorkingDir: dir, Env: EnvMap{
				"Z_LAST": "line one\nline two\t\\end", "A_FIRST": "a'b\"c $(never-run); `never-run` * [x]", "EMPTY": "", "EQUALS": "left=right", "UNICODE": "héllo 世界",
			}}
			mgr := newApprovalAutoTestManager(NewToolPermissions())
			tool := NewShellTool(mgr, nil, DefaultOutputLimits())
			prompts := 0
			observe := func(command string) {
				prompts++
				assertEnvApprovalCommand(t, command, tool.shellPath, args.Command, args.Env)
			}
			if legacy {
				mgr.PromptFunc = func(req *ApprovalRequest) (ConfirmOutcome, string) {
					observe(req.Command)
					return Cancel, ""
				}
			} else {
				mgr.PromptUIFunc = func(command string, isWrite, isShell bool, workDir string) (ApprovalResult, error) {
					observe(command)
					if isWrite || !isShell || workDir != dir {
						t.Error("incorrect shell prompt metadata")
					}
					return ApprovalResult{Choice: ApprovalChoiceCancelled, Cancelled: true}, nil
				}
			}
			runEnvApprovalShell(t, tool, args, "PERMISSION_DENIED")
			if prompts != 1 {
				t.Errorf("human prompts = %d, want 1", prompts)
			}
			assertEnvApprovalFileAbsent(t, filepath.Join(dir, "command-marker"))
		})
	}
}

func TestShellToolEnvApprovalCommandOnlyGrantsDoNotBypassReview(t *testing.T) {
	for _, mode := range []ApprovalMode{ModeAuto, ModePrompt} {
		t.Run(map[ApprovalMode]string{ModeAuto: "auto", ModePrompt: "prompt"}[mode], func(t *testing.T) {
			for _, source := range []string{"configured", "session", "ancestor", "project", "session exact", "Guardian exact", "configured env pattern", "session env pattern"} {
				t.Run(source, func(t *testing.T) {
					const command = "printf benign"
					mgr := newApprovalAutoTestManager(NewToolPermissions())
					mgr.SetApprovalMode(mode)
					dir := t.TempDir()
					switch source {
					case "configured", "session", "ancestor", "project":
						mgr, dir = newPatternApprovalTestManager(t, mode, source, "printf *")
					case "session exact":
						if err := mgr.shellCache.AddCommand(command, dir); err != nil {
							t.Fatal(err)
						}
					case "Guardian exact":
						mgr.SetApprovalMode(ModeAuto)
						mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
							return PolicyDecision{Allowed: true, Rationale: "raw command only"}, nil
						}, nil)
						runEnvApprovalShell(t, NewShellTool(mgr, nil, DefaultOutputLimits()), ShellArgs{Command: command, WorkingDir: dir}, "")
					case "configured env pattern", "session env pattern":
						add := mgr.permissions.AddShellPattern
						if source == "session env pattern" {
							add = mgr.ApproveShellPattern
						}
						if err := add("env *"); err != nil {
							t.Fatal(err)
						}
					}
					mgr.SetApprovalMode(mode)
					args := ShellArgs{Command: command, WorkingDir: dir, Env: EnvMap{"ENV_APPROVAL_PROBE": "ordinary safe value"}}
					tool := NewShellTool(mgr, nil, DefaultOutputLimits())
					reviews := 0
					mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
						reviews++
						assertEnvApprovalCommand(t, req.Command, tool.shellPath, args.Command, args.Env)
						return PolicyDecision{Allowed: false, Rationale: "env needs its own review"}, nil
					}, nil)
					mgr.PromptUIFunc = func(command string, _, _ bool, workDir string) (ApprovalResult, error) {
						reviews++
						if mode == ModeAuto || workDir != dir {
							t.Error("unexpected human prompt or workdir")
						}
						assertEnvApprovalCommand(t, command, tool.shellPath, args.Command, args.Env)
						return ApprovalResult{Choice: ApprovalChoiceCancelled, Cancelled: true}, nil
					}
					runEnvApprovalShell(t, tool, args, "PERMISSION_DENIED")
					if reviews != 1 {
						t.Errorf("environment reviews = %d, want 1 despite %s grant", reviews, source)
					}
				})
			}
		})
	}
}

func TestShellToolEnvApprovalGuardianCachesOnlyExactIdentity(t *testing.T) {
	mgr := newApprovalAutoTestManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	tool := NewShellTool(mgr, nil, DefaultOutputLimits())
	dir := t.TempDir()
	base := ShellArgs{Command: "printf '%s' \"$A_FIRST:$Z_LAST\"", WorkingDir: dir, Env: EnvMap{"Z_LAST": "two", "A_FIRST": "one"}}
	reviews := 0
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		reviews++
		return PolicyDecision{Allowed: true, Rationale: "explicit exact environment approval"}, nil
	}, nil)
	runEnvApprovalShell(t, tool, base, "")
	commandJSON, _ := json.Marshal(base.Command)
	dirJSON, _ := json.Marshal(dir)
	// Deliberately reverse JSON order, then use the strict-provider array form.
	for _, envJSON := range []string{`{"Z_LAST":"two","A_FIRST":"one"}`, `[{"key":"Z_LAST","value":"two"},{"key":"A_FIRST","value":"one"}]`} {
		raw := json.RawMessage(`{"command":` + string(commandJSON) + `,"working_dir":` + string(dirJSON) + `,"env":` + envJSON + `}`)
		runEnvApprovalShell(t, tool, raw, "")
	}
	if reviews != 1 {
		t.Errorf("equivalent reordered/array overrides caused %d reviews, want 1", reviews)
	}
	for _, change := range []string{"value", "key", "command", "workdir", "removed env"} {
		args := ShellArgs{Command: base.Command, WorkingDir: dir, Env: EnvMap{"Z_LAST": "two", "A_FIRST": "one"}}
		switch change {
		case "value":
			args.Env["A_FIRST"] = "different"
		case "key":
			delete(args.Env, "A_FIRST")
			args.Env["B_OTHER"] = "one"
		case "command":
			args.Command = "printf changed"
		case "workdir":
			args.WorkingDir = t.TempDir()
		case "removed env":
			args.Env = nil
		}
		before := reviews
		runEnvApprovalShell(t, tool, args, "")
		if reviews != before+1 {
			t.Errorf("changing %s caused %d reviews, want 1", change, reviews-before)
		}
	}
}

func TestShellToolEnvApprovalPatternChoiceStaysExactAndSessionOnly(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "UI", true: "legacy"}[legacy], func(t *testing.T) {
			mgr, dir := newPatternApprovalTestManager(t, ModePrompt, "project")
			project := mgr.projectCache[DetectGitRepo(dir).Root]
			project.filePath = filepath.Join(t.TempDir(), "project-approvals.yaml")
			tool := NewShellTool(mgr, nil, DefaultOutputLimits())
			args := ShellArgs{Command: "printf '%s' \"$ENV_APPROVAL_PROBE\"", WorkingDir: dir, Env: EnvMap{"ENV_APPROVAL_PROBE": "allowed * [x]"}}
			prompts := 0
			if legacy {
				mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
					prompts++
					if prompts == 1 {
						return ProceedAlwaysAndSave, "env *"
					}
					return Cancel, ""
				}
			} else {
				mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
					prompts++
					if prompts == 1 {
						return ApprovalResult{Choice: ApprovalChoicePattern, Pattern: "env *", SaveToRepo: true}, nil
					}
					return ApprovalResult{Choice: ApprovalChoiceCancelled, Cancelled: true}, nil
				}
			}
			runEnvApprovalShell(t, tool, args, "")
			runEnvApprovalShell(t, tool, args, "")
			if prompts != 1 || len(mgr.shellCache.GetPatterns()) != 0 || len(project.shellPatternsSnapshot()) != 0 {
				t.Errorf("env pattern choice broadened authority or missed exact cache: prompts=%d, session=%v, project=%v", prompts, mgr.shellCache.GetPatterns(), project.shellPatternsSnapshot())
			}
			assertEnvApprovalFileAbsent(t, project.filePath)
			for _, changed := range []ShellArgs{
				{Command: args.Command, WorkingDir: dir, Env: EnvMap{"ENV_APPROVAL_PROBE": "allowed expanded x"}},
				{Command: "printf changed", WorkingDir: dir, Env: args.Env},
				{Command: args.Command, WorkingDir: t.TempDir(), Env: args.Env},
				{Command: args.Command, WorkingDir: dir},
			} {
				before := prompts
				runEnvApprovalShell(t, tool, changed, "PERMISSION_DENIED")
				if prompts != before+1 {
					t.Errorf("changed invocation did not prompt: %#v", changed)
				}
			}
		})
	}
}

func TestShellToolEnvApprovalRejectsInvalidEnvironmentBeforeApproval(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{{"empty key", "", "value"}, {"equals in key", "BAD=KEY", "value"}, {"NUL in key", "BAD\x00KEY", "value"}, {"NUL in value", "VALID_KEY", "bad\x00value"}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []ApprovalMode{ModeAuto, ModePrompt} {
				mgr := newApprovalAutoTestManager(NewToolPermissions())
				mgr.SetApprovalMode(mode)
				mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
					t.Error("invalid environment reached Guardian")
					return PolicyDecision{Allowed: false}, nil
				}, nil)
				mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
					t.Error("invalid environment reached human prompt")
					return ApprovalResult{Choice: ApprovalChoiceCancelled, Cancelled: true}, nil
				}
				dir := t.TempDir()
				for _, env := range []any{EnvMap{tc.key: tc.value}, []map[string]string{{"key": tc.key, "value": tc.value}}} {
					args := map[string]any{"command": "printf executed > command-marker", "working_dir": dir, "env": env}
					runEnvApprovalShell(t, NewShellTool(mgr, nil, DefaultOutputLimits()), args, "INVALID_PARAMS")
				}
				assertEnvApprovalFileAbsent(t, filepath.Join(dir, "command-marker"))
			}
		})
	}
}

func TestShellToolEnvApprovalBlocksStartupAndPathPayloadUntilExplicitApproval(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	for _, kind := range []string{"BASH_ENV", "PATH"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			payload, command := filepath.Join(dir, "startup.sh"), "printf benign"
			if kind == "PATH" {
				payload, command = filepath.Join(dir, "env-approval-shadow"), "env-approval-shadow"
			}
			// Payloads only write a marker in this test's temporary directory.
			if err := os.WriteFile(payload, []byte("#!/bin/sh\nprintf payload-ran > payload-marker\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			value := payload
			if kind == "PATH" {
				value = dir
			}
			mgr := newApprovalAutoTestManager(NewToolPermissions())
			mgr.SetApprovalMode(ModeAuto)
			if err := mgr.shellCache.AddCommand(command, dir); err != nil {
				t.Fatal(err)
			}
			tool := NewShellTool(mgr, nil, DefaultOutputLimits())
			tool.shellPath = bash // Pin bash for BASH_ENV, not the ambient shell.
			args := ShellArgs{Command: command, WorkingDir: dir, Env: EnvMap{kind: value}}
			reviews, allow := 0, false
			mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
				reviews++
				assertEnvApprovalCommand(t, req.Command, bash, command, args.Env)
				return PolicyDecision{Allowed: allow, Rationale: "explicit override decision"}, nil
			}, nil)
			runEnvApprovalShell(t, tool, args, "PERMISSION_DENIED")
			assertEnvApprovalFileAbsent(t, filepath.Join(dir, "payload-marker"))
			allow = true
			runEnvApprovalShell(t, tool, args, "")
			data, err := os.ReadFile(filepath.Join(dir, "payload-marker"))
			if reviews != 2 || err != nil || string(data) != "payload-ran" {
				t.Errorf("explicit approval: reviews=%d, marker=%q, error=%v", reviews, data, err)
			}
		})
	}
}

func TestShellToolEnvApprovalEmptyEnvironmentKeepsRawCommandAPI(t *testing.T) {
	for _, env := range []any{nil, EnvMap{}, []map[string]string{}} {
		mgr := newApprovalAutoTestManager(NewToolPermissions())
		mgr.SetApprovalMode(ModeAuto)
		dir := t.TempDir()
		const command = "printf unchanged"
		reviews := 0
		mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
			reviews++
			if req.Command != command || req.WorkDir != dir {
				t.Errorf("no-env review changed existing API: %#v", req)
			}
			return PolicyDecision{Allowed: true}, nil
		}, nil)
		tool := NewShellTool(mgr, nil, DefaultOutputLimits())
		args := map[string]any{"command": command, "working_dir": dir, "env": env}
		runEnvApprovalShell(t, tool, args, "")
		runEnvApprovalShell(t, tool, args, "")
		if reviews != 1 {
			t.Errorf("no-env exact approval reviews = %d, want 1", reviews)
		}
	}
}
