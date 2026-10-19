package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestServeMCPOptionsValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts serveMCPOptions
		want string
	}{
		{"tools required", serveMCPOptions{}, "--tools is required"},
		{"port range", serveMCPOptions{Tools: "all", Port: 0}, "invalid --port"},
		{"edit format", serveMCPOptions{Tools: "all", Port: 8080, EditFormat: "unknown"}, "invalid --edit-format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runServeMCPWithOptions(context.Background(), appserve.MCPInvocation{}, nil, &tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runServeMCPWithOptions() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServeMCPValidationConfigApprovalOrder(t *testing.T) {
	approvalFailure := errors.New("deferred approval failure")
	for _, tc := range []struct {
		name, config, tools, want string
		resolve                   func(*config.Config) (resolvedApprovalMode, error)
		wantResolver              bool
	}{
		{
			name: "validation before config", config: "serve: [", want: "--tools is required",
			resolve: func(*config.Config) (resolvedApprovalMode, error) {
				t.Fatal("resolver called after validation failure")
				return resolvedApprovalMode{}, nil
			},
		},
		{
			name: "config before approval", config: "serve: [", tools: "all", want: "failed to read config:",
			resolve: func(*config.Config) (resolvedApprovalMode, error) {
				t.Fatal("resolver called after config failure")
				return resolvedApprovalMode{}, nil
			},
		},
		{
			name: "approval after config", config: "default_provider: debug\n", tools: "all", want: approvalFailure.Error(), wantResolver: true,
			resolve: func(*config.Config) (resolvedApprovalMode, error) { return resolvedApprovalMode{}, approvalFailure },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeServeContractConfig(t, tc.config)
			called := false
			invocation := appserve.MCPInvocation{Stdout: io.Discard, Stderr: io.Discard, ResolveApproval: func(cfg *config.Config) (resolvedApprovalMode, error) {
				called = true
				return tc.resolve(cfg)
			}}
			opts := serveMCPOptions{Tools: tc.tools, Host: "127.0.0.1", Port: 8080, EditFormat: "edit_file"}
			err := runServeMCPWithOptions(context.Background(), invocation, nil, &opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MCP startup error = %v, want %q", err, tc.want)
			}
			if called != tc.wantResolver {
				t.Fatalf("approval resolver called = %v, want %v", called, tc.wantResolver)
			}
		})
	}
}

func TestResolveServeMCPApprovalModeAcceptsNormalizedParentFlagAndDetectsCrossLevelConflict(t *testing.T) {
	parentApproval, childApproval := serveCmd.Flags().Lookup("approval"), serveMCPCmd.Flags().Lookup("approval")
	parentAuto, parentYolo := serveCmd.Flags().Lookup("auto"), serveCmd.Flags().Lookup("yolo")
	childAuto, childYolo := serveMCPCmd.Flags().Lookup("auto"), serveMCPCmd.Flags().Lookup("yolo")
	flags := []*pflag.Flag{parentApproval, parentAuto, parentYolo, childApproval, childAuto, childYolo}
	changed := make([]bool, len(flags))
	for i, flag := range flags {
		changed[i] = flag.Changed
		flag.Changed = false
	}
	oldServeApproval, oldMCPApproval := serveFlags.Approval, serveMCPFlags.Approval
	t.Cleanup(func() {
		serveFlags.Approval, serveMCPFlags.Approval = oldServeApproval, oldMCPApproval
		for i, flag := range flags {
			flag.Changed = changed[i]
		}
	})

	serveFlags.Approval = "auto"
	parentApproval.Changed = true
	got, err := resolveServeMCPApprovalMode(serveMCPCmd, &config.Config{Serve: config.ServeConfig{MCP: config.ServeMCPConfig{ApprovalMode: "prompt"}}}, &serveMCPFlags)
	if err != nil {
		t.Fatalf("resolve parent-normalized approval: %v", err)
	}
	if got.Mode != tools.ModeAuto || got.Source != approvalModeSourceCLI {
		t.Fatalf("resolved = {%v %s}, want CLI auto", got.Mode, got.Source)
	}

	serveMCPFlags.Approval = "prompt"
	childApproval.Changed = true
	if _, err := resolveServeMCPApprovalMode(serveMCPCmd, &config.Config{}, &serveMCPFlags); err == nil || !strings.Contains(err.Error(), "mutually exclusive across serve and serve mcp") {
		t.Fatalf("cross-level conflict error = %v", err)
	}
}

func TestServeMCPApprovalArbitrationContract(t *testing.T) {
	// Exercise the real parent/child bindings. Pre-command normalization can
	// leave a mode on serveCmd; ordinary `serve mcp --approval ...` binds the
	// child. Neither level may silently override an explicit choice on the other.
	oldParent, oldChild := serveFlags, serveMCPFlags
	flags := []*pflag.Flag{}
	for _, cmd := range []*cobra.Command{serveCmd, serveMCPCmd} {
		for _, name := range []string{"approval", "auto", "yolo"} {
			flags = append(flags, cmd.Flags().Lookup(name))
		}
	}
	changed := make([]bool, len(flags))
	for i, flag := range flags {
		changed[i] = flag.Changed
	}
	t.Cleanup(func() {
		serveFlags, serveMCPFlags = oldParent, oldChild
		for i, flag := range flags {
			flag.Changed = changed[i]
		}
	})
	const crossLevel = "--approval, --auto, and --yolo are mutually exclusive across serve and serve mcp"
	const sameLevel = "--approval, --auto, and --yolo are mutually exclusive"
	for _, tc := range []struct {
		name                           string
		parent, child                  []string
		mcpMode, serveMode, globalMode string
		want                           tools.ApprovalMode
		source                         approvalModeSource
		wantErr                        string
	}{
		{name: "neither uses MCP builtin prompt", want: tools.ModePrompt, source: approvalModeSourceBuiltinDefault},
		{name: "neither ignores ordinary serve config", serveMode: "auto", want: tools.ModePrompt, source: approvalModeSourceBuiltinDefault},
		{name: "neither uses global config", globalMode: "auto", want: tools.ModeAuto, source: approvalModeSourceGlobalConfig},
		{name: "neither uses MCP config", mcpMode: "auto", want: tools.ModeAuto, source: approvalModeSourceSurfaceConfig},
		{name: "MCP config overrides global", mcpMode: "prompt", globalMode: "auto", want: tools.ModePrompt, source: approvalModeSourceSurfaceConfig},
		{name: "parent only approval", parent: []string{"--approval=auto"}, mcpMode: "prompt", want: tools.ModeAuto, source: approvalModeSourceCLI},
		{name: "parent explicit prompt overrides config", parent: []string{"--approval=prompt"}, mcpMode: "auto", want: tools.ModePrompt, source: approvalModeSourceCLI},
		{name: "parent only auto", parent: []string{"--auto"}, want: tools.ModeAuto, source: approvalModeSourceCLI},
		{name: "parent only yolo", parent: []string{"--yolo"}, want: tools.ModeYolo, source: approvalModeSourceCLI},
		{name: "child only approval", child: []string{"--approval=auto"}, mcpMode: "prompt", want: tools.ModeAuto, source: approvalModeSourceCLI},
		{name: "child explicit prompt overrides config", child: []string{"--approval=prompt"}, mcpMode: "auto", want: tools.ModePrompt, source: approvalModeSourceCLI},
		{name: "child only auto", child: []string{"--auto"}, want: tools.ModeAuto, source: approvalModeSourceCLI},
		{name: "child only yolo", child: []string{"--yolo"}, want: tools.ModeYolo, source: approvalModeSourceCLI},
		{name: "child approval is normalized", child: []string{"--approval= AUTO "}, want: tools.ModeAuto, source: approvalModeSourceCLI},
		{name: "both same mode still conflict", parent: []string{"--approval=auto"}, child: []string{"--approval=auto"}, wantErr: crossLevel},
		{name: "parent approval child alias conflict", parent: []string{"--approval=prompt"}, child: []string{"--yolo"}, wantErr: crossLevel},
		{name: "parent alias child approval conflict", parent: []string{"--auto"}, child: []string{"--approval=prompt"}, wantErr: crossLevel},
		{name: "parent auto false is explicit", parent: []string{"--auto=false"}, wantErr: "--auto=false is not supported; use --approval prompt"},
		{name: "child auto false is explicit", child: []string{"--auto=false"}, wantErr: "--auto=false is not supported; use --approval prompt"},
		{name: "parent yolo false is explicit", parent: []string{"--yolo=false"}, wantErr: "--yolo=false is not supported; use --approval prompt"},
		{name: "child yolo false is explicit", child: []string{"--yolo=false"}, wantErr: "--yolo=false is not supported; use --approval prompt"},
		{name: "both false conflict before unsupported false", parent: []string{"--auto=false"}, child: []string{"--yolo=false"}, wantErr: crossLevel},
		{name: "child false conflicts with parent", parent: []string{"--approval=auto"}, child: []string{"--auto=false"}, wantErr: crossLevel},
		{name: "parent same-level conflict", parent: []string{"--auto", "--yolo"}, wantErr: sameLevel},
		{name: "child same-level conflict", child: []string{"--approval=prompt", "--auto"}, wantErr: sameLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, flag := range flags {
				flag.Changed = false
			}
			serveFlags.Approval, serveFlags.Auto, serveFlags.Yolo = "", false, false
			serveMCPFlags.Approval, serveMCPFlags.Auto, serveMCPFlags.Yolo = "", false, false
			if err := serveCmd.ParseFlags(tc.parent); err != nil {
				t.Fatal(err)
			}
			if err := serveMCPCmd.ParseFlags(tc.child); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Approval: config.ApprovalConfig{DefaultMode: tc.globalMode}}
			cfg.Serve.ApprovalMode, cfg.Serve.MCP.ApprovalMode = tc.serveMode, tc.mcpMode
			got, err := resolveServeMCPApprovalMode(serveMCPCmd, cfg, &serveMCPFlags)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("arbitration error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.Mode != tc.want || got.Source != tc.source {
				t.Fatalf("arbitration = {%v %s}, err=%v; want {%v %s}", got.Mode, got.Source, err, tc.want, tc.source)
			}
		})
	}
}

func TestServeMCPToolResultPreservesFailureStatus(t *testing.T) {
	tests := []struct {
		name string
		out  core.ToolOutput
		want bool
	}{
		{name: "success", out: core.TextOutput("ok")},
		{name: "semantic error", out: core.ToolOutput{Content: "failed", IsError: true}, want: true},
		{name: "timeout", out: core.ToolOutput{Content: "timed out", TimedOut: true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serveMCPToolResult(tt.out)
			if got.Content != tt.out.Content || got.IsError != tt.want {
				t.Fatalf("serve MCP result = %#v, want content %q IsError %v", got, tt.out.Content, tt.want)
			}
		})
	}
}

func TestParseMCPToolsFlag(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "all expands to full list",
			input:    "all",
			expected: mcpAllToolNames(),
		},
		{
			name:     "star expands to full list",
			input:    "*",
			expected: mcpAllToolNames(),
		},
		{
			name:     "single tool",
			input:    "read_file",
			expected: []string{"read_file"},
		},
		{
			name:     "comma separated",
			input:    "read_file,grep,glob",
			expected: []string{"read_file", "grep", "glob"},
		},
		{
			name:     "deduplication",
			input:    "read_file,grep,read_file",
			expected: []string{"read_file", "grep"},
		},
		{
			name:     "whitespace trimming",
			input:    " read_file , grep ",
			expected: []string{"read_file", "grep"},
		},
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "all with whitespace",
			input:    " all ",
			expected: mcpAllToolNames(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseMCPToolsFlag(tc.input)
			if len(result) != len(tc.expected) {
				t.Fatalf("parseMCPToolsFlag(%q) returned %d items, want %d\ngot:  %v\nwant: %v",
					tc.input, len(result), len(tc.expected), result, tc.expected)
			}
			for i := range result {
				if result[i] != tc.expected[i] {
					t.Errorf("parseMCPToolsFlag(%q)[%d] = %q, want %q", tc.input, i, result[i], tc.expected[i])
				}
			}
		})
	}
}

func TestValidateMCPToolNames(t *testing.T) {
	t.Run("valid tools pass", func(t *testing.T) {
		err := validateMCPToolNames([]string{"read_file", "grep", "web_search"})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("unified_diff passes (allowed for edit-format swap)", func(t *testing.T) {
		err := validateMCPToolNames([]string{"unified_diff"})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("ask_user rejected", func(t *testing.T) {
		err := validateMCPToolNames([]string{"read_file", "ask_user"})
		if err == nil {
			t.Fatal("expected error for ask_user")
		}
		if got := err.Error(); !strings.Contains(got, "ask_user") {
			t.Errorf("error should mention ask_user, got: %s", got)
		}
	})

	t.Run("spawn_agent rejected", func(t *testing.T) {
		err := validateMCPToolNames([]string{"spawn_agent"})
		if err == nil {
			t.Fatal("expected error for spawn_agent")
		}
	})

	t.Run("run_agent_script rejected", func(t *testing.T) {
		err := validateMCPToolNames([]string{"run_agent_script"})
		if err == nil {
			t.Fatal("expected error for run_agent_script")
		}
	})

	t.Run("view_image rejected", func(t *testing.T) {
		err := validateMCPToolNames([]string{"view_image"})
		if err == nil {
			t.Fatal("expected error for view_image")
		}
	})

	t.Run("show_image rejected", func(t *testing.T) {
		err := validateMCPToolNames([]string{"show_image"})
		if err == nil {
			t.Fatal("expected error for show_image")
		}
	})

	t.Run("totally unknown name rejected", func(t *testing.T) {
		err := validateMCPToolNames([]string{"nonexistent"})
		if err == nil {
			t.Fatal("expected error for nonexistent tool")
		}
	})

	t.Run("multiple invalid reported together", func(t *testing.T) {
		err := validateMCPToolNames([]string{"ask_user", "spawn_agent"})
		if err == nil {
			t.Fatal("expected error")
		}
		got := err.Error()
		if !strings.Contains(got, "ask_user") || !strings.Contains(got, "spawn_agent") {
			t.Errorf("error should list both invalid tools, got: %s", got)
		}
	})

	t.Run("empty list passes", func(t *testing.T) {
		err := validateMCPToolNames(nil)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestSwapEditTool(t *testing.T) {
	t.Run("swaps edit_file to unified_diff", func(t *testing.T) {
		input := []string{"read_file", tools.EditFileToolName, "grep"}
		result := swapEditTool(input, tools.EditFileToolName, tools.UnifiedDiffToolName)
		expected := []string{"read_file", tools.UnifiedDiffToolName, "grep"}
		assertStringSlice(t, result, expected)
	})

	t.Run("no-op when target not present", func(t *testing.T) {
		input := []string{"read_file", "grep"}
		result := swapEditTool(input, tools.EditFileToolName, tools.UnifiedDiffToolName)
		assertStringSlice(t, result, input)
	})

	t.Run("does not modify original", func(t *testing.T) {
		input := []string{tools.EditFileToolName}
		result := swapEditTool(input, tools.EditFileToolName, tools.UnifiedDiffToolName)
		if input[0] != tools.EditFileToolName {
			t.Error("original slice was mutated")
		}
		if result[0] != tools.UnifiedDiffToolName {
			t.Errorf("expected %s, got %s", tools.UnifiedDiffToolName, result[0])
		}
	})
}

func TestMCPAllToolNamesAreValid(t *testing.T) {
	// Ensure every name in the "all" list is in the allowlist.
	for _, name := range mcpAllToolNames() {
		if !mcpAllowedTools[name] {
			t.Errorf("tool %q is in mcpAllToolNames() but not in mcpAllowedTools", name)
		}
	}
}

func assertStringSlice(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}
