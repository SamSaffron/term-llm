package testutil

import (
	"os"
	"testing"
)

func TestProcessHasExited(t *testing.T) {
	if ProcessHasExited(os.Getpid()) {
		t.Fatal("current process reported as exited")
	}
	if !ProcessHasExited(1 << 30) {
		t.Fatal("nonexistent process reported as running")
	}
}

func TestProcStatState(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want byte
		ok   bool
	}{
		{name: "running", stat: "123 (sleep) S 1 2 3", want: 'S', ok: true},
		{name: "zombie", stat: "123 (sleep) Z 1 2 3", want: 'Z', ok: true},
		{name: "parenthesis in name", stat: "123 (sleep) helper) Z 1 2 3", want: 'Z', ok: true},
		{name: "missing name", stat: "123 sleep Z 1 2 3", ok: false},
		{name: "missing state", stat: "123 (sleep)", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := procStatState([]byte(tt.stat))
			if got != tt.want || ok != tt.ok {
				t.Fatalf("procStatState(%q) = %q, %v; want %q, %v", tt.stat, got, ok, tt.want, tt.ok)
			}
		})
	}
}
