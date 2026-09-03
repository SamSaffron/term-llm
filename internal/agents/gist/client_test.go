package gist

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCheckGHUsesActiveAccountAPI(t *testing.T) {
	var gotName string
	var gotArgs []string
	client := &Client{execCommand: func(name string, args ...string) *exec.Cmd {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return gistTestCommand(t, 0, "")
	}}

	if err := client.checkGH(); err != nil {
		t.Fatal(err)
	}
	if gotName != "gh" {
		t.Fatalf("command = %q, want gh", gotName)
	}
	if want := []string{"api", "user", "--silent"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("arguments = %q, want %q", gotArgs, want)
	}
}

func TestCheckGHAuthenticationFailureIsConcise(t *testing.T) {
	const diagnostic = "token ghp_example and unrelated inactive account details"
	client := &Client{execCommand: func(string, ...string) *exec.Cmd {
		return gistTestCommand(t, 1, diagnostic)
	}}

	err := client.checkGH()
	if err == nil {
		t.Fatal("checkGH succeeded, want authentication error")
	}
	if !strings.Contains(err.Error(), "gh auth login --hostname github.com") {
		t.Fatalf("error lacks login guidance: %v", err)
	}
	if strings.Contains(err.Error(), diagnostic) || strings.Contains(err.Error(), "ghp_example") {
		t.Fatalf("error exposed gh diagnostic: %v", err)
	}
}

func TestCheckGHReportsMissingCLI(t *testing.T) {
	client := &Client{execCommand: func(string, ...string) *exec.Cmd {
		return exec.Command(filepath.Join(t.TempDir(), "missing-gh"))
	}}

	err := client.checkGH()
	if err == nil || !strings.Contains(err.Error(), "gh CLI not found") {
		t.Fatalf("error = %v, want missing CLI guidance", err)
	}
}

func gistTestCommand(t *testing.T, exitCode int, stderr string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestGistCommandHelper$")
	cmd.Env = append(os.Environ(),
		"TERM_LLM_GIST_HELPER=1",
		fmt.Sprintf("TERM_LLM_GIST_HELPER_EXIT=%d", exitCode),
		"TERM_LLM_GIST_HELPER_STDERR="+stderr,
	)
	return cmd
}

func TestGistCommandHelper(t *testing.T) {
	if os.Getenv("TERM_LLM_GIST_HELPER") != "1" {
		return
	}
	exitCode, err := strconv.Atoi(os.Getenv("TERM_LLM_GIST_HELPER_EXIT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Fprint(os.Stderr, os.Getenv("TERM_LLM_GIST_HELPER_STDERR"))
	os.Exit(exitCode)
}

func TestGistWriteFailuresDoNotExposeStderr(t *testing.T) {
	const diagnostic = "SECRET ghp_example remote diagnostic"
	client := &Client{execCommand: func(string, ...string) *exec.Cmd {
		return gistTestCommand(t, 1, diagnostic)
	}}
	for name, run := range map[string]func() error{
		"create": func() error {
			_, err := client.Create("description", false, map[string]string{"index.html": "hello"})
			return err
		},
		"update": func() error {
			return client.Update("abc123", map[string]string{"index.html": "hello"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil {
				t.Fatal("write succeeded unexpectedly")
			}
			for current := err; current != nil; current = errors.Unwrap(current) {
				if strings.Contains(current.Error(), diagnostic) || strings.Contains(current.Error(), "ghp_example") {
					t.Fatalf("error chain exposed stderr: %v", current)
				}
			}
		})
	}
}

func TestParseGistRef(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "raw gist ID",
			input: "abc123def456",
			want:  "abc123def456",
		},
		{
			name:  "full URL with https",
			input: "https://gist.github.com/user/abc123def456",
			want:  "abc123def456",
		},
		{
			name:  "URL without scheme",
			input: "gist.github.com/user/abc123def456",
			want:  "abc123def456",
		},
		{
			name:  "URL with http",
			input: "http://gist.github.com/user/abc123def456",
			want:  "abc123def456",
		},
		{
			name:  "URL with trailing whitespace",
			input: "  https://gist.github.com/user/abc123def456  ",
			want:  "abc123def456",
		},
		{
			name:  "long gist ID",
			input: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
			want:  "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
		},
		{
			name:    "invalid - contains uppercase",
			input:   "ABC123",
			wantErr: true,
		},
		{
			name:    "invalid - random string",
			input:   "not-a-gist",
			wantErr: true,
		},
		{
			name:    "invalid - wrong domain",
			input:   "https://github.com/user/repo",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGistRef(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseGistRef() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseGistRef() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetURL(t *testing.T) {
	got := GetURL("abc123def456")
	want := "https://gist.github.com/abc123def456"
	if got != want {
		t.Errorf("GetURL() = %v, want %v", got, want)
	}
}
