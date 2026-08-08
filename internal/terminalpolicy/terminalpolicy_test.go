package terminalpolicy

import (
	"os"
	"testing"
)

func TestInteractiveWith(t *testing.T) {
	tests := []struct {
		name      string
		inputTTY  bool
		outputTTY bool
		ci        string
		term      string
		want      bool
	}{
		{name: "interactive", inputTTY: true, outputTTY: true, term: "xterm-256color", want: true},
		{name: "redirected input", outputTTY: true, term: "xterm-256color"},
		{name: "captured output", inputTTY: true, term: "xterm-256color"},
		{name: "CI", inputTTY: true, outputTTY: true, ci: "1", term: "xterm-256color"},
		{name: "CI false", inputTTY: true, outputTTY: true, ci: "false", term: "xterm-256color", want: true},
		{name: "CI zero", inputTTY: true, outputTTY: true, ci: "0", term: "xterm-256color", want: true},
		{name: "dumb terminal", inputTTY: true, outputTTY: true, term: "dumb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(name string) string {
				switch name {
				case "CI":
					return tt.ci
				case "TERM":
					return tt.term
				default:
					return ""
				}
			}
			isTerminal := func(fd int) bool {
				switch fd {
				case int(os.Stdin.Fd()):
					return tt.inputTTY
				case int(os.Stderr.Fd()):
					return tt.outputTTY
				default:
					return false
				}
			}
			if got := InteractiveWith(os.Stdin, os.Stderr, isTerminal, getenv); got != tt.want {
				t.Fatalf("InteractiveWith() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestOutputInteractiveWith(t *testing.T) {
	isTerminal := func(fd int) bool { return fd == int(os.Stderr.Fd()) }
	getenv := func(string) string { return "" }
	if !OutputInteractiveWith(os.Stderr, isTerminal, getenv) {
		t.Fatal("terminal stderr should be interactive output")
	}
	if OutputInteractiveWith(os.Stdout, isTerminal, getenv) {
		t.Fatal("captured stdout should not be interactive output")
	}
	ciEnv := func(name string) string {
		if name == "CI" {
			return "1"
		}
		return ""
	}
	if OutputInteractiveWith(os.Stderr, isTerminal, ciEnv) {
		t.Fatal("CI output should not be interactive")
	}
}

func TestInteractiveWithRejectsMissingDependencies(t *testing.T) {
	getenv := func(string) string { return "" }
	isTerminal := func(int) bool { return true }
	if InteractiveWith(nil, os.Stderr, isTerminal, getenv) {
		t.Fatal("nil input should not be interactive")
	}
	if InteractiveWith(os.Stdin, nil, isTerminal, getenv) {
		t.Fatal("nil output should not be interactive")
	}
	if InteractiveWith(os.Stdin, os.Stderr, nil, getenv) {
		t.Fatal("nil terminal probe should not be interactive")
	}
	if InteractiveWith(os.Stdin, os.Stderr, isTerminal, nil) {
		t.Fatal("nil environment lookup should not be interactive")
	}
}
