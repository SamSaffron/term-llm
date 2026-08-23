package cmd

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func approvalModePtr(mode tools.ApprovalMode) *tools.ApprovalMode { return &mode }

func sessionApprovalModePtr(mode session.SessionApprovalMode) *session.SessionApprovalMode {
	return &mode
}

func TestResolveApprovalMode(t *testing.T) {
	tests := []struct {
		name       string
		input      approvalModeResolutionInput
		wantMode   tools.ApprovalMode
		wantSource approvalModeSource
		wantErr    string
	}{
		{name: "blank chat is auto", input: approvalModeResolutionInput{Surface: approvalSurfaceChat}, wantMode: tools.ModeAuto, wantSource: approvalModeSourceBuiltinDefault},
		{name: "blank ask is auto", input: approvalModeResolutionInput{Surface: approvalSurfaceAsk}, wantMode: tools.ModeAuto, wantSource: approvalModeSourceBuiltinDefault},
		{name: "blank edit is prompt", input: approvalModeResolutionInput{Surface: approvalSurfaceEdit}, wantMode: tools.ModePrompt, wantSource: approvalModeSourceBuiltinDefault},
		{name: "blank exec is prompt", input: approvalModeResolutionInput{Surface: approvalSurfaceExec}, wantMode: tools.ModePrompt, wantSource: approvalModeSourceBuiltinDefault},
		{name: "blank loop is prompt", input: approvalModeResolutionInput{Surface: approvalSurfaceLoop}, wantMode: tools.ModePrompt, wantSource: approvalModeSourceBuiltinDefault},
		{name: "blank serve is auto", input: approvalModeResolutionInput{Surface: approvalSurfaceServe}, wantMode: tools.ModeAuto, wantSource: approvalModeSourceBuiltinDefault},
		{name: "blank serve mcp is prompt", input: approvalModeResolutionInput{Surface: approvalSurfaceServeMCP}, wantMode: tools.ModePrompt, wantSource: approvalModeSourceBuiltinDefault},
		{
			name:       "global prompt overrides chat builtin",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceChat, Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "prompt"}}},
			wantMode:   tools.ModePrompt,
			wantSource: approvalModeSourceGlobalConfig,
		},
		{
			name:       "global auto overrides conservative builtin",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceExec, Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "auto"}}},
			wantMode:   tools.ModeAuto,
			wantSource: approvalModeSourceGlobalConfig,
		},
		{
			name: "surface prompt overrides global auto",
			input: approvalModeResolutionInput{Surface: approvalSurfaceAsk, Config: &config.Config{
				Approval: config.ApprovalConfig{DefaultMode: "auto"},
				Ask:      config.AskConfig{ApprovalMode: "prompt"},
			}},
			wantMode:   tools.ModePrompt,
			wantSource: approvalModeSourceSurfaceConfig,
		},
		{
			name: "surface auto overrides global prompt",
			input: approvalModeResolutionInput{Surface: approvalSurfaceEdit, Config: &config.Config{
				Approval: config.ApprovalConfig{DefaultMode: "prompt"},
				Edit:     config.EditConfig{ApprovalMode: "auto"},
			}},
			wantMode:   tools.ModeAuto,
			wantSource: approvalModeSourceSurfaceConfig,
		},
		{
			name:       "cli prompt overrides config auto",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceChat, CLI: approvalModePtr(tools.ModePrompt), Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "auto"}}},
			wantMode:   tools.ModePrompt,
			wantSource: approvalModeSourceCLI,
		},
		{
			name:       "cli prompt overrides serve auto builtin",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceServe, CLI: approvalModePtr(tools.ModePrompt)},
			wantMode:   tools.ModePrompt,
			wantSource: approvalModeSourceCLI,
		},
		{
			name:       "cli auto overrides config prompt",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceEdit, CLI: approvalModePtr(tools.ModeAuto), Config: &config.Config{Edit: config.EditConfig{ApprovalMode: "prompt"}}},
			wantMode:   tools.ModeAuto,
			wantSource: approvalModeSourceCLI,
		},
		{
			name:       "cli yolo is accepted",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceServe, CLI: approvalModePtr(tools.ModeYolo), Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "prompt"}}},
			wantMode:   tools.ModeYolo,
			wantSource: approvalModeSourceCLI,
		},
		{
			name:       "stored prompt overrides config",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceChat, Session: sessionApprovalModePtr(session.ApprovalModePrompt), Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "auto"}}},
			wantMode:   tools.ModePrompt,
			wantSource: approvalModeSourceSession,
		},
		{
			name:       "stored auto overrides config",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceChat, Session: sessionApprovalModePtr(session.ApprovalModeAuto), Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "prompt"}}},
			wantMode:   tools.ModeAuto,
			wantSource: approvalModeSourceSession,
		},
		{
			name:       "cli overrides stored session",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceChat, CLI: approvalModePtr(tools.ModeAuto), Session: sessionApprovalModePtr(session.ApprovalModePrompt)},
			wantMode:   tools.ModeAuto,
			wantSource: approvalModeSourceCLI,
		},
		{
			name:       "empty legacy session is prompt",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceChat, Session: sessionApprovalModePtr("")},
			wantMode:   tools.ModePrompt,
			wantSource: approvalModeSourceLegacySession,
		},
		{
			name:       "stored yolo is prompt",
			input:      approvalModeResolutionInput{Surface: approvalSurfaceChat, Session: sessionApprovalModePtr(session.ApprovalModeYolo)},
			wantMode:   tools.ModePrompt,
			wantSource: approvalModeSourceLegacySession,
		},
		{
			name:    "invalid global config",
			input:   approvalModeResolutionInput{Surface: approvalSurfaceChat, Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "automatic"}}},
			wantErr: `invalid approval.default_mode "automatic": expected prompt or auto`,
		},
		{
			name:    "unknown surface is rejected",
			input:   approvalModeResolutionInput{Surface: approvalSurface("future")},
			wantErr: `unknown approval surface "future"`,
		},
		{
			name:    "invalid surface config",
			input:   approvalModeResolutionInput{Surface: approvalSurfaceChat, Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "automatic"}}},
			wantErr: `invalid chat.approval_mode "automatic": expected prompt or auto`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveApprovalMode(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveApprovalMode() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveApprovalMode() error = %v", err)
			}
			if got.Mode != tt.wantMode || got.Source != tt.wantSource {
				t.Fatalf("resolveApprovalMode() = {%v %v}, want {%v %v}", got.Mode, got.Source, tt.wantMode, tt.wantSource)
			}
		})
	}
}

func TestChatApprovalCarryOnlyAppliesToToggledHandover(t *testing.T) {
	tests := []struct {
		name         string
		modeChanged  bool
		handoverText string
		want         *tools.ApprovalMode
	}{
		{name: "ordinary resume does not carry", modeChanged: true, want: nil},
		{name: "untoggled handover does not synthesize cli", handoverText: "continue", want: nil},
		{name: "toggled handover carries", modeChanged: true, handoverText: "continue", want: approvalModePtr(tools.ModeAuto)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chatApprovalCarryForRelaunch(tt.modeChanged, tt.handoverText, tools.ModeAuto)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("carry = %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("carry = %v, want %v", got, *tt.want)
			}
		})
	}
}

func TestApprovalModeForColdPersistenceDowngradesYolo(t *testing.T) {
	if got := approvalModeForColdPersistence(tools.ModeYolo); got != session.ApprovalModePrompt {
		t.Fatalf("cold-persisted yolo = %q, want prompt", got)
	}
	if got := approvalModeForColdPersistence(tools.ModeAuto); got != session.ApprovalModeAuto {
		t.Fatalf("cold-persisted auto = %q, want auto", got)
	}
}

func TestChildRunnerInheritsWithoutIndependentEscalation(t *testing.T) {
	parent := tools.NewApprovalManager(tools.NewToolPermissions())
	parent.SetApprovalMode(tools.ModePrompt)
	got := resolvedRunnerApprovalMode(cmdRunnerOptions{
		ApprovalMode:      tools.ModeAuto,
		ApprovalModeSet:   true,
		ParentApprovalMgr: parent,
	}, runpkg.Request{Yolo: true, Auto: true})
	if got != tools.ModePrompt {
		t.Fatalf("child local mode = %v, want prompt so parent remains authoritative", got)
	}
}
