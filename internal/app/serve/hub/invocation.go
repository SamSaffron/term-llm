package hub

import (
	"io"

	"golang.org/x/term"
)

// FlagState records explicit flag choices before startup.
type FlagState struct{ BasePath bool }

// Dependencies contains captured invocation writers and explicit flag state.
type Dependencies struct {
	Flags          FlagState
	Stdout, Stderr io.Writer
}

// OutputIsTerminal reports whether the output destination is interactive.
var OutputIsTerminal = func(w any) bool { f, ok := w.(interface{ Fd() uintptr }); return ok && term.IsTerminal(int(f.Fd())) }
