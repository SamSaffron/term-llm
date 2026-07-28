package tea

import (
	"image/color"

	uv "github.com/charmbracelet/ultraviolet"
)

// backgroundColorMsg requests the terminal background color.
type backgroundColorMsg struct{}

// RequestBackgroundColor requests the terminal background color.
func RequestBackgroundColor() Msg {
	return backgroundColorMsg{}
}

// BackgroundColorMsg is emitted after RequestBackgroundColor.
type BackgroundColorMsg struct{ color.Color }

// String returns the hex representation of the color.
func (e BackgroundColorMsg) String() string {
	return uv.BackgroundColorEvent(e).String()
}

// IsDark reports whether the color is dark.
func (e BackgroundColorMsg) IsDark() bool {
	return uv.BackgroundColorEvent(e).IsDark()
}
