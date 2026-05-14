package termimage

import "image/color"

// Protocol identifies a terminal image rendering protocol/strategy.
type Protocol string

const (
	ProtocolAuto  Protocol = "auto"
	ProtocolKitty Protocol = "kitty"
	ProtocolITerm Protocol = "iterm"
	ProtocolSixel Protocol = "sixel"
	ProtocolANSI  Protocol = "ansi"
	ProtocolNone  Protocol = "none"
)

// Mode identifies the output surface the rendered image is destined for.
type Mode string

const (
	// ModeViewport is Bubble Tea viewport content. Results must be safe to line
	// clip and redraw; raw image upload escapes are returned separately in Upload.
	ModeViewport Mode = "viewport"
	// ModeScrollback is inline/scrollback terminal output. One-shot escape based
	// renderers are acceptable here.
	ModeScrollback Mode = "scrollback"
	// ModeOneShot is direct terminal output outside a redrawable viewport.
	ModeOneShot Mode = "oneshot"
)

const (
	DefaultMaxCols      = 80
	DefaultMaxRows      = 40
	DefaultCellWidthPx  = 10
	DefaultCellHeightPx = 20
)

// Request describes an image render request.
type Request struct {
	Path     string
	MaxCols  int
	MaxRows  int
	Mode     Mode
	Protocol Protocol

	// Background is used by text-based renderers when compositing transparent
	// pixels. If nil or transparent, a dark terminal-style fallback is used.
	Background color.Color

	// AllowEscapeUploads is reserved for callers that want to explicitly document
	// whether they can emit Upload out-of-band. Protocol selection currently uses
	// Protocol=ansi/none for text-only operation.
	AllowEscapeUploads bool

	// CellWidthPx/CellHeightPx optionally override terminal cell pixel size.
	// When zero, the renderer tries to detect the size and falls back to 10x20.
	CellWidthPx  int
	CellHeightPx int
}

// Result is the normalized output shape returned by all rendering strategies.
type Result struct {
	Protocol Protocol

	// Upload contains terminal control bytes that must be emitted directly to the
	// terminal, outside any line-wrapping/slicing viewport content.
	Upload string
	// Display is safe for the requested output surface. For Kitty viewport mode it
	// is only the Unicode placeholder grid. For ANSI it is ordinary ANSI text.
	Display string
	// Full is a one-shot convenience string equal to Upload + Display.
	Full string

	WidthCells  int
	HeightCells int

	TextFallback string
	CacheKey     string
	Warnings     []string
}

// Environment captures terminal-related environment used for deterministic
// protocol selection. DefaultEnvironment reads these fields from os.Environ.
type Environment struct {
	Term          string
	TermProgram   string
	LCTerminal    string
	KittyWindowID string
	ColorTerm     string
	Tmux          string
	Screen        string

	ForcedProtocol string
	Debug          bool
}

// Strategy is the selected rendering adventure for a request/environment pair.
type Strategy struct {
	Protocol Protocol
	Name     string
	Warnings []string
}
