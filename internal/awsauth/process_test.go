package awsauth

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCredentialProcess(t *testing.T) {
	command := `example --arg "value with spaces" | helper`
	o := testOptions(t, nil, "[default]\ncredential_process = "+command+"\n", "")
	calls := 0
	o.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
		calls++
		wantName, wantArgs := "sh", []string{"-c", command}
		if runtime.GOOS == "windows" {
			wantName, wantArgs = "cmd.exe", []string{"/C", command}
		}
		if name != wantName || !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("command = %q %v", name, args)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 59*time.Second {
			t.Error("unexpected timeout")
		}
		return []byte(`{"Version":1,"AccessKeyId":"process-id","SecretAccessKey":"process-secret","SessionToken":"process-token","Expiration":"2030-01-02T03:04:05Z"}`), nil
	}
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("process not lazy")
	}
	c, err := r.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "process-id" || c.SessionToken != "process-token" || !c.CanExpire || c.Expires.Format(time.RFC3339) != "2030-01-02T03:04:05Z" || c.Source != "credential_process" {
		t.Error("wrong credentials")
	}
}

func TestCredentialProcessInvalidOutputNeverLeaks(t *testing.T) {
	cases := []struct {
		name, output string
		runErr       error
		want         string
	}{
		{"invalid-json", `SECRET-STDOUT`, nil, "invalid credential JSON"},
		{"json-expiration", `{"Version":1,"AccessKeyId":"SECRET-ID","SecretAccessKey":"SECRET-KEY","Expiration":"SECRET-STDOUT"}`, nil, "invalid credential JSON"},
		{"version", `{"Version":2,"AccessKeyId":"SECRET-ID","SecretAccessKey":"SECRET-KEY"}`, nil, "Version must be 1"},
		{"missing-id", `{"Version":1,"SecretAccessKey":"SECRET-KEY"}`, nil, "missing AccessKeyId"},
		{"missing-secret", `{"Version":1,"AccessKeyId":"SECRET-ID"}`, nil, "missing AccessKeyId"},
		{"runner-error", `SECRET-STDOUT`, errors.New("SECRET-STDOUT SECRET-KEY"), "command failed"},
		{"stderr", `{"Version":1,"AccessKeyId":"SECRET-ID","SecretAccessKey":"SECRET-KEY"}`, &exec.ExitError{Stderr: []byte(" useful stderr SECRET-ID SECRET-KEY ")}, "run the command manually"},
		{"stderr-echoes-stdout", `SECRET-STDOUT`, &exec.ExitError{Stderr: []byte("helpful stderr SECRET-STDOUT")}, "run the command manually"},
		// Fresh credentials that appear only on stderr must never surface.
		{"stderr-only-fresh-secrets", ``, &exec.ExitError{Stderr: []byte("minted SECRET-ID SECRET-KEY SECRET-STDOUT")}, "command failed"},
		{"bounded-stderr", `SECRET-STDOUT`, &exec.ExitError{Stderr: []byte(strings.Repeat("x", 10000))}, "command failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := defaultOptions(testOptions(t, nil, "", ""))
			o.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) { return []byte(c.output), c.runErr }
			_, err := processProvider(o, "command").Retrieve(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v", err)
			}
			for _, secret := range []string{"SECRET-STDOUT", "SECRET-KEY", "SECRET-ID"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaked %q", secret)
				}
			}
			if len(err.Error()) > 2200 {
				t.Errorf("error unbounded: %d", len(err.Error()))
			}
		})
	}
}

func TestCredentialCommandContextError(t *testing.T) {
	o := defaultOptions(testOptions(t, nil, "", ""))
	o.RunCommand = func(ctx context.Context, _ string, _ []string, _ []string) ([]byte, error) { return nil, ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := processProvider(o, "command").Retrieve(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Error(err)
	}
}

func TestBoundedCommandBuffer(t *testing.T) {
	b := &boundedCommandBuffer{limit: 4}
	for _, s := range []string{"abc", "def", "ghi"} {
		n, err := b.Write([]byte(s))
		if n != 3 || err != nil {
			t.Fatal(n, err)
		}
	}
	if b.text != "abcd" || !b.overflow {
		t.Errorf("buffer=%#v", b)
	}
}

func TestCLIExportStderrIsRedacted(t *testing.T) {
	o := defaultOptions(testOptions(t, nil, "[profile sso]\nsso_session = s\n", ""))
	o.Profile = "sso"
	o.LookPath = func(string) (string, error) { return "fake-aws", nil }
	stderr := "Error when retrieving token from sso: Token has expired and refresh failed AKIAABCDEFGHIJKLMNOP wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEYabcdef"
	o.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, &exec.ExitError{Stderr: []byte(stderr)}
	}
	_, err := cliProvider(o, "sso", "SSO").Retrieve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Token has expired") {
		t.Fatalf("error = %v, want AWS CLI diagnostic", err)
	}
	for _, secret := range []string{"AKIAABCDEFGHIJKLMNOP", "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEYabcdef"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked secret-like value %q: %v", secret, err)
		}
	}
}
