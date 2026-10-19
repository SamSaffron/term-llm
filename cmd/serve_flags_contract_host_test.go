package cmd

import (
	"reflect"

	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeRuntimeFixtureOptionsMatchContractDefaults(t *testing.T) {
	neutral := serveContractDefaultOptions()
	if host := serveRuntimeFixtureOptions(); !reflect.DeepEqual(host, neutral) {
		t.Fatalf("host runtime fixture defaults = %#v, neutral = %#v", host, neutral)
	}
}

func TestServeAuthAliasContract(t *testing.T) {
	for _, tc := range []struct {
		name, auth, want, wantErr string
		allowNoAuth, authSet      bool
		noAuthSet                 bool
	}{
		{name: "unset", auth: "bearer", want: "bearer"},
		{name: "explicit default", auth: "bearer", authSet: true, want: "bearer"},
		{name: "no auth", auth: "bearer", allowNoAuth: true, noAuthSet: true, want: "none"},
		{name: "hidden alias", auth: "bearer", allowNoAuth: true, noAuthSet: true, want: "none"},
		{name: "explicit false", auth: "bearer", noAuthSet: true, want: "bearer"},
		{name: "hidden explicit false", auth: "bearer", noAuthSet: true, want: "bearer"},
		{name: "last hidden alias false wins", auth: "bearer", noAuthSet: true, want: "bearer"},
		{name: "last visible alias true wins", auth: "bearer", allowNoAuth: true, noAuthSet: true, want: "none"},
		{name: "last visible alias false wins", auth: "bearer", noAuthSet: true, want: "bearer"},
		{name: "last hidden alias true wins", auth: "bearer", allowNoAuth: true, noAuthSet: true, want: "none"},
		{name: "matching auth", auth: "none", allowNoAuth: true, authSet: true, noAuthSet: true, want: "none"},
		{name: "auth conflicts with alias", auth: "bearer", allowNoAuth: true, authSet: true, noAuthSet: true, wantErr: "--auth bearer conflicts with --no-auth=true"},
		{name: "alias conflicts with auth", auth: "bearer", allowNoAuth: true, authSet: true, noAuthSet: true, wantErr: "--auth bearer conflicts with --no-auth=true"},
		{name: "false alias conflicts", auth: "none", authSet: true, noAuthSet: true, wantErr: "--auth none conflicts with --no-auth=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := serveRuntimeFixtureOptions()
			opts.Token, opts.AuthMode, opts.AllowNoAuth = "contract-token", tc.auth, tc.allowNoAuth
			got, err := resolveServeStartup(serveFlagState{Auth: tc.authSet, NoAuth: tc.noAuthSet}, &opts)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("startup error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.authMode != tc.want || got.requireAuth != (tc.want != "none") {
				t.Fatalf("startup auth = %q required=%v, error=%v; want %q", got.authMode, got.requireAuth, err, tc.want)
			}
		})
	}
}

// The registered flag is system-message, but today the runner reads Changed("system").
// Explicit system-message values therefore do NOT set SystemMessageSet. Freeze
// that discrepancy here; correcting it is outside this behavior-preserving step.
func TestServeRuntimeExplicitEmptyContract(t *testing.T) {
	for _, tc := range []struct {
		name          string
		toolsSet      bool
		tools, system string
	}{
		{"unset", false, "", ""},
		{"explicit empty", true, "", ""},
		{"explicit values", true, "read_file", "be brief"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := serveRuntimeFixtureOptions()
			opts.Tools, opts.SystemMessage = tc.tools, tc.system
			flags := serveFlagState{Tools: tc.toolsSet, System: false}
			got := serveRuntimeRunnerDefaults(serveAgentRuntimeOptions{
				flags: opts, toolsSet: flags.Tools, systemMessageSet: flags.System,
			}, serveRuntimeRequest{}, tools.ModePrompt)
			if got.ToolsSet != tc.toolsSet || got.SystemMessageSet || got.Tools != tc.tools || got.SystemMessage != tc.system || got.MaxTurns != 200 {
				t.Fatalf("runner inputs: tools=%q set=%v system=%q set=%v maxTurns=%d", got.Tools, got.ToolsSet, got.SystemMessage, got.SystemMessageSet, got.MaxTurns)
			}
		})
	}
}
