package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// resetServeHubFlagVarsForTest snapshots the package-level serve hub flag
// vars (shared by `serve hub` and `serve hub systemd`) and restores them when
// the test finishes.
func resetServeHubFlagVarsForTest(t *testing.T) {
	t.Helper()
	oldHost, oldPort := serveHubHost, serveHubPort
	oldConfig, oldContain := serveHubConfig, serveHubContain
	oldNodesFile, oldAuth := serveHubNodesFile, serveHubAuthMode
	oldToken, oldReg := serveHubToken, serveHubRegistrationTokenFlag
	oldBasePath, oldInstall := serveHubBasePath, serveHubSystemdInstall
	t.Cleanup(func() {
		serveHubHost, serveHubPort = oldHost, oldPort
		serveHubConfig, serveHubContain = oldConfig, oldContain
		serveHubNodesFile, serveHubAuthMode = oldNodesFile, oldAuth
		serveHubToken, serveHubRegistrationTokenFlag = oldToken, oldReg
		serveHubBasePath, serveHubSystemdInstall = oldBasePath, oldInstall
	})
}

// clearServeHubSystemdCmdFlagsForTest resets the real command's flag values
// and Changed bits after a test drives it through rootCmd.Execute.
func clearServeHubSystemdCmdFlagsForTest(t *testing.T) {
	t.Helper()
	resetServeHubFlagVarsForTest(t)
	t.Cleanup(func() {
		serveHubSystemdCmd.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Changed {
				_ = f.Value.Set(f.DefValue)
				f.Changed = false
			}
		})
	})
}

// newServeHubSystemdFlagsForTest returns a flag set mirroring the
// `serve hub systemd` flags on a throwaway command, so tests can mark flags
// changed without touching the real command's Changed state.
func newServeHubSystemdFlagsForTest(t *testing.T) *pflag.FlagSet {
	t.Helper()
	resetServeHubFlagVarsForTest(t)
	c := &cobra.Command{}
	addServeHubFlags(c)
	var install bool
	c.Flags().BoolVar(&install, "install", false, "")
	return c.Flags()
}

func TestSystemdQuoteArg(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "plain", want: "plain"},
		{in: "/usr/local/bin/term-llm", want: "/usr/local/bin/term-llm"},
		{in: "--host=0.0.0.0", want: "--host=0.0.0.0"},
		{in: "", want: `""`},
		{in: "has space", want: `"has space"`},
		{in: "pct%val", want: "pct%%val"},
		{in: "$HOME", want: "$$HOME"},
		{in: `dq"uote`, want: `"dq\"uote"`},
		{in: `back\slash`, want: `"back\\slash"`},
		{in: "semi;colon", want: `"semi;colon"`},
		{in: "line\nbreak", wantErr: true},
		{in: "tab\there", wantErr: true},
	}
	for _, tc := range cases {
		got, err := systemdQuoteArg(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("systemdQuoteArg(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("systemdQuoteArg(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("systemdQuoteArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSystemdQuoteExecPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "/usr/local/bin/term-llm", want: "/usr/local/bin/term-llm"},
		{in: "/opt/ca$h/term-llm", want: "/opt/ca$h/term-llm"},
		{in: "/opt/pct%dir/term-llm", want: "/opt/pct%%dir/term-llm"},
		{in: "/opt/my tools/term-llm", want: `"/opt/my tools/term-llm"`},
	}
	for _, tc := range cases {
		got, err := systemdQuoteExecPath(tc.in)
		if err != nil {
			t.Errorf("systemdQuoteExecPath(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("systemdQuoteExecPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildServeHubSystemdExecStart(t *testing.T) {
	absConfig, err := filepath.Abs("nodes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		set  map[string]string
		exe  string
		want string
	}{
		{
			name: "no flags",
			exe:  "/usr/local/bin/term-llm",
			want: "/usr/local/bin/term-llm serve hub",
		},
		{
			name: "host and port",
			set:  map[string]string{"host": "0.0.0.0", "port": "9000"},
			exe:  "/usr/local/bin/term-llm",
			want: "/usr/local/bin/term-llm serve hub --host=0.0.0.0 --port=9000",
		},
		{
			name: "secrets and install excluded",
			set:  map[string]string{"token": "secret", "registration-token": "reg", "install": "true", "host": "0.0.0.0"},
			exe:  "/usr/local/bin/term-llm",
			want: "/usr/local/bin/term-llm serve hub --host=0.0.0.0",
		},
		{
			name: "bool flag disabled",
			set:  map[string]string{"contain": "false"},
			exe:  "/usr/local/bin/term-llm",
			want: "/usr/local/bin/term-llm serve hub --contain=false",
		},
		{
			name: "relative config absolutized",
			set:  map[string]string{"config": "nodes.yaml"},
			exe:  "/usr/local/bin/term-llm",
			want: "/usr/local/bin/term-llm serve hub --config=" + absConfig,
		},
		{
			name: "exe with space quoted",
			exe:  "/opt/my tools/term-llm",
			want: `"/opt/my tools/term-llm" serve hub`,
		},
		{
			name: "dollar single in exe path but doubled in arg values",
			set:  map[string]string{"nodes-file": "/var/lib/$hub/nodes.json"},
			exe:  "/opt/ca$h/term-llm",
			want: "/opt/ca$h/term-llm serve hub --nodes-file=/var/lib/$$hub/nodes.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newServeHubSystemdFlagsForTest(t)
			for name, value := range tc.set {
				if err := fs.Set(name, value); err != nil {
					t.Fatal(err)
				}
			}
			got, err := buildServeHubSystemdExecStart(tc.exe, fs)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("execStart = %q, want %q", got, tc.want)
			}
		})
	}
}

// Guards the sync between addServeHubFlags and serveHubSystemdBakedFlags: a
// new hub flag must be either baked into ExecStart or explicitly a secret.
func TestServeHubSystemdBakedFlagsCoverAllHubFlags(t *testing.T) {
	resetServeHubFlagVarsForTest(t)
	c := &cobra.Command{}
	addServeHubFlags(c)
	baked := map[string]bool{}
	for _, name := range serveHubSystemdBakedFlags {
		baked[name] = true
		if c.Flags().Lookup(name) == nil {
			t.Errorf("baked flag --%s is not a hub flag", name)
		}
	}
	secret := map[string]bool{"token": true, "registration-token": true}
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if !baked[f.Name] && !secret[f.Name] {
			t.Errorf("hub flag --%s is neither baked into ExecStart nor a known secret; add it to serveHubSystemdBakedFlags or the env file handling", f.Name)
		}
	})
}

func TestRenderServeHubSystemdUnit(t *testing.T) {
	unit, err := renderServeHubSystemdUnit("/usr/local/bin/term-llm serve hub --host=0.0.0.0", "/etc/term-llm-hub.env")
	if err != nil {
		t.Fatal(err)
	}
	s := string(unit)
	for _, want := range []string{
		"[Unit]",
		"Type=exec\n",
		// User= (even root) is what makes systemd set HOME; without it the
		// hub cannot resolve its data dir and crash-loops on start.
		"User=root\n",
		"ExecStart=/usr/local/bin/term-llm serve hub --host=0.0.0.0\n",
		"EnvironmentFile=-/etc/term-llm-hub.env\n",
		"Restart=on-failure",
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("unit missing %q:\n%s", want, s)
		}
	}
}

func TestResolveServeHubSystemdTokens(t *testing.T) {
	defer resetHubRegistrationForTest()()
	cases := []struct {
		name          string
		requireAuth   bool
		tokenFlag     string
		tokenEnv      string
		regFlag       string
		regSet        bool
		existing      map[string]string
		wantToken     string
		wantGenerated bool
		wantReg       string
	}{
		{
			name:        "flag wins",
			requireAuth: true,
			tokenFlag:   "flagtok",
			tokenEnv:    "envtok",
			existing:    map[string]string{"TERM_LLM_HUB_TOKEN": "filetok"},
			wantToken:   "flagtok",
		},
		{
			name:        "env beats existing file",
			requireAuth: true,
			tokenEnv:    "envtok",
			existing:    map[string]string{"TERM_LLM_HUB_TOKEN": "filetok"},
			wantToken:   "envtok",
		},
		{
			name:        "existing file beats generation",
			requireAuth: true,
			existing:    map[string]string{"TERM_LLM_HUB_TOKEN": "filetok"},
			wantToken:   "filetok",
		},
		{
			name:          "generated when nothing configured",
			requireAuth:   true,
			wantGenerated: true,
		},
		{
			name:        "no auth keeps existing token without minting one",
			requireAuth: false,
			existing:    map[string]string{"TERM_LLM_HUB_TOKEN": "filetok"},
			wantToken:   "filetok",
		},
		{
			name:        "no auth with nothing configured stays empty",
			requireAuth: false,
			wantToken:   "",
		},
		{
			name:        "registration flag wins",
			requireAuth: true,
			tokenFlag:   "t",
			regFlag:     "regflag",
			regSet:      true,
			existing:    map[string]string{"TERM_LLM_HUB_REGISTRATION_TOKEN": "regfile"},
			wantToken:   "t",
			wantReg:     "regflag",
		},
		{
			name:        "registration kept from existing file",
			requireAuth: true,
			tokenFlag:   "t",
			existing:    map[string]string{"TERM_LLM_HUB_REGISTRATION_TOKEN": "regfile"},
			wantToken:   "t",
			wantReg:     "regfile",
		},
		{
			name:        "explicit empty registration flag disables",
			requireAuth: true,
			tokenFlag:   "t",
			regFlag:     "",
			regSet:      true,
			existing:    map[string]string{"TERM_LLM_HUB_REGISTRATION_TOKEN": "regfile"},
			wantToken:   "t",
			wantReg:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, generated, reg, err := resolveServeHubSystemdTokens(tc.requireAuth, tc.tokenFlag, tc.tokenEnv, tc.regFlag, tc.regSet, tc.existing)
			if err != nil {
				t.Fatal(err)
			}
			if generated != tc.wantGenerated {
				t.Fatalf("generated = %v, want %v", generated, tc.wantGenerated)
			}
			if tc.wantGenerated {
				if token == "" {
					t.Fatal("generated token is empty")
				}
			} else if token != tc.wantToken {
				t.Fatalf("token = %q, want %q", token, tc.wantToken)
			}
			if reg != tc.wantReg {
				t.Fatalf("registration token = %q, want %q", reg, tc.wantReg)
			}
		})
	}
}

func TestServeHubSystemdEnvFileContent(t *testing.T) {
	content, err := serveHubSystemdEnvFileContent("tok", "reg")
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	if !strings.Contains(s, "TERM_LLM_HUB_TOKEN=tok\n") {
		t.Fatalf("missing bearer token line:\n%s", s)
	}
	if !strings.Contains(s, "TERM_LLM_HUB_REGISTRATION_TOKEN=reg\n") {
		t.Fatalf("missing registration token line:\n%s", s)
	}

	content, err = serveHubSystemdEnvFileContent("tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "TERM_LLM_HUB_REGISTRATION_TOKEN") {
		t.Fatalf("empty registration token should be omitted:\n%s", content)
	}

	for _, bad := range []string{"has space", "has\nnewline", `has"quote`, "has#hash"} {
		if _, err := serveHubSystemdEnvFileContent(bad, ""); err == nil {
			t.Errorf("serveHubSystemdEnvFileContent(%q) succeeded, want error", bad)
		}
	}
}

type fakeSystemctl struct {
	calls  [][]string
	failOn string
}

func (f *fakeSystemctl) run(ctx context.Context, args ...string) error {
	f.calls = append(f.calls, args)
	if f.failOn != "" && args[0] == f.failOn {
		return fmt.Errorf("systemctl %s failed", args[0])
	}
	return nil
}

func newServeHubSystemdInstallerForTest(t *testing.T, fake *fakeSystemctl) serveHubSystemdInstaller {
	t.Helper()
	dir := t.TempDir()
	return serveHubSystemdInstaller{
		unitPath:  filepath.Join(dir, "systemd", "term-llm-hub.service"),
		envPath:   filepath.Join(dir, "term-llm-hub.env"),
		goos:      "linux",
		systemctl: fake.run,
	}
}

func TestServeHubSystemdInstallWritesFilesAndRunsSystemctl(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{}
	in := newServeHubSystemdInstallerForTest(t, fake)
	var out bytes.Buffer
	unit := []byte("[Unit]\nfake unit\n")

	if err := in.install(t.Context(), unit, true, "", "regtok", true, &out); err != nil {
		t.Fatal(err)
	}

	gotUnit, err := os.ReadFile(in.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotUnit, unit) {
		t.Fatalf("unit file = %q, want %q", gotUnit, unit)
	}
	envData, err := os.ReadFile(in.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envData), "TERM_LLM_HUB_TOKEN=") {
		t.Fatalf("env file missing bearer token:\n%s", envData)
	}
	if !strings.Contains(string(envData), "TERM_LLM_HUB_REGISTRATION_TOKEN=regtok\n") {
		t.Fatalf("env file missing registration token:\n%s", envData)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(in.envPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("env file mode = %o, want 0600", perm)
		}
	}
	wantCalls := [][]string{
		{"daemon-reload"},
		{"enable", "term-llm-hub.service"},
		{"reset-failed", "term-llm-hub.service"},
		{"restart", "term-llm-hub.service"},
		{"is-active", "--quiet", "term-llm-hub.service"},
	}
	if !reflect.DeepEqual(fake.calls, wantCalls) {
		t.Fatalf("systemctl calls = %v, want %v", fake.calls, wantCalls)
	}
	if !strings.Contains(out.String(), "generated Hub bearer token: ") {
		t.Fatalf("output missing generated token notice:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "registration: enabled\n") {
		t.Fatalf("output missing registration status:\n%s", out.String())
	}
}

func TestServeHubSystemdInstallPreservesExistingTokens(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{}
	in := newServeHubSystemdInstallerForTest(t, fake)
	existing := "TERM_LLM_HUB_TOKEN=keepme\nTERM_LLM_HUB_REGISTRATION_TOKEN=keepreg\n"
	if err := os.WriteFile(in.envPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	if err := in.install(t.Context(), []byte("[Unit]\n"), true, "", "", false, &out); err != nil {
		t.Fatal(err)
	}

	envData, err := os.ReadFile(in.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envData), "TERM_LLM_HUB_TOKEN=keepme\n") {
		t.Fatalf("existing bearer token not preserved:\n%s", envData)
	}
	if !strings.Contains(string(envData), "TERM_LLM_HUB_REGISTRATION_TOKEN=keepreg\n") {
		t.Fatalf("existing registration token not preserved:\n%s", envData)
	}
	if strings.Contains(out.String(), "generated Hub bearer token") {
		t.Fatalf("token should not be regenerated:\n%s", out.String())
	}
}

func TestServeHubSystemdInstallExplicitEmptyRegistrationDisables(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{}
	in := newServeHubSystemdInstallerForTest(t, fake)
	existing := "TERM_LLM_HUB_TOKEN=keepme\nTERM_LLM_HUB_REGISTRATION_TOKEN=keepreg\n"
	if err := os.WriteFile(in.envPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	if err := in.install(t.Context(), []byte("[Unit]\n"), true, "", "", true, &out); err != nil {
		t.Fatal(err)
	}

	envData, err := os.ReadFile(in.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envData), "TERM_LLM_HUB_REGISTRATION_TOKEN") {
		t.Fatalf("registration token should be removed:\n%s", envData)
	}
	if !strings.Contains(out.String(), "registration: disabled\n") {
		t.Fatalf("output missing disabled registration status:\n%s", out.String())
	}
}

func TestServeHubSystemdInstallFixesPermissiveEnvFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes not meaningful on windows")
	}
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{}
	in := newServeHubSystemdInstallerForTest(t, fake)
	if err := os.WriteFile(in.envPath, []byte("TERM_LLM_HUB_TOKEN=keepme\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := in.install(t.Context(), []byte("[Unit]\n"), true, "", "", false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(in.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("env file mode after reinstall = %o, want 0600", perm)
	}
}

func TestServeHubSystemdInstallAttributesBadExistingEnvValue(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{}
	in := newServeHubSystemdInstallerForTest(t, fake)
	if err := os.WriteFile(in.envPath, []byte("TERM_LLM_HUB_TOKEN=\"has space\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := in.install(t.Context(), []byte("[Unit]\n"), true, "", "", false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), in.envPath) {
		t.Fatalf("err = %v, want error naming %s", err, in.envPath)
	}
}

func TestServeHubSystemdInstallFlagOverridesExistingToken(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{}
	in := newServeHubSystemdInstallerForTest(t, fake)
	if err := os.WriteFile(in.envPath, []byte("TERM_LLM_HUB_TOKEN=keepme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	if err := in.install(t.Context(), []byte("[Unit]\n"), true, "newtok", "", false, &out); err != nil {
		t.Fatal(err)
	}

	envData, err := os.ReadFile(in.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envData), "TERM_LLM_HUB_TOKEN=newtok\n") {
		t.Fatalf("flag token not written:\n%s", envData)
	}
	if strings.Contains(string(envData), "keepme") {
		t.Fatalf("old token still present:\n%s", envData)
	}
}

func TestServeHubSystemdInstallRequiresLinux(t *testing.T) {
	fake := &fakeSystemctl{}
	in := newServeHubSystemdInstallerForTest(t, fake)
	in.goos = "darwin"

	err := in.install(t.Context(), []byte("[Unit]\n"), true, "", "", false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("err = %v, want linux-only error", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("systemctl should not run on non-linux, got %v", fake.calls)
	}
	if _, statErr := os.Stat(in.unitPath); !os.IsNotExist(statErr) {
		t.Fatalf("unit file should not be written on non-linux (stat err %v)", statErr)
	}
}

func TestServeHubSystemdInstallSystemctlFailure(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{failOn: "daemon-reload"}
	in := newServeHubSystemdInstallerForTest(t, fake)

	err := in.install(t.Context(), []byte("[Unit]\n"), true, "tok", "", false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "daemon-reload") {
		t.Fatalf("err = %v, want daemon-reload failure", err)
	}
	if !reflect.DeepEqual(fake.calls, [][]string{{"daemon-reload"}}) {
		t.Fatalf("enable/restart should be skipped after failure, got %v", fake.calls)
	}
}

func TestServeHubSystemdInstallIgnoresResetFailedError(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{failOn: "reset-failed"}
	in := newServeHubSystemdInstallerForTest(t, fake)

	if err := in.install(t.Context(), []byte("[Unit]\n"), true, "tok", "", false, &bytes.Buffer{}); err != nil {
		t.Fatalf("reset-failed errors should be ignored, got %v", err)
	}
	if got := len(fake.calls); got != 5 {
		t.Fatalf("systemctl call count = %d (%v), want 5", got, fake.calls)
	}
}

func TestServeHubSystemdInstallReportsNotRunning(t *testing.T) {
	defer resetHubRegistrationForTest()()
	t.Setenv("TERM_LLM_HUB_TOKEN", "")
	fake := &fakeSystemctl{failOn: "is-active"}
	in := newServeHubSystemdInstallerForTest(t, fake)
	var out bytes.Buffer

	err := in.install(t.Context(), []byte("[Unit]\n"), true, "tok", "", false, &out)
	if err == nil || !strings.Contains(err.Error(), "journalctl") {
		t.Fatalf("err = %v, want not-running error with journalctl hint", err)
	}
	if strings.Contains(out.String(), "Installed") {
		t.Fatalf("success summary should not print when the service is down:\n%s", out.String())
	}
}

func TestServeHubSystemdPrintsUnit(t *testing.T) {
	clearServeHubSystemdCmdFlagsForTest(t)

	stdout, stderr, err := executeRootForContainTest(t, "serve", "hub", "systemd", "--host", "0.0.0.0")
	if err != nil {
		t.Fatalf("execute: %v (stderr %q)", err, stderr)
	}
	for _, want := range []string{"[Unit]", "ExecStart=", "--host=0.0.0.0", "EnvironmentFile=-/etc/term-llm-hub.env"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "--install") {
		t.Fatalf("stderr should mention --install:\n%s", stderr)
	}
}

func TestServeHubSystemdRejectsPublicNoAuth(t *testing.T) {
	clearServeHubSystemdCmdFlagsForTest(t)

	_, _, err := executeRootForContainTest(t, "serve", "hub", "systemd", "--auth", "none", "--host", "0.0.0.0")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err = %v, want loopback-only error", err)
	}
}
