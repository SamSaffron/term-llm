package image

import (
	"fmt"
	"io"
	"strings"

	"github.com/term-llm/term-llm/internal/termimage"
)

// TerminalImageCapability represents the terminal's image rendering capability.
type TerminalImageCapability int

const (
	CapNone  TerminalImageCapability = iota // No image support
	CapKitty                                // Kitty graphics protocol
	CapITerm                                // iTerm2 inline images
	CapSixel                                // Sixel graphics
)

// String returns the capability name.
func (c TerminalImageCapability) String() string {
	switch c {
	case CapKitty:
		return "kitty"
	case CapITerm:
		return "iterm"
	case CapSixel:
		return "sixel"
	default:
		return "none"
	}
}

// DetectCapability detects the terminal's image rendering capability.
//
// This package intentionally delegates terminal protocol detection/rendering to
// internal/termimage so command paths and TUI image artifacts share one Kitty
// implementation. ANSI/text fallback is reported as CapNone here because this
// legacy API is used only for direct terminal image display.
func DetectCapability() TerminalImageCapability {
	strategy := termimage.Select(termimage.Request{
		Mode:               termimage.ModeOneShot,
		Protocol:           termimage.ProtocolAuto,
		AllowEscapeUploads: true,
	}, termimage.DefaultEnvironment())
	return capabilityFromProtocol(strategy.Protocol)
}

func capabilityFromProtocol(protocol termimage.Protocol) TerminalImageCapability {
	switch protocol {
	case termimage.ProtocolKitty:
		return CapKitty
	case termimage.ProtocolITerm:
		return CapITerm
	case termimage.ProtocolSixel:
		return CapSixel
	default:
		return CapNone
	}
}

// RenderImageResult contains the result of rendering an image.
type RenderImageResult struct {
	// Upload contains terminal control bytes that must go directly to terminal.
	Upload string
	// Placeholder contains the displayable portion.
	Placeholder string
	// Full is Upload + Placeholder for one-shot display.
	Full string
}

// RenderImageToString renders an image and returns terminal image bytes for display.
func RenderImageToString(path string) (RenderImageResult, error) {
	result, err := renderViaTermimage(path)
	if err != nil {
		return RenderImageResult{}, err
	}
	if capabilityFromProtocol(result.Protocol) == CapNone {
		return RenderImageResult{}, nil
	}
	return RenderImageResult{
		Upload:      result.Upload,
		Placeholder: result.Display,
		Full:        result.Full,
	}, nil
}

// RenderImageToWriter renders an image to a writer using the detected capability.
func RenderImageToWriter(w io.Writer, path string) error {
	result, err := renderViaTermimage(path)
	if err != nil {
		return err
	}
	if capabilityFromProtocol(result.Protocol) == CapNone || result.Full == "" {
		return nil
	}
	if _, err := io.WriteString(w, result.Full); err != nil {
		return err
	}
	_, err = io.WriteString(w, terminalImageCursorAdvance(result))
	return err
}

func terminalImageCursorAdvance(result termimage.Result) string {
	switch result.Protocol {
	case termimage.ProtocolKitty:
		if kittyUsesUnicodePlaceholders(result) {
			// The placeholder grid is real terminal text and already consumed the
			// image rows. Move from the end of the final placeholder row to the next
			// prompt line only.
			return "\r\n"
		}
		// Kitty direct placements are emitted with C=1 so they do not move the
		// terminal cursor. Reserve the same vertical space explicitly.
		rows := result.HeightCells
		if rows < 1 {
			rows = 1
		}
		return strings.Repeat("\r\n", rows)
	case termimage.ProtocolITerm, termimage.ProtocolSixel:
		return "\r\n"
	default:
		return ""
	}
}

func kittyUsesUnicodePlaceholders(result termimage.Result) bool {
	return result.Protocol == termimage.ProtocolKitty && strings.Contains(result.Place, "U=1")
}

func renderViaTermimage(path string) (termimage.Result, error) {
	strategy := termimage.Select(termimage.Request{
		Mode:               termimage.ModeOneShot,
		Protocol:           termimage.ProtocolAuto,
		AllowEscapeUploads: true,
	}, termimage.DefaultEnvironment())

	mode := termimage.ModeOneShot
	protocol := termimage.ProtocolAuto
	if strategy.Protocol == termimage.ProtocolKitty {
		// For normal console output, use Kitty Unicode placeholders just like the
		// chat TUI viewport path. They are real grid cells, so old images survive
		// later scrollback/redraws. Direct placements can be invalidated when later
		// terminal output scrolls or clears screen cells around them.
		mode = termimage.ModeViewport
		protocol = termimage.ProtocolKitty
	}

	result, err := termimage.Render(termimage.Request{
		Path:               path,
		Mode:               mode,
		Protocol:           protocol,
		MaxCols:            termimage.DefaultMaxCols,
		MaxRows:            termimage.DefaultMaxRows,
		AllowEscapeUploads: true,
	})
	if err != nil {
		return termimage.Result{}, fmt.Errorf("render terminal image: %w", err)
	}
	return result, nil
}
