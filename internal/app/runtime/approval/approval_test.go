package approval

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"strings"
	"testing"
)

func approvalModePtr(mode tools.ApprovalMode) *tools.ApprovalMode { return &mode }

func sessionApprovalModePtr(mode session.SessionApprovalMode) *session.SessionApprovalMode {
	return &mode
}

func TestResolveApprovalMode(t *testing.T) {
	tests := []struct {
		name       string
		input      ResolutionInput
		wantMode   tools.ApprovalMode
		wantSource ModeSource
		wantErr    string
	}{
		{name: "blank chat is auto", input: ResolutionInput{Surface: SurfaceChat}, wantMode: tools.ModeAuto, wantSource: ModeSourceBuiltinDefault},
		{name: "blank ask is auto", input: ResolutionInput{Surface: SurfaceAsk}, wantMode: tools.ModeAuto, wantSource: ModeSourceBuiltinDefault},
		{name: "blank edit is prompt", input: ResolutionInput{Surface: SurfaceEdit}, wantMode: tools.ModePrompt, wantSource: ModeSourceBuiltinDefault},
		{name: "blank exec is prompt", input: ResolutionInput{Surface: SurfaceExec}, wantMode: tools.ModePrompt, wantSource: ModeSourceBuiltinDefault},
		{name: "blank loop is prompt", input: ResolutionInput{Surface: SurfaceLoop}, wantMode: tools.ModePrompt, wantSource: ModeSourceBuiltinDefault},
		{name: "blank serve is auto", input: ResolutionInput{Surface: SurfaceServe}, wantMode: tools.ModeAuto, wantSource: ModeSourceBuiltinDefault},
		{name: "blank serve mcp is prompt", input: ResolutionInput{Surface: SurfaceServeMCP}, wantMode: tools.ModePrompt, wantSource: ModeSourceBuiltinDefault},
		{
			name:       "global prompt overrides chat builtin",
			input:      ResolutionInput{Surface: SurfaceChat, Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "prompt"}}},
			wantMode:   tools.ModePrompt,
			wantSource: ModeSourceGlobalConfig,
		},
		{
			name:       "global auto overrides conservative builtin",
			input:      ResolutionInput{Surface: SurfaceExec, Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "auto"}}},
			wantMode:   tools.ModeAuto,
			wantSource: ModeSourceGlobalConfig,
		},
		{
			name: "surface prompt overrides global auto",
			input: ResolutionInput{Surface: SurfaceAsk, Config: &config.Config{
				Approval: config.ApprovalConfig{DefaultMode: "auto"},
				Ask:      config.AskConfig{ApprovalMode: "prompt"},
			}},
			wantMode:   tools.ModePrompt,
			wantSource: ModeSourceSurfaceConfig,
		},
		{
			name: "surface auto overrides global prompt",
			input: ResolutionInput{Surface: SurfaceEdit, Config: &config.Config{
				Approval: config.ApprovalConfig{DefaultMode: "prompt"},
				Edit:     config.EditConfig{ApprovalMode: "auto"},
			}},
			wantMode:   tools.ModeAuto,
			wantSource: ModeSourceSurfaceConfig,
		},
		{
			name:       "cli prompt overrides config auto",
			input:      ResolutionInput{Surface: SurfaceChat, CLI: approvalModePtr(tools.ModePrompt), Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "auto"}}},
			wantMode:   tools.ModePrompt,
			wantSource: ModeSourceCLI,
		},
		{
			name:       "cli prompt overrides serve auto builtin",
			input:      ResolutionInput{Surface: SurfaceServe, CLI: approvalModePtr(tools.ModePrompt)},
			wantMode:   tools.ModePrompt,
			wantSource: ModeSourceCLI,
		},
		{
			name:       "cli auto overrides config prompt",
			input:      ResolutionInput{Surface: SurfaceEdit, CLI: approvalModePtr(tools.ModeAuto), Config: &config.Config{Edit: config.EditConfig{ApprovalMode: "prompt"}}},
			wantMode:   tools.ModeAuto,
			wantSource: ModeSourceCLI,
		},
		{
			name:       "cli yolo is accepted",
			input:      ResolutionInput{Surface: SurfaceServe, CLI: approvalModePtr(tools.ModeYolo), Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "prompt"}}},
			wantMode:   tools.ModeYolo,
			wantSource: ModeSourceCLI,
		},
		{
			name:       "stored prompt overrides config",
			input:      ResolutionInput{Surface: SurfaceChat, Session: sessionApprovalModePtr(session.ApprovalModePrompt), Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "auto"}}},
			wantMode:   tools.ModePrompt,
			wantSource: ModeSourceSession,
		},
		{
			name:       "stored auto overrides config",
			input:      ResolutionInput{Surface: SurfaceChat, Session: sessionApprovalModePtr(session.ApprovalModeAuto), Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "prompt"}}},
			wantMode:   tools.ModeAuto,
			wantSource: ModeSourceSession,
		},
		{
			name:       "cli overrides stored session",
			input:      ResolutionInput{Surface: SurfaceChat, CLI: approvalModePtr(tools.ModeAuto), Session: sessionApprovalModePtr(session.ApprovalModePrompt)},
			wantMode:   tools.ModeAuto,
			wantSource: ModeSourceCLI,
		},
		{
			name:       "empty legacy session is prompt",
			input:      ResolutionInput{Surface: SurfaceChat, Session: sessionApprovalModePtr("")},
			wantMode:   tools.ModePrompt,
			wantSource: ModeSourceLegacySession,
		},
		{
			name:       "stored yolo is prompt",
			input:      ResolutionInput{Surface: SurfaceChat, Session: sessionApprovalModePtr(session.ApprovalModeYolo)},
			wantMode:   tools.ModePrompt,
			wantSource: ModeSourceLegacySession,
		},
		{
			name:    "invalid global config",
			input:   ResolutionInput{Surface: SurfaceChat, Config: &config.Config{Approval: config.ApprovalConfig{DefaultMode: "automatic"}}},
			wantErr: `invalid approval.default_mode "automatic": expected prompt or auto`,
		},
		{
			name:    "unknown surface is rejected",
			input:   ResolutionInput{Surface: Surface("future")},
			wantErr: `unknown approval surface "future"`,
		},
		{
			name:    "invalid surface config",
			input:   ResolutionInput{Surface: SurfaceChat, Config: &config.Config{Chat: config.ChatConfig{ApprovalMode: "automatic"}}},
			wantErr: `invalid chat.approval_mode "automatic": expected prompt or auto`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Resolve() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Mode != tt.wantMode || got.Source != tt.wantSource {
				t.Fatalf("Resolve() = {%v %v}, want {%v %v}", got.Mode, got.Source, tt.wantMode, tt.wantSource)
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
			got := ChatCarryForRelaunch(tt.modeChanged, tt.handoverText, tools.ModeAuto)
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
	if got := ForColdPersistence(tools.ModeYolo); got != session.ApprovalModePrompt {
		t.Fatalf("cold-persisted yolo = %q, want prompt", got)
	}
	if got := ForColdPersistence(tools.ModeAuto); got != session.ApprovalModeAuto {
		t.Fatalf("cold-persisted auto = %q, want auto", got)
	}
}
