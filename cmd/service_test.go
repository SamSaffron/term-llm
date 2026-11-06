package cmd

import (
	"slices"
	"strings"
	"testing"
)

func TestServiceLaunchParsing(t *testing.T) {
	originalHost, originalPort, originalCORS := serveFlags.Host, serveFlags.Port, append([]string(nil), serveFlags.CORSOrigins...)
	for _, tc := range []struct {
		name, kind string
		args       []string
		auth, url  string
		bad        bool
	}{
		{name: "web default", kind: "web", auth: "passkey", url: "http://localhost:8080/ui/"},
		{name: "hub default", kind: "hub", auth: "passkey", url: "http://localhost:8090/hub/"},
		{name: "custom port", kind: "web", args: []string{"--auth", "bearer", "--port", "8181"}, auth: "bearer", url: "http://localhost:8181/ui/"},
		{name: "public Hub", kind: "hub", args: []string{"--public-url", "https://hub.example.com/hub/"}, auth: "passkey", url: "https://hub.example.com/hub/"},
		{name: "reverse bearer", kind: "web", args: []string{"--auth", "bearer", "--hub-connect", "reverse", "--hub-register", "--hub-url", "https://hub.example.com/hub/", "--hub-node-id", "laptop"}, auth: "bearer", url: "http://localhost:8080/ui/"},
		{name: "existing no-auth alias", kind: "web", args: []string{"--no-auth"}, auth: "none", url: "http://localhost:8080/ui/"},
		{name: "unsafe secret argv", kind: "web", args: []string{"--token", "do-not-print"}, bad: true},
		{name: "unsafe Hub secret argv", kind: "hub", args: []string{"--registration-token", "do-not-print"}, bad: true},
		{name: "passkey reverse rejected", kind: "web", args: []string{"--hub-connect", " Reverse "}, bad: true},
		{name: "unknown native server flag", kind: "hub", args: []string{"--made-up"}, bad: true},
		{name: "additional platform", kind: "web", args: []string{"jobs"}, bad: true},
		{name: "public unauthenticated", kind: "web", args: []string{"--no-auth", "--host", "0.0.0.0"}, bad: true},
		{name: "mismatched passkey paths", kind: "web", args: []string{"--public-url", "http://localhost:8080/chat/", "--base-path", "/ui"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := serviceApp.ParseLaunch(tc.kind, tc.args)
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted %+v", s)
				}
				if strings.Contains(err.Error(), "do-not-print") {
					t.Fatal("error leaked secret")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Auth != tc.auth || s.URL != tc.url {
				t.Fatalf("got %+v", s)
			}
			again, err := serviceApp.ParseLaunch(tc.kind, s.Args)
			if err != nil || again.URL != s.URL || again.Auth != s.Auth {
				t.Fatalf("not idempotent %+v %v", again, err)
			}
		})
	}
	if serveFlags.Host != originalHost || serveFlags.Port != originalPort || !slices.Equal(serveFlags.CORSOrigins, originalCORS) {
		t.Fatal("installer changed serve globals")
	}
}

func TestServiceStartRestartHaveDebugFlag(t *testing.T) {
	for _, name := range []string{"start", "restart"} {
		child, _, err := serviceCmd.Find([]string{name})
		if err != nil || child.Flags().Lookup("debug") == nil {
			t.Fatalf("%s missing debug flag: %v", name, err)
		}
	}
}

func TestServiceIndependentControlFlags(t *testing.T) {
	for _, tc := range []struct{ command, flag string }{
		{"status", "json"}, {"start", "no-enable"}, {"restart", "no-enable"}, {"stop", "keep-enabled"},
	} {
		child, _, err := serviceCmd.Find([]string{tc.command})
		if err != nil {
			t.Fatal(err)
		}
		flag := child.Flags().Lookup(tc.flag)
		if flag == nil || flag.DefValue != "false" {
			t.Fatalf("%s missing default-false --%s", tc.command, tc.flag)
		}
	}
	for _, name := range []string{"start", "restart", "stop", "open", "autostart"} {
		child, _, err := serviceCmd.Find([]string{name})
		if err != nil || child.Flags().Lookup("json") != nil {
			t.Fatalf("--json must be status-only: %s (%v)", name, err)
		}
	}
}

func TestServiceAutostartArgs(t *testing.T) {
	child, _, err := serviceCmd.Find([]string{"autostart"})
	if err != nil || child == serviceCmd {
		t.Fatalf("missing autostart: %v", err)
	}
	for _, args := range [][]string{{"on"}, {"off"}, {"status"}, {"web", "on"}, {"hub", "off"}, {"web", "status"}} {
		if err := child.ValidateArgs(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{nil, {"web"}, {"other", "on"}, {"web", "enable"}, {"on", "web"}, {"web", "on", "off"}} {
		if err := child.ValidateArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
