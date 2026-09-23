module charm.land/bubbletea/v2

go 1.26.0

require (
	github.com/charmbracelet/colorprofile v0.4.3
	github.com/charmbracelet/ultraviolet v0.0.0-20260416155717-489999b90468
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/charmbracelet/x/exp/golden v0.0.0-20241212170349-ad4b7ae0f25f
	github.com/charmbracelet/x/term v0.2.2
	github.com/creack/pty v1.1.24
	github.com/lucasb-eyer/go-colorful v1.4.1
	github.com/muesli/cancelreader v0.2.2
	golang.org/x/sys v0.48.0
)

require (
	github.com/aymanbagabas/go-udiff v0.2.0 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/mattn/go-runewidth v0.0.30 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v1.2.0 // indirect
)

replace github.com/charmbracelet/ultraviolet => ../renderer
