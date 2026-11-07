package shellcmd

import (
	"reflect"
	"testing"
)

func TestInteractiveArgsSemantics(t *testing.T) {
	tests := []struct {
		shell string
		want  []string
	}{
		{"/bin/bash", []string{"--noprofile", "--norc"}},
		{"/usr/bin/fish", []string{"--no-config"}},
		{"powershell.exe", []string{"-NoLogo", "-NoProfile"}},
		{"cmd.exe", nil},
		{"/bin/sh", nil},
	}
	for _, tt := range tests {
		got, err := InteractiveArgs(tt.shell, true)
		if err != nil {
			t.Fatalf("InteractiveArgs(%q): %v", tt.shell, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("InteractiveArgs(%q) = %v, want %v", tt.shell, got, tt.want)
		}
	}
	if _, err := InteractiveArgs("/bin/unknown", true); err == nil {
		t.Fatal("unsupported shell did not error")
	}
}

func TestApprovalCommandPreservesPosixAndNativeArgv(t *testing.T) {
	if got, want := ApprovalCommand("/bin/bash", "echo hi", nil), "echo hi"; got != want {
		t.Fatalf("ApprovalCommand = %q, want %q", got, want)
	}
	if got, want := ApprovalCommand("/bin/bash", "echo hi", map[string]string{"K": "v"}),
		`env -- 'K=v' '/bin/bash' -c 'echo hi'`; got != want {
		t.Fatalf("ApprovalCommand env = %q, want %q", got, want)
	}
}
