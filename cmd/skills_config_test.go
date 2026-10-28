package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestProjectSkillToolsCobraConfigPrecedence(t *testing.T) {
	const flagName = "allow-project-skill-tools"
	for _, name := range []string{"chat", "ask", "serve"} {
		for _, before := range []bool{false, true} {
			for _, configured := range []bool{false, true} {
				for _, flag := range []string{"", "--allow-project-skill-tools", "--allow-project-skill-tools=true", "--allow-project-skill-tools=false"} {
					t.Run(fmt.Sprintf("%s/pre=%t/config=%t/%s", name, before, configured, flag), func(t *testing.T) {
						cleanupPreCommandParseState(t)
						oldServe := serveFlags
						t.Cleanup(func() { serveFlags = oldServe })
						writeServeContractConfig(t, fmt.Sprintf("default_provider: debug\nskills:\n  allow_project_script_tools: %t\n  include_project_skills: false\n", configured))
						command, _, err := rootCmd.Find([]string{name})
						if err != nil {
							t.Fatal(err)
						}
						bound := command.Flags().Lookup(flagName)
						if bound == nil || bound.DefValue != "false" || !strings.Contains(bound.Usage, "without approval") || !strings.Contains(bound.Usage, "yolo") {
							t.Fatalf("missing or incorrect project skill flag: %#v", bound)
						}
						oldValue := bound.Value.String()
						t.Cleanup(func() { _ = bound.Value.Set(oldValue) })
						bound.Changed = false
						args := []string{name}
						if flag != "" {
							if before {
								args = []string{flag, name}
							} else {
								args = append(args, flag)
							}
						}
						command, _, err = parseNormalizedArgsWithCobraForTest(normalizePreCommandFlags(args))
						if err != nil {
							t.Fatal(err)
						}
						if command.Name() != name || bound.Changed != (flag != "") {
							t.Fatalf("command=%s changed=%t, want %s changed=%t", command.Name(), bound.Changed, name, flag != "")
						}
						want := configured
						if flag != "" {
							want = !strings.HasSuffix(flag, "=false")
						}
						var load func() (*config.Config, error)
						switch name {
						case "chat":
							_, deps := captureChatInvocation(command)
							load = deps.LoadConfig
						case "ask":
							_, deps := captureAskInvocation(command)
							load = deps.LoadConfig
						case "serve":
							serveFlags.Token = "contract-token"
							invocation := captureServeInvocation(command, &serveFlags)
							stop := errors.New("stop after loading effective config")
							var loaded *config.Config
							invocation.ResolveApproval = func(cfg *config.Config) (resolvedApprovalMode, error) {
								loaded = cfg
								return resolvedApprovalMode{}, stop
							}
							load = func() (*config.Config, error) {
								err := appserve.Run(context.Background(), &serveFlags, []string{"web"}, invocation)
								if !errors.Is(err, stop) {
									return nil, fmt.Errorf("startup did not reach config checkpoint: %v", err)
								}
								return loaded, nil
							}
						}
						// A captured invocation, including later rebuilds, must not read
						// mutated Cobra state or mistake an absent flag for false.
						if err := command.Flags().Set(flagName, strconv.FormatBool(!want)); err != nil {
							t.Fatal(err)
						}
						for range 2 {
							cfg, err := load()
							if err != nil {
								t.Fatal(err)
							}
							if cfg.Skills.AllowProjectScriptTools != want || cfg.Skills.IncludeProjectSkills {
								t.Fatalf("effective skills config = %+v, want script tools %t and project discovery false", cfg.Skills, want)
							}
						}
						persisted, err := config.Load()
						if err != nil || persisted.Skills.AllowProjectScriptTools != configured {
							t.Fatalf("CLI override changed persisted config: cfg=%+v err=%v", persisted, err)
						}
					})
				}
			}
		}
	}
}

func TestProjectSkillToolsConfigSetRoundTrip(t *testing.T) {
	writeServeContractConfig(t, "default_provider: debug\n")
	for _, value := range []string{"true", "false", "true"} {
		if err := configSet(configCmd, []string{"skills.allow_project_script_tools", value}); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if strconv.FormatBool(cfg.Skills.AllowProjectScriptTools) != value {
			t.Fatalf("config set %s round-trip = %t", value, cfg.Skills.AllowProjectScriptTools)
		}
	}
}

func TestProjectSkillToolsConfigLoaderError(t *testing.T) {
	want := errors.New("config error")
	load := projectSkillToolsConfigLoader(nil, func() (*config.Config, error) { return nil, want })
	if cfg, err := load(); cfg != nil || !errors.Is(err, want) {
		t.Fatalf("loader = %v, %v, want nil, %v", cfg, err, want)
	}
}
