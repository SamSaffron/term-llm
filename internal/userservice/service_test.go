package userservice

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixtureSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{Version: 1, Kind: "web", Binary: "/opt/term llm/bin/term-llm", Directory: t.TempDir(), Auth: "passkey", Host: "127.0.0.1", Port: 8080, URL: "http://localhost:8080/ui/", BasePath: "/ui", Environment: map[string]string{"HOME": t.TempDir(), "PATH": "/usr/bin"}, Args: []string{"--auth", "passkey"}}
}
func TestNativeDefinitions(t *testing.T) {
	s := fixtureSpec(t)
	s.Binary = "/opt/quote\" dollar$percent%/term-llm"
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			n := Native{OS: platform, Home: t.TempDir(), ConfigHome: t.TempDir(), UID: 1000}
			path := filepath.Join(t.TempDir(), "service.json")
			data, err := n.Render(s, path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), Marker) || !strings.Contains(string(data), "service") {
				t.Fatalf("invalid definition %s", data)
			}
			if platform == "darwin" {
				dec := xml.NewDecoder(strings.NewReader(string(data)))
				for {
					_, err := dec.Token()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if strings.Contains(string(data), `WorkingDirectory="`) {
					t.Fatal("systemd WorkingDirectory must not use ExecStart quoting")
				}
				if !strings.Contains(string(data), "$$") || !strings.Contains(string(data), "%%") {
					t.Fatal("systemd expansion not escaped")
				}
			}
			if err = n.Install(s, path); err != nil {
				t.Fatal(err)
			}
			if err = n.CheckOwned("web", path); err != nil {
				t.Fatal(err)
			}
			if err = n.CheckOwned("web", filepath.Join(t.TempDir(), "other.json")); err == nil {
				t.Fatal("adopted foreign service")
			}
		})
	}
}
func TestNativeLifecycle(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			calls := []string{}
			n := Native{OS: platform, Home: t.TempDir(), ConfigHome: t.TempDir(), UID: 1000, Run: func(_ context.Context, exe string, args ...string) ([]byte, error) {
				calls = append(calls, exe+" "+strings.Join(args, " "))
				return nil, nil
			}}
			if err := n.Start(context.Background(), "hub", true); err != nil {
				t.Fatal(err)
			}
			if err := n.Stop(context.Background(), "hub"); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(calls, "\n")
			if platform == "linux" {
				if !strings.Contains(joined, "systemctl --user restart term-llm-hub.service") || !strings.Contains(joined, "disable --now") {
					t.Fatal(joined)
				}
			} else {
				if !strings.Contains(joined, "launchctl kickstart -k gui/1000/com.term-llm.hub") || strings.Contains(joined, "launchctl bootstrap") {
					t.Fatal(joined)
				}
			}
		})
	}
}
func TestPrivateFilesAndSpec(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "service.json")
	s := fixtureSpec(t)
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Kind != s.Kind {
		t.Fatalf("roundtrip %+v %v", got, err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(path); err == nil {
		t.Fatal("accepted public credentials")
	}
	target := filepath.Join(root, "target")
	if err = os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err = os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err = WritePrivate(link, []byte("replacement")); err == nil {
		t.Fatal("followed symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "original" {
		t.Fatal("modified target")
	}
}
func TestCredentialImportAndEnvironment(t *testing.T) {
	for _, name := range []string{"HOME", "PATH", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "BASH_ENV", "TERM_LLM_SERVE_BOOTSTRAP_TOKEN", "lowercase_TOKEN"} {
		if SecretName(name) {
			t.Errorf("accepted dangerous/noncredential variable %s", name)
		}
	}
	c := Credentials{OS: "linux", Dir: t.TempDir(), Kind: "web"}
	value := "quotes'\" dollar$ backslash\\ literal"
	if err := c.Put(context.Background(), "OPENAI_API_KEY", value); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(context.Background(), "OPENAI_API_KEY")
	if err != nil || got != value {
		t.Fatalf("credential roundtrip %q %v", got, err)
	}
	file := filepath.Join(t.TempDir(), "secrets.env")
	_ = os.WriteFile(file, []byte("# literal\nOPENAI_API_KEY="+value+"\n"), 0600)
	imported, err := ImportSecrets(file)
	if err != nil || imported["OPENAI_API_KEY"] != value {
		t.Fatalf("import %v %v", imported, err)
	}
	_ = os.WriteFile(file, []byte("OPENAI_API_KEY=a\nOPENAI_API_KEY=b\n"), 0600)
	if _, err = ImportSecrets(file); err == nil {
		t.Fatal("accepted duplicate")
	}
	s := fixtureSpec(t)
	env := strings.Join(RunnerEnvironment(s, []string{"BASH_ENV=attack", "OPENAI_API_KEY=ambient", "DISPLAY=:1", "LD_PRELOAD=attack"}, map[string]string{"OPENAI_API_KEY": "explicit"}), "\n")
	if strings.Contains(env, "attack") || strings.Contains(env, "ambient") || !strings.Contains(env, "explicit") || !strings.Contains(env, "DISPLAY=:1") {
		t.Fatal(env)
	}
}
func TestRefuseManualUnit(t *testing.T) {
	n := Native{OS: "linux", ConfigHome: t.TempDir()}
	path := n.Path("web")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = os.WriteFile(path, []byte("[Service]\nExecStart=/custom\n"), 0600)
	if err := n.CheckOwned("web", "/spec"); err == nil {
		t.Fatal("accepted manual service")
	}
}

func TestKeychainInputNeverInterpretsCredentialSyntax(t *testing.T) {
	c := Credentials{OS: "darwin", Kind: "web", Dir: t.TempDir()}
	secret := "quote'\" newline\nadd-generic-password -A; shell$()\\"
	input, err := c.keychainInput("OPENAI_API_KEY", secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(input, "\n") != 1 || strings.Contains(input, secret) || !strings.HasSuffix(input, base64.StdEncoding.EncodeToString([]byte(secret))+"\n") {
		t.Fatal("credential escaped into interactive command syntax")
	}
	if _, err = c.keychainInput("OPENAI_API_KEY", strings.Repeat("x", 4096)); err == nil {
		t.Fatal("accepted truncated Keychain input")
	}
}

func TestLoadedForeignDefinitionIsNotAdopted(t *testing.T) {
	source := filepath.Join(t.TempDir(), "foreign.service")
	_ = os.WriteFile(source, []byte("[Service]\nExecStart=/foreign\n"), 0600)
	n := Native{OS: "linux", Run: func(context.Context, string, ...string) ([]byte, error) { return []byte(source + "\n"), nil }}
	if err := n.CheckLoaded(context.Background(), "web", "/our/service.json"); err == nil {
		t.Fatal("adopted foreign manager definition")
	}
	n.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	if err := n.CheckLoaded(context.Background(), "web", "/our/service.json"); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinStartAndReconcile(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		loaded, restart, reconcile bool
		want                       []string
	}{
		{"start loaded", true, false, false, []string{"kickstart gui/501/com.term-llm.web"}},
		{"restart loaded", true, true, false, []string{"kickstart -k gui/501/com.term-llm.web"}},
		{"start unloaded", false, false, false, []string{"bootstrap"}},
		{"restart unloaded", false, true, false, []string{"bootstrap"}},
		{"reconcile changed", true, true, true, []string{"bootout gui/501/com.term-llm.web", "bootstrap"}},
		{"reconcile unchanged", true, false, true, []string{"kickstart gui/501/com.term-llm.web"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			n := Native{OS: "darwin", Home: t.TempDir(), UID: 501}
			n.Run = func(_ context.Context, exe string, args ...string) ([]byte, error) {
				if exe != "launchctl" {
					t.Fatalf("unexpected executable %s", exe)
				}
				calls = append(calls, strings.Join(args, " "))
				if args[0] == "print" && !tc.loaded {
					return nil, errors.New("not loaded")
				}
				return nil, nil
			}
			var err error
			if tc.reconcile {
				err = n.Reconcile(context.Background(), "web", tc.restart)
			} else {
				err = n.Start(context.Background(), "web", tc.restart)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"enable gui/501/com.term-llm.web", "print gui/501/com.term-llm.web"}
			for _, call := range tc.want {
				if call == "bootstrap" {
					call = "bootstrap gui/501 " + n.Path("web")
				}
				want = append(want, call)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
		})
	}
}

func TestDarwinStartFailureDiagnostics(t *testing.T) {
	for _, failAt := range []string{"enable", "bootstrap", "kickstart"} {
		t.Run(failAt, func(t *testing.T) {
			failure := errors.New("exit status 5")
			var calls []string
			n := Native{OS: "darwin", Home: t.TempDir(), UID: 501, Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				calls = append(calls, args[0])
				if args[0] == failAt {
					return []byte("  launchd diagnostic\n"), failure
				}
				if args[0] == "print" && failAt == "bootstrap" {
					return nil, errors.New("not loaded")
				}
				return nil, nil
			}}
			err := n.Start(context.Background(), "web", true)
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), "launchd diagnostic") || !strings.Contains(err.Error(), "launchctl "+failAt) {
				t.Fatalf("missing failure details: %v", err)
			}
			if calls[len(calls)-1] != failAt {
				t.Fatalf("continued after failure: %v", calls)
			}
		})
	}
}
