package clipboard

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestOSC52Sequence(t *testing.T) {
	text := "hello **world**"
	got := OSC52Sequence(text)

	// Verify format: ESC ] 52 ; c ; <base64> ESC backslash
	payload := base64.StdEncoding.EncodeToString([]byte(text))
	want := "\033]52;c;" + payload + "\033\\"
	if got != want {
		t.Fatalf("OSC52Sequence() = %q, want %q", got, want)
	}

	// Verify payload round-trips
	// Extract base64 portion between "c;" and ESC
	prefix := "\033]52;c;"
	suffix := "\033\\"
	if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) {
		t.Fatalf("unexpected framing in %q", got)
	}
	b64 := got[len(prefix) : len(got)-len(suffix)]
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("invalid base64 in sequence: %v", err)
	}
	if string(decoded) != text {
		t.Fatalf("round-trip decoded = %q, want %q", string(decoded), text)
	}
}

func TestOSC52SequenceTmux(t *testing.T) {
	// Simulate tmux environment
	old := os.Getenv("TMUX")
	os.Setenv("TMUX", "/tmp/tmux-1000/default,12345,0")
	defer os.Setenv("TMUX", old)

	got := OSC52Sequence("hi")

	// Must be wrapped in DCS passthrough: ESC P tmux; ... ESC backslash
	if !strings.HasPrefix(got, "\033Ptmux;") {
		t.Fatalf("expected tmux DCS prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "\033\\") {
		t.Fatalf("expected ST suffix, got %q", got)
	}
	// Inner ESC chars should be doubled
	inner := got[len("\033Ptmux;") : len(got)-len("\033\\")]
	if !strings.Contains(inner, "\033\033]52;c;") {
		t.Fatalf("expected doubled ESC in inner sequence, got %q", inner)
	}
}

func TestOSC52SequenceScreen(t *testing.T) {
	// Simulate GNU screen environment
	oldTmux := os.Getenv("TMUX")
	oldSty := os.Getenv("STY")
	os.Setenv("TMUX", "")
	os.Setenv("STY", "12345.pts-0.hostname")
	defer func() {
		os.Setenv("TMUX", oldTmux)
		os.Setenv("STY", oldSty)
	}()

	got := OSC52Sequence("hi")
	if !strings.HasPrefix(got, "\033P") {
		t.Fatalf("expected screen DCS prefix, got %q", got)
	}
	if !strings.Contains(got, "\033]52;c;") {
		t.Fatalf("expected OSC 52 inside DCS, got %q", got)
	}
}

func TestCopyTextBestEffortOrdering(t *testing.T) {
	tests := []struct {
		name        string
		preferOSC52 bool
		nativeErr   error
		osc52Err    error
		wantMethod  CopyMethod
		wantCalls   []CopyMethod
		wantErr     bool
	}{
		{name: "local native success stops", wantMethod: CopyMethodNative, wantCalls: []CopyMethod{CopyMethodNative}},
		{name: "local native failure falls back", nativeErr: errors.New("native unavailable"), wantMethod: CopyMethodOSC52, wantCalls: []CopyMethod{CopyMethodNative, CopyMethodOSC52}},
		{name: "interactive SSH OSC 52 success stops", preferOSC52: true, wantMethod: CopyMethodOSC52, wantCalls: []CopyMethod{CopyMethodOSC52}},
		{name: "interactive SSH OSC 52 failure falls back", preferOSC52: true, osc52Err: errors.New("terminal rejected sequence"), wantMethod: CopyMethodNative, wantCalls: []CopyMethod{CopyMethodOSC52, CopyMethodNative}},
		{name: "both failures", nativeErr: errors.New("native unavailable"), osc52Err: errors.New("terminal unavailable"), wantCalls: []CopyMethod{CopyMethodNative, CopyMethodOSC52}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []CopyMethod
			native := func(text string) error {
				calls = append(calls, CopyMethodNative)
				return tt.nativeErr
			}
			osc52 := func(text string) error {
				calls = append(calls, CopyMethodOSC52)
				return tt.osc52Err
			}

			method, err := copyTextBestEffort("secret payload", tt.preferOSC52, native, osc52)
			if method != tt.wantMethod {
				t.Fatalf("method = %q, want %q", method, tt.wantMethod)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tt.wantErr)
			}
			if strings.Join(copyMethodsToStrings(calls), ",") != strings.Join(copyMethodsToStrings(tt.wantCalls), ",") {
				t.Fatalf("calls = %v, want %v", calls, tt.wantCalls)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "native unavailable") || !strings.Contains(err.Error(), "terminal unavailable") {
					t.Fatalf("combined error = %q", err)
				}
				if strings.Contains(err.Error(), "secret payload") {
					t.Fatalf("error leaked copied content: %q", err)
				}
			}
		})
	}
}

func TestCopyTextBestEffortJoinsBackendErrors(t *testing.T) {
	nativeErr := errors.New("native sentinel")
	osc52Err := errors.New("OSC sentinel")
	_, err := copyTextBestEffort("secret payload", false,
		func(string) error { return nativeErr },
		func(string) error { return osc52Err },
	)
	if !errors.Is(err, nativeErr) || !errors.Is(err, osc52Err) {
		t.Fatalf("joined error = %v, want both backend causes", err)
	}
	if strings.Contains(err.Error(), "secret payload") {
		t.Fatalf("joined error leaked payload: %q", err)
	}
}

func copyMethodsToStrings(methods []CopyMethod) []string {
	result := make([]string, len(methods))
	for i, method := range methods {
		result[i] = string(method)
	}
	return result
}

func TestCopyTextOSC52RejectsOverCapBeforeTTYWrite(t *testing.T) {
	err := CopyTextOSC52(strings.Repeat("x", osc52MaxPayloadBytes+1))
	if err == nil || !strings.Contains(err.Error(), "too large for terminal clipboard transfer") {
		t.Fatalf("error = %v", err)
	}
}

func TestCopyTextBestEffortOSC52PayloadCap(t *testing.T) {
	t.Run("exact boundary uses OSC 52 unchanged", func(t *testing.T) {
		text := strings.Repeat("x", osc52MaxPayloadBytes)
		var got string
		method, err := copyTextBestEffort(text, true,
			func(string) error { return errors.New("native should not run") },
			func(value string) error { got = value; return nil },
		)
		if err != nil || method != CopyMethodOSC52 {
			t.Fatalf("method=%q error=%v", method, err)
		}
		if got != text {
			t.Fatal("OSC 52 backend received modified boundary payload")
		}
	})

	t.Run("over cap skips OSC 52 and falls back native", func(t *testing.T) {
		text := strings.Repeat("界", osc52MaxPayloadBytes/3+1)
		oscCalled := false
		var got string
		method, err := copyTextBestEffort(text, true,
			func(value string) error { got = value; return nil },
			func(string) error { oscCalled = true; return nil },
		)
		if err != nil || method != CopyMethodNative {
			t.Fatalf("method=%q error=%v", method, err)
		}
		if oscCalled {
			t.Fatal("over-cap payload was emitted through OSC 52")
		}
		if got != text {
			t.Fatal("native fallback received truncated or modified payload")
		}
	})

	t.Run("over cap and native failure explains terminal limit", func(t *testing.T) {
		text := strings.Repeat("x", osc52MaxPayloadBytes+1)
		method, err := copyTextBestEffort(text, true,
			func(string) error { return errors.New("native unavailable") },
			func(string) error { t.Fatal("over-cap OSC 52 backend called"); return nil },
		)
		if method != "" || err == nil {
			t.Fatalf("method=%q error=%v", method, err)
		}
		if !strings.Contains(err.Error(), "too large for terminal clipboard transfer") || strings.Contains(err.Error(), text) {
			t.Fatalf("error = %q", err)
		}
	})
}

func TestCopyTextBestEffortPassesPayloadUnchanged(t *testing.T) {
	payloads := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "unicode", text: "héllo 世界"},
		{name: "large", text: strings.Repeat("large\n", 10_000)},
	}
	for _, payload := range payloads {
		t.Run(payload.name, func(t *testing.T) {
			text := payload.text
			var got string
			method, err := copyTextBestEffort(text, false,
				func(value string) error { got = value; return nil },
				func(string) error { t.Fatal("fallback unexpectedly called"); return nil },
			)
			if err != nil || method != CopyMethodNative {
				t.Fatalf("method=%q error=%v", method, err)
			}
			if got != text {
				t.Fatalf("payload changed: got length %d, want %d", len(got), len(text))
			}
		})
	}
}

func TestInteractiveSSHSessionRequiresUsableTTY(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		usableTTY bool
		want      bool
	}{
		{name: "SSH with usable controlling TTY", env: map[string]string{"SSH_CONNECTION": "client 1 server 2", "TERM": "xterm-256color"}, usableTTY: true, want: true},
		{name: "SSH without controlling TTY", env: map[string]string{"SSH_CONNECTION": "client 1 server 2", "TERM": "xterm-256color"}, usableTTY: false},
		{name: "SSH dumb terminal", env: map[string]string{"SSH_TTY": "/dev/pts/1", "TERM": "dumb"}, usableTTY: true},
		{name: "local usable TTY", env: map[string]string{"TERM": "xterm-256color"}, usableTTY: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ttyCalls := 0
			got := isInteractiveSSHSessionWith(func(key string) string {
				return tt.env[key]
			}, func() bool {
				ttyCalls++
				return tt.usableTTY
			})
			if got != tt.want {
				t.Fatalf("isInteractiveSSHSessionWith() = %t, want %t", got, tt.want)
			}
			if tt.env["SSH_CONNECTION"] != "" && tt.env["TERM"] != "dumb" && ttyCalls != 1 {
				t.Fatalf("usable TTY probe calls = %d, want 1", ttyCalls)
			}
		})
	}
}

func TestDetectPreferredImageMIME(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "prefers png when available",
			in:   "text/plain\nimage/jpeg\nimage/png\n",
			want: "image/png",
		},
		{
			name: "accepts jpeg when png absent",
			in:   "text/plain\nimage/jpg\n",
			want: "image/jpeg",
		},
		{
			name: "accepts first image type as fallback",
			in:   "text/plain\nimage/bmp\napplication/json\n",
			want: "image/bmp",
		},
		{
			name: "ignores non image types",
			in:   "text/plain\napplication/octet-stream\n",
			want: "",
		},
		{
			name: "strips mime parameters",
			in:   "image/webp; charset=binary\n",
			want: "image/webp",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := detectPreferredImageMIME(tc.in)
			if got != tc.want {
				t.Fatalf("detectPreferredImageMIME() = %q, want %q", got, tc.want)
			}
		})
	}
}
