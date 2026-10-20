package cmd

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

func TestChatApprovalResolverUsesMutableExplicitPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, explicit    string
		initial, selected tools.ApprovalMode
	}{
		{"explicit yolo to prompt", "yolo", tools.ModeYolo, tools.ModePrompt},
		{"explicit prompt to yolo", "prompt", tools.ModePrompt, tools.ModeYolo},
		{"no explicit flag", "", tools.ModeAuto, tools.ModeYolo},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldApproval, oldYolo, oldAuto := chatApproval, chatYolo, chatAutoApproval
			t.Cleanup(func() { chatApproval, chatYolo, chatAutoApproval = oldApproval, oldYolo, oldAuto })
			chatApproval, chatYolo, chatAutoApproval = tt.explicit, false, false
			command := &cobra.Command{}
			command.Flags().String("approval", "", "")
			if tt.explicit != "" {
				if err := command.Flags().Set("approval", tt.explicit); err != nil {
					t.Fatal(err)
				}
			}
			opts, deps := captureChatInvocation(command)
			// Later Cobra/global mutations are not a source of runtime policy.
			chatApproval, chatYolo, chatAutoApproval = "invalid", true, true
			if err := command.Flags().Set("approval", "invalid"); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{}
			cfg.Chat.ApprovalMode = "auto"
			initial, err := deps.ResolveApproval(cfg, nil, nil, opts.Approval)
			if err != nil || initial.Mode != tt.initial {
				t.Fatalf("initial=%+v err=%v", initial, err)
			}
			// runChatOnce updates invocation policy after the operator toggles it.
			opts.Approval, opts.Yolo, opts.AutoApproval = tt.selected.String(), tt.selected == tools.ModeYolo, tt.selected == tools.ModeAuto
			persisted := session.ApprovalModePrompt
			carried := tt.selected
			target, err := deps.ResolveApproval(cfg, &persisted, &carried, opts.Approval)
			if err != nil || target.Mode != tt.selected {
				t.Errorf("handover=%+v err=%v want=%v", target, err, tt.selected)
			}
			// A later ordinary session switch may still see old handover carry.
			// It must not replace an explicit invocation policy at every call.
			staleCarry := tools.ModeAuto
			want := tt.selected
			if tt.explicit == "" {
				want = staleCarry
			}
			switched, err := deps.ResolveApproval(cfg, &persisted, &staleCarry, opts.Approval)
			if err != nil || switched.Mode != want {
				t.Errorf("switch=%+v err=%v want=%v", switched, err, want)
			}
			withoutCarry, err := deps.ResolveApproval(cfg, &persisted, nil, opts.Approval)
			if tt.explicit == "" {
				want = tools.ModePrompt
			}
			if err != nil || withoutCarry.Mode != want {
				t.Errorf("no carry=%+v err=%v want=%v", withoutCarry, err, want)
			}
		})
	}
}

func TestChatApprovalResolverValidationAndConfigPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name        string
		flags       []string
		want        tools.ApprovalMode
		errContains string
	}{
		{"approval", []string{"--approval=yolo"}, tools.ModeYolo, ""},
		{"yolo alias", []string{"--yolo"}, tools.ModeYolo, ""},
		{"auto alias", []string{"--auto"}, tools.ModeAuto, ""},
		{"invalid", []string{"--approval=invalid"}, tools.ModePrompt, "invalid approval mode"},
		{"empty explicit", []string{"--approval="}, tools.ModePrompt, "invalid approval mode"},
		{"false auto", []string{"--auto=false"}, tools.ModePrompt, "--auto=false"},
		{"false yolo", []string{"--yolo=false"}, tools.ModePrompt, "--yolo=false"},
		{"conflict", []string{"--approval=prompt", "--yolo"}, tools.ModePrompt, "mutually exclusive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldApproval, oldYolo, oldAuto := chatApproval, chatYolo, chatAutoApproval
			t.Cleanup(func() { chatApproval, chatYolo, chatAutoApproval = oldApproval, oldYolo, oldAuto })
			command := &cobra.Command{}
			command.Flags().StringVar(&chatApproval, "approval", "", "")
			command.Flags().BoolVar(&chatYolo, "yolo", false, "")
			command.Flags().BoolVar(&chatAutoApproval, "auto", false, "")
			if err := command.ParseFlags(tt.flags); err != nil {
				t.Fatal(err)
			}
			opts, deps := captureChatInvocation(command)
			cfg := &config.Config{}
			cfg.Chat.ApprovalMode = "invalid" // Explicit CLI still wins over config validation.
			resolved, err := deps.ResolveApproval(cfg, nil, nil, opts.Approval)
			if tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("got %v want %q", err, tt.errContains)
				}
				// Carry/current request cannot erase a captured invalid launch flag.
				carry := tools.ModeYolo
				if _, err := deps.ResolveApproval(cfg, nil, &carry, "prompt"); err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("lost deferred flag error: %v", err)
				}
				return
			}
			if err != nil || resolved.Mode != tt.want || opts.Approval != tt.want.String() {
				t.Fatalf("initial=%+v options=%+v err=%v", resolved, opts, err)
			}
			// Valid aliases retain explicitness but use current invocation policy.
			resolved, err = deps.ResolveApproval(cfg, nil, nil, "prompt")
			if err != nil || resolved.Mode != tools.ModePrompt {
				t.Fatalf("updated explicit=%+v err=%v", resolved, err)
			}
		})
	}
	_, deps := captureChatInvocation(nil)
	cfg := &config.Config{}
	cfg.Approval.DefaultMode, cfg.Chat.ApprovalMode = "prompt", "auto"
	for _, tt := range []struct {
		surface, global string
		want            tools.ApprovalMode
		source          approvalModeSource
		invalid         bool
	}{
		{"auto", "prompt", tools.ModeAuto, approvalModeSourceSurfaceConfig, false},
		{"prompt", "auto", tools.ModePrompt, approvalModeSourceSurfaceConfig, false},
		{"", "prompt", tools.ModePrompt, approvalModeSourceGlobalConfig, false},
		{"", "", tools.ModeAuto, approvalModeSourceBuiltinDefault, false},
		{"invalid", "prompt", tools.ModePrompt, "", true},
	} {
		cfg.Chat.ApprovalMode, cfg.Approval.DefaultMode = tt.surface, tt.global
		// A mutable value alone must not become an explicit CLI flag.
		resolved, err := deps.ResolveApproval(cfg, nil, nil, "yolo")
		if tt.invalid {
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			continue
		}
		if err != nil || resolved.Mode != tt.want || resolved.Source != tt.source {
			t.Fatalf("config %q/%q: %+v %v", tt.surface, tt.global, resolved, err)
		}
	}
}
