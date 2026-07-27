package tea

import (
	uv "github.com/charmbracelet/ultraviolet"
)

// translateInputEvent translates retained input events into Bubble Tea messages.
func (p *Program) translateInputEvent(e uv.Event) Msg {
	switch e := e.(type) {
	case uv.BackgroundColorEvent:
		return BackgroundColorMsg(e)
	case uv.FocusEvent:
		return FocusMsg(e)
	case uv.BlurEvent:
		return BlurMsg(e)
	case uv.KeyPressEvent:
		return KeyPressMsg(e)
	case uv.MouseClickEvent:
		return MouseClickMsg(e)
	case uv.MouseMotionEvent:
		return MouseMotionMsg(e)
	case uv.MouseReleaseEvent:
		return MouseReleaseMsg(e)
	case uv.MouseWheelEvent:
		return MouseWheelMsg(e)
	case uv.PasteEvent:
		return PasteMsg(e)
	case uv.WindowSizeEvent:
		return WindowSizeMsg(e)
	case uv.ModeReportEvent:
		return ModeReportMsg(e)
	case uv.ClipboardEvent,
		uv.ForegroundColorEvent,
		uv.CursorColorEvent,
		uv.CursorPositionEvent,
		uv.KeyReleaseEvent,
		uv.PasteStartEvent,
		uv.PasteEndEvent,
		uv.CapabilityEvent,
		uv.TerminalVersionEvent,
		uv.KeyboardEnhancementsEvent:
		return nil
	}
	return e
}
