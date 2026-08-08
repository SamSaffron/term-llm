// Package terminalpolicy centralizes decisions about whether term-llm may start
// an interactive terminal UI. A controlling TTY alone is not sufficient when
// the command's invoking streams are redirected or captured.
package terminalpolicy

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// Interactive reports whether input and output are terminals and the process
// environment permits an interactive UI.
func Interactive(input, output *os.File) bool {
	return InteractiveWith(input, output, term.IsTerminal, os.Getenv)
}

// OutputInteractive reports whether output can safely present an interactive
// prompt. It is useful when input intentionally comes from /dev/tty rather than
// the process stdin.
func OutputInteractive(output *os.File) bool {
	return OutputInteractiveWith(output, term.IsTerminal, os.Getenv)
}

// OutputInteractiveWith is the testable form of OutputInteractive.
func OutputInteractiveWith(output *os.File, isTerminal func(int) bool, getenv func(string) string) bool {
	if output == nil || isTerminal == nil || getenv == nil {
		return false
	}
	if EnvironmentEnabled(getenv("CI")) || getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(int(output.Fd()))
}

// InteractiveWith is the testable form of Interactive.
func InteractiveWith(input, output *os.File, isTerminal func(int) bool, getenv func(string) string) bool {
	if input == nil || output == nil || isTerminal == nil || getenv == nil {
		return false
	}
	if EnvironmentEnabled(getenv("CI")) || getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(int(input.Fd())) && isTerminal(int(output.Fd()))
}

// EnvironmentEnabled interprets conventional boolean environment values.
func EnvironmentEnabled(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "0" && !strings.EqualFold(value, "false") &&
		!strings.EqualFold(value, "no") && !strings.EqualFold(value, "off") &&
		!strings.EqualFold(value, "disabled")
}
