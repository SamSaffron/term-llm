package cmd

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"

	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

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
