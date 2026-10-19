package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

// These are the 17 distinct Changed bits read by serve startup/runtime and
// serve mcp approval arbitration. Keep the aliases separate: they share a value,
// not a Changed bit. Fresh bindings avoid leaking global Cobra state between rows.
var serveContractFlags = []string{
	"projects", "no-projects", "response-timeout", "auth", "no-auth", "allow-no-auth",
	"base-path", "title", "disable-location-sharing", "max-turns", "tools", "system",
	"extensions", "enable-widgets", "approval", "auto", "yolo",
}

func bindServeContractArgs(t *testing.T, args ...string) (*cobra.Command, *ServeOptions) {
	t.Helper()
	cmd, opts := &cobra.Command{}, &ServeOptions{}
	bindServeFlags(cmd, opts)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return cmd, opts
}

func assertServeContractChanged(t *testing.T, cmd *cobra.Command, names []string) {
	t.Helper()
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	for _, name := range append(append([]string(nil), serveContractFlags...), "system-message") {
		if got := cmd.Flags().Changed(name); got != want[name] {
			t.Errorf("Changed(%q) = %v, want %v", name, got, want[name])
		}
	}
}

type serveContractValues struct {
	Projects, NoProjects, AllowNoAuth, DisableLocationSharing, EnableWidgets, Auto, Yolo bool
	ResponseTimeout                                                                      time.Duration
	Auth, BasePath, Title, Tools, System, Approval                                       string
	MaxTurns                                                                             int
	Extensions                                                                           []string
}

func TestServeFlagsContract(t *testing.T) {
	// AddCommonFlags updates a shared registry, so these bindings are serial.
	defaults := serveContractValues{ResponseTimeout: 30 * time.Minute, Auth: "bearer", BasePath: "/ui", MaxTurns: 200}
	for _, tc := range []struct {
		name    string
		args    []string
		changed []string
		want    serveContractValues
	}{
		{"unset", nil, nil, defaults},
		{"explicit defaults", []string{"--projects=false", "--no-projects=false", "--response-timeout=30m", "--auth=bearer", "--no-auth=false", "--allow-no-auth=false", "--base-path=/ui", "--title=", "--disable-location-sharing=false", "--max-turns=200", "--tools=", "--system-message=", "--extensions=", "--enable-widgets=false", "--auto=false", "--yolo=false"}, []string{"projects", "no-projects", "response-timeout", "auth", "no-auth", "allow-no-auth", "base-path", "title", "disable-location-sharing", "max-turns", "tools", "system-message", "extensions", "enable-widgets", "auto", "yolo"}, defaults},
		{"non defaults", []string{"--projects", "--response-timeout=1h", "--auth=none", "--no-auth", "--base-path=/chat", "--title= Lab ", "--disable-location-sharing", "--max-turns=7", "--tools=read_file", "--system-message=be brief", "--extensions=one,two", "--enable-widgets", "--approval=prompt", "--auto", "--yolo"}, []string{"projects", "response-timeout", "auth", "no-auth", "base-path", "title", "disable-location-sharing", "max-turns", "tools", "system-message", "extensions", "enable-widgets", "approval", "auto", "yolo"}, serveContractValues{Projects: true, AllowNoAuth: true, DisableLocationSharing: true, EnableWidgets: true, Auto: true, Yolo: true, ResponseTimeout: time.Hour, Auth: "none", BasePath: "/chat", Title: " Lab ", Tools: "read_file", System: "be brief", Approval: "prompt", MaxTurns: 7, Extensions: []string{"one", "two"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, opts := bindServeContractArgs(t, tc.args...)
			assertServeContractChanged(t, cmd, tc.changed)
			widgets, err := cmd.Flags().GetBool("enable-widgets")
			if err != nil {
				t.Fatal(err)
			}
			got := serveContractValues{opts.Projects, opts.NoProjects, opts.AllowNoAuth, opts.DisableLocationSharing, widgets, opts.Auto, opts.Yolo, opts.ResponseTimeout, opts.AuthMode, opts.BasePath, opts.Title, opts.Tools, opts.SystemMessage, opts.Approval, opts.MaxTurns, opts.ExtensionIDs}
			// pflag distinguishes nil from explicit empty slices; the contract is
			// no extension IDs in both cases, with Changed carrying precedence.
			if len(got.Extensions) == 0 {
				got.Extensions = nil
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("bound values = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestServeRuntimeFixtureOptionsMatchRegisteredDefaults(t *testing.T) {
	_, registered := bindServeContractArgs(t)
	neutral := serveContractDefaultOptions()
	if neutral.Host != registered.Host || neutral.Port != registered.Port || neutral.AuthMode != registered.AuthMode ||
		neutral.BasePath != registered.BasePath || neutral.SessionTTL != registered.SessionTTL || neutral.SessionMax != registered.SessionMax ||
		neutral.TelegramCarryoverChars != registered.TelegramCarryoverChars || neutral.JobsWorkers != registered.JobsWorkers ||
		neutral.SidebarSessions != registered.SidebarSessions || neutral.ResponseTimeout != registered.ResponseTimeout ||
		neutral.MaxTurns != registered.MaxTurns || neutral.HubConnect != registered.HubConnect {
		t.Fatalf("neutral runtime fixture defaults = %#v, registered = %#v", neutral, *registered)
	}
}

func TestServeRuntimeFixtureOptionsMatchContractDefaults(t *testing.T) {
	neutral := serveContractDefaultOptions()
	if host := serveRuntimeFixtureOptions(); !reflect.DeepEqual(host, neutral) {
		t.Fatalf("host runtime fixture defaults = %#v, neutral = %#v", host, neutral)
	}
}

func TestCaptureServeInvocationFreezesFlagAndRootInputs(t *testing.T) {
	oldNoSession, oldSessionPath := noSession, sessionDBPath
	oldMemoryPath, oldMemoryDefault := memoryDBPath, defaultMemoryDBPath
	t.Cleanup(func() {
		noSession, sessionDBPath = oldNoSession, oldSessionPath
		memoryDBPath, defaultMemoryDBPath = oldMemoryPath, oldMemoryDefault
	})
	noSession, sessionDBPath = true, "captured-sessions.db"
	memoryDBPath, defaultMemoryDBPath = "captured-memory.db", "default-memory.db"
	cmd, opts := bindServeContractArgs(t,
		"--projects", "--no-projects=false", "--response-timeout=1h", "--auth=bearer",
		"--no-auth=false", "--base-path=/chat", "--title=Lab", "--disable-location-sharing",
		"--max-turns=7", "--tools=read_file", "--system-message=brief", "--extensions=one", "--enable-widgets=false",
	)
	invocation := captureServeInvocation(cmd, opts)
	want := serveFlagState{
		Projects: true, NoProjects: true, ResponseTimeout: true, Auth: true, NoAuth: true,
		BasePath: true, Title: true, DisableLocationSharing: true, MaxTurns: true,
		Tools: true, System: false, Extensions: true, EnableWidgets: true,
	}
	if !reflect.DeepEqual(invocation.Flags, want) {
		t.Fatalf("captured flags = %#v, want %#v", invocation.Flags, want)
	}
	if !invocation.SessionStore.NoSession || invocation.SessionStore.DBPath != "captured-sessions.db" ||
		invocation.MemoryPaths.DBPath != "captured-memory.db" || invocation.MemoryPaths.DefaultDBPath != "default-memory.db" {
		t.Fatalf("captured roots = %#v / %#v", invocation.SessionStore, invocation.MemoryPaths)
	}
	noSession, sessionDBPath, memoryDBPath = false, "mutated.db", "mutated-memory.db"
	if !invocation.SessionStore.NoSession || invocation.SessionStore.DBPath != "captured-sessions.db" || invocation.MemoryPaths.DBPath != "captured-memory.db" {
		t.Fatalf("invocation changed with root globals: %#v / %#v", invocation.SessionStore, invocation.MemoryPaths)
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

func TestServeAuthAliasFlagBindingContract(t *testing.T) {
	for _, tc := range []struct {
		name, auth    string
		args, changed []string
		allowNoAuth   bool
	}{
		{name: "unset", auth: "bearer"},
		{name: "explicit default", args: []string{"--auth=bearer"}, changed: []string{"auth"}, auth: "bearer"},
		{name: "no auth", args: []string{"--no-auth"}, changed: []string{"no-auth"}, auth: "bearer", allowNoAuth: true},
		{name: "hidden alias", args: []string{"--allow-no-auth"}, changed: []string{"allow-no-auth"}, auth: "bearer", allowNoAuth: true},
		{name: "explicit false", args: []string{"--no-auth=false"}, changed: []string{"no-auth"}, auth: "bearer"},
		{name: "hidden explicit false", args: []string{"--allow-no-auth=false"}, changed: []string{"allow-no-auth"}, auth: "bearer"},
		{name: "last hidden alias false wins", args: []string{"--no-auth", "--allow-no-auth=false"}, changed: []string{"no-auth", "allow-no-auth"}, auth: "bearer"},
		{name: "last visible alias true wins", args: []string{"--allow-no-auth=false", "--no-auth"}, changed: []string{"no-auth", "allow-no-auth"}, auth: "bearer", allowNoAuth: true},
		{name: "last visible alias false wins", args: []string{"--allow-no-auth", "--no-auth=false"}, changed: []string{"no-auth", "allow-no-auth"}, auth: "bearer"},
		{name: "last hidden alias true wins", args: []string{"--no-auth=false", "--allow-no-auth"}, changed: []string{"no-auth", "allow-no-auth"}, auth: "bearer", allowNoAuth: true},
		{name: "matching auth", args: []string{"--auth=none", "--no-auth"}, changed: []string{"auth", "no-auth"}, auth: "none", allowNoAuth: true},
		{name: "auth conflicts with alias", args: []string{"--auth=bearer", "--no-auth"}, changed: []string{"auth", "no-auth"}, auth: "bearer", allowNoAuth: true},
		{name: "alias conflicts with auth", args: []string{"--allow-no-auth", "--auth=bearer"}, changed: []string{"auth", "allow-no-auth"}, auth: "bearer", allowNoAuth: true},
		{name: "false alias conflicts", args: []string{"--auth=none", "--allow-no-auth=false"}, changed: []string{"auth", "allow-no-auth"}, auth: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, opts := bindServeContractArgs(t, tc.args...)
			assertServeContractChanged(t, cmd, tc.changed)
			if opts.AuthMode != tc.auth || opts.AllowNoAuth != tc.allowNoAuth {
				t.Fatalf("bound auth=%q no-auth=%v, want %q %v", opts.AuthMode, opts.AllowNoAuth, tc.auth, tc.allowNoAuth)
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

func TestServeDeprecatedWidgetsExplicitFalseContract(t *testing.T) {
	for _, args := range [][]string{nil, {"--enable-widgets"}, {"--enable-widgets=false"}} {
		cmd, opts := bindServeContractArgs(t, args...)
		invocation := captureServeInvocation(cmd, opts)
		var stderr bytes.Buffer
		warnDeprecatedEnableWidgets(invocation.Flags.EnableWidgets, &stderr)
		if got, want := strings.Contains(stderr.String(), "deprecated and has no effect"), len(args) != 0; got != want {
			t.Fatalf("args=%v warning=%q, want warning=%v", args, stderr.String(), want)
		}
	}
}

func TestServeFlagParseErrorsContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		execute bool
	}{
		{"empty approval", []string{"--port=0", "--approval="}, `invalid approval mode ""`, false},
		{"invalid approval before startup validation", []string{"--approval=invalid", "--port=0"}, `invalid approval mode "invalid"`, false},
		{"system is not a flag", []string{"--system="}, "unknown flag: --system", false},
		{"Cobra approval group before startup", []string{"--port=0", "--auto", "--yolo"}, "if any flags in the group [approval yolo auto]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, opts := &cobra.Command{}, &ServeOptions{}
			bindServeFlags(cmd, opts)
			var err error
			if tc.execute {
				cmd.RunE = func(*cobra.Command, []string) error {
					t.Error("RunE must not execute before Cobra rejects the flag group")
					return nil
				}
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				cmd.SetArgs(tc.args)
				err = cmd.Execute()
			} else {
				err = cmd.ParseFlags(tc.args)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("flag error = %v, want %q", err, tc.want)
			}
		})
	}
}
