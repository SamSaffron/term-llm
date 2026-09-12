package uv

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image/color"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	xwindows "github.com/charmbracelet/x/windows"
	"github.com/rivo/uniseg"
)

// Flags to control the behavior of the parser.
const (
	// When this flag is set, the driver will treat both Ctrl+Space and Ctrl+@
	// as the same key sequence.
	//
	// Historically, the ANSI specs generate NUL (0x00) on both the Ctrl+Space
	// and Ctrl+@ key sequences. This flag allows the driver to treat both as
	// the same key sequence.
	flagCtrlAt = 1 << iota

	// When this flag is set, the driver will treat the Tab key and Ctrl+I as
	// the same key sequence.
	//
	// Historically, the ANSI specs generate HT (0x09) on both the Tab key and
	// Ctrl+I. This flag allows the driver to treat both as the same key
	// sequence.
	flagCtrlI

	// When this flag is set, the driver will treat the Enter key and Ctrl+M as
	// the same key sequence.
	//
	// Historically, the ANSI specs generate CR (0x0D) on both the Enter key
	// and Ctrl+M. This flag allows the driver to treat both as the same key.
	flagCtrlM

	// When this flag is set, the driver will treat Escape and Ctrl+[ as
	// the same key sequence.
	//
	// Historically, the ANSI specs generate ESC (0x1B) on both the Escape key
	// and Ctrl+[. This flag allows the driver to treat both as the same key
	// sequence.
	flagCtrlOpenBracket

	// When this flag is set, the driver will send a BS (0x08 byte) character
	// instead of a DEL (0x7F byte) character when the Backspace key is
	// pressed.
	//
	// The VT100 terminal has both a Backspace and a Delete key. The VT220
	// terminal dropped the Backspace key and replaced it with the Delete key.
	// Both terminals send a DEL character when the Delete key is pressed.
	// Modern terminals and PCs later readded the Delete key but used a
	// different key sequence, and the Backspace key was standardized to send a
	// DEL character.
	flagBackspace

	// When this flag is set, the driver will recognize the Find key instead of
	// treating it as a Home key.
	//
	// The Find key was part of the VT220 keyboard, and is no longer used in
	// modern day PCs.
	flagFind

	// When this flag is set, the driver will recognize the Select key instead
	// of treating it as a End key.
	//
	// The Symbol key was part of the VT220 keyboard, and is no longer used in
	// modern day PCs.
	flagSelect

	// When this flag is set, the driver will preserve function keys (F13-F63)
	// as symbols.
	//
	// Since these keys are not part of today's standard 20th century keyboard,
	// we treat them as F1-F12 modifier keys i.e. ctrl/shift/alt + Fn combos.
	// Key definitions come from Terminfo, this flag is only useful when
	// FlagTerminfo is not set.
	flagFKeys
)

// LegacyKeyEncoding is a set of flags that control the behavior of legacy
// terminal key encodings. Historically, ctrl+<key> input events produce
// control characters (0x00-0x1F) that collide with some special keys like Tab,
// Enter, and Escape. This controls the expected behavior of encoding these
// keys.
//
// This type has the following default values:
//   - CtrlAt maps 0x00 [ansi.NUL] to ctrl+space instead of ctrl+@.
//   - CtrlI maps 0x09 [ansi.HT] to the tab key instead of ctrl+i.
//   - CtrlM maps 0x0D [ansi.CR] to the enter key instead of ctrl+m.
//   - CtrlOpenBracket maps 0x1B [ansi.ESC] to the escape key instead of ctrl+[.
//   - Backspace maps the backspace key to 0x08 [ansi.BS] instead of 0x7F [ansi.DEL].
//   - Find maps the legacy find key to the home key.
//   - Select maps the legacy select key to the end key.
//   - FKeys maps high function keys instead of treating them as Function+<modifiers>.
type LegacyKeyEncoding uint32

// CtrlAt returns a [LegacyKeyEncoding] with whether [ansi.NUL] (0x00)
// is mapped to ctrl+at instead of ctrl+space.
func (l LegacyKeyEncoding) CtrlAt(v bool) LegacyKeyEncoding {
	if v {
		l |= flagCtrlAt
	} else {
		l &^= flagCtrlAt
	}
	return l
}

// CtrlI returns a [LegacyKeyEncoding] with whether [ansi.HT] (0x09)
// is mapped to ctrl+i instead of the tab key.
func (l LegacyKeyEncoding) CtrlI(v bool) LegacyKeyEncoding {
	if v {
		l |= flagCtrlI
	} else {
		l &^= flagCtrlI
	}
	return l
}

// CtrlM returns a [LegacyKeyEncoding] with whether [ansi.CR] (0x0D)
// is mapped to ctrl+m instead of the enter key.
func (l LegacyKeyEncoding) CtrlM(v bool) LegacyKeyEncoding {
	if v {
		l |= flagCtrlM
	} else {
		l &^= flagCtrlM
	}
	return l
}

// CtrlOpenBracket returns a [LegacyKeyEncoding] with whether [ansi.ESC] (0x1B)
// is mapped to ctrl+[ instead of the escape key.
func (l LegacyKeyEncoding) CtrlOpenBracket(v bool) LegacyKeyEncoding {
	if v {
		l |= flagCtrlOpenBracket
	} else {
		l &^= flagCtrlOpenBracket
	}
	return l
}

// Backspace returns a [LegacyKeyEncoding] with whether the backspace key is
// mapped to [ansi.BS] (0x08) instead of [ansi.DEL] (0x7F).
func (l LegacyKeyEncoding) Backspace(v bool) LegacyKeyEncoding {
	if v {
		l |= flagBackspace
	} else {
		l &^= flagBackspace
	}
	return l
}

// Find returns a [LegacyKeyEncoding] with whether the legacy find key
// is mapped to the home key.
func (l LegacyKeyEncoding) Find(v bool) LegacyKeyEncoding {
	if v {
		l |= flagFind
	} else {
		l &^= flagFind
	}
	return l
}

// Select returns a [LegacyKeyEncoding] with whether the legacy select key
// is mapped to the end key.
func (l LegacyKeyEncoding) Select(v bool) LegacyKeyEncoding {
	if v {
		l |= flagSelect
	} else {
		l &^= flagSelect
	}
	return l
}

// FKeys returns a [LegacyKeyEncoding] with whether high function keys are
// mapped to high function keys (beyond F20) instead of treating them as
// Function+<modifiers> keys.
func (l LegacyKeyEncoding) FKeys(v bool) LegacyKeyEncoding {
	if v {
		l |= flagFKeys
	} else {
		l &^= flagFKeys
	}
	return l
}

// EventDecoder decodes terminal input events from a byte buffer. Terminal
// input events are typically encoded as Unicode or ASCII characters, control
// codes, or ANSI escape sequences.
type EventDecoder struct {
	// Legacy is the legacy key encoding flags. These flags control the
	// behavior of legacy terminal key encodings. See [LegacyKeyEncoding] for
	// more details.
	Legacy LegacyKeyEncoding
	// UseTerminfo is a flag that controls whether to use the terminal type
	// Terminfo database to map escape sequences to key events. This will
	// override the default key sequences handled by the parser.
	UseTerminfo bool

	lastCks uint32 // the last control key state for the previous key event record.
}

// Decode finds the first recognized event sequence and returns it along
// with its length.
//
// It will return zero and nil no sequence is recognized or when the buffer is
// empty. If a sequence is not supported, an [UnknownEvent] is returned.
//
// Example:
//
//	```go
//	var decoder EventDecoder
//	var events []Event
//	buf := []byte("\x00" + // Ctrl+Space
//	    "\x1b[A" + // Up arrow
//	    "Hello") // Text input
//	for len(buf) > 0 {
//	    n, ev := decoder.Decode(buf)
//	    if ev != nil {
//	        events = append(events, ev)
//	    }
//	    buf = buf[n:]
//	}
//	```
func (p *EventDecoder) Decode(buf []byte) (n int, Event Event) {
	if len(buf) == 0 {
		return 0, nil
	}

	switch b := buf[0]; b {
	case ansi.ESC:
		if len(buf) == 1 {
			// Escape key
			return 1, KeyPressEvent{Code: KeyEscape}
		}

		switch bPrime := buf[1]; bPrime {
		case 'O': // Esc-prefixed SS3
			return p.parseSs3(buf)
		case 'P': // Esc-prefixed DCS
			return p.parseDcs(buf)
		case '[': // Esc-prefixed CSI
			return p.parseCsi(buf)
		case ']': // Esc-prefixed OSC
			return p.parseOsc(buf)
		case '_': // Esc-prefixed APC
			return p.parseApc(buf)
		case '^': // Esc-prefixed PM
			return p.parseStTerminated(ansi.PM, '^', nil)(buf)
		case 'X': // Esc-prefixed SOS
			return p.parseStTerminated(ansi.SOS, 'X', nil)(buf)
		default:
			n, e := p.Decode(buf[1:])
			if k, ok := e.(KeyPressEvent); ok {
				k.Text = ""
				k.Mod |= ModAlt
				return n + 1, k
			}

			// Not a key sequence, nor an alt modified key sequence. In that
			// case, just report a single escape key.
			return 1, KeyPressEvent{Code: KeyEscape}
		}
	case ansi.SS3:
		return p.parseSs3(buf)
	case ansi.DCS:
		return p.parseDcs(buf)
	case ansi.CSI:
		return p.parseCsi(buf)
	case ansi.OSC:
		return p.parseOsc(buf)
	case ansi.APC:
		return p.parseApc(buf)
	case ansi.PM:
		return p.parseStTerminated(ansi.PM, '^', nil)(buf)
	case ansi.SOS:
		return p.parseStTerminated(ansi.SOS, 'X', nil)(buf)
	default:
		if b <= ansi.US || b == ansi.DEL || b == ansi.SP {
			return 1, p.parseControl(b)
		} else if b >= ansi.PAD && b <= ansi.APC {
			// C1 control code
			// UTF-8 never starts with a C1 control code
			// Encode these as Ctrl+Alt+<code - 0x40>
			code := rune(b) - 0x40
			return 1, KeyPressEvent{Code: code, Mod: ModCtrl | ModAlt}
		}
		return p.parseUtf8(buf)
	}
}

// scanParams is shared by CSI and DCS parsing. Keep its state transitions aligned
// with the upstream parser when syncing this owned renderer module.
func scanParams(b []byte, i int, params []ansi.Param) (next, count int) {
	var scanned int
	for scanned = 0; i < len(b) && count < len(params) && b[i] >= 0x30 && b[i] <= 0x3F; i, scanned = i+1, scanned+1 {
		if b[i] >= '0' && b[i] <= '9' {
			if params[count] == parser.MissingParam {
				params[count] = 0
			}
			params[count] *= 10
			params[count] += ansi.Param(b[i]) - '0'
		}
		if b[i] == ':' {
			params[count] |= parser.HasMoreFlag
		}
		if b[i] == ';' || b[i] == ':' {
			count++
			if count < len(params) {
				params[count] = parser.MissingParam
			}
		}
	}
	if scanned > 0 && count < len(params) {
		count++
	}
	return i, count
}

func (p *EventDecoder) parseCsi(b []byte) (int, Event) {
	if len(b) == 2 && b[0] == ansi.ESC {
		// short cut if this is an alt+[ key
		return 2, KeyPressEvent{Code: rune(b[1]), Mod: ModAlt}
	}

	var params [parser.MaxParamsSize]ansi.Param
	i, cmd, paramsLen, event, done := p.scanCsiSequence(b, params[:])
	if done {
		return i, event
	}
	pa := ansi.Params(params[:paramsLen])
	raw := b[:i]
	switch cmd {
	case 'y' | '?'<<parser.PrefixShift | '$'<<parser.IntermedShift,
		'c' | '?'<<parser.PrefixShift, 'c' | '>'<<parser.PrefixShift,
		'u' | '?'<<parser.PrefixShift, 'R' | '?'<<parser.PrefixShift,
		'y' | '$'<<parser.IntermedShift:
		return i, parseCsiReport(cmd, pa, raw)
	case 'm' | '<'<<parser.PrefixShift, 'M' | '<'<<parser.PrefixShift,
		'm' | '>'<<parser.PrefixShift, 'n' | '?'<<parser.PrefixShift,
		'I', 'O':
		return i, parseCsiTerminalEvent(cmd, pa, paramsLen, raw)
	case 'R', 'a', 'b', 'c', 'd', 'A', 'B', 'C', 'D', 'E', 'F', 'H', 'P', 'Q', 'S', 'Z':
		return i, parseCsiCursorKey(cmd, pa, paramsLen, raw)
	case 'M':
		if i+3 > len(b) {
			return i, UnknownCsiEvent(raw)
		}
		return i + 3, parseX10MouseEvent(append(raw, b[i:i+3]...))
	case 'u':
		if paramsLen == 0 {
			return i, UnknownCsiEvent(raw)
		}
		return i, parseKittyKeyboard(pa)
	case '_':
		return i, p.parseCsiWin32Input(pa, paramsLen, raw)
	case '@', '^', '~':
		return i, p.parseCsiLegacyKey(cmd, pa, paramsLen, raw)
	case 't':
		return i, parseCsiWindowOperation(pa, paramsLen, raw)
	default:
		return i, UnknownCsiEvent(raw)
	}
}

func (p *EventDecoder) scanCsiSequence(b []byte, params []ansi.Param) (int, ansi.Cmd, int, Event, bool) {
	var cmd ansi.Cmd
	i := 0
	if b[i] == ansi.CSI || b[i] == ansi.ESC {
		i++
	}
	if i < len(b) && b[i-1] == ansi.ESC && b[i] == '[' {
		i++
	}
	if i < len(b) && b[i] >= '<' && b[i] <= '?' {
		cmd |= ansi.Cmd(b[i]) << parser.PrefixShift
	}
	i, paramsLen := scanParams(b, i, params)

	var intermediate byte
	for ; i < len(b) && b[i] >= 0x20 && b[i] <= 0x2F; i++ {
		intermediate = b[i]
	}
	cmd |= ansi.Cmd(intermediate) << parser.IntermedShift
	if i >= len(b) || b[i] < 0x40 || b[i] > 0x7E {
		if intermediate == '$' && b[i-1] == '$' {
			buf := slices.Clone(b[:i-1])
			n, event := p.parseCsi(append(buf, '~'))
			if key, ok := event.(KeyPressEvent); ok {
				key.Mod |= ModShift
				return n, 0, 0, key, true
			}
		}
		return i, 0, 0, UnknownEvent(b[:i]), true
	}
	cmd |= ansi.Cmd(b[i])
	i++
	return i, cmd, paramsLen, nil, false
}

func parseCsiReport(cmd ansi.Cmd, params ansi.Params, raw []byte) Event {
	switch cmd {
	case 'y' | '?'<<parser.PrefixShift | '$'<<parser.IntermedShift:
		return parseCsiModeReport(params, true, raw)
	case 'y' | '$'<<parser.IntermedShift:
		return parseCsiModeReport(params, false, raw)
	case 'c' | '?'<<parser.PrefixShift:
		return parsePrimaryDevAttrs(params)
	case 'c' | '>'<<parser.PrefixShift:
		return parseSecondaryDevAttrs(params)
	case 'u' | '?'<<parser.PrefixShift:
		flags, _, _ := params.Param(0, -1)
		return KeyboardEnhancementsEvent{flags}
	case 'R' | '?'<<parser.PrefixShift:
		row, _, _ := params.Param(0, 1)
		col, _, ok := params.Param(1, 1)
		if ok {
			return CursorPositionEvent{Y: row - 1, X: col - 1}
		}
	}
	return UnknownCsiEvent(raw)
}

func parseCsiModeReport(params ansi.Params, dec bool, raw []byte) Event {
	mode, _, ok := params.Param(0, -1)
	if !ok || mode == -1 {
		return UnknownCsiEvent(raw)
	}
	value, _, ok := params.Param(1, 0)
	if !ok {
		return UnknownCsiEvent(raw)
	}
	if dec {
		return ModeReportEvent{Mode: ansi.DECMode(mode), Value: ansi.ModeSetting(value)}
	}
	return ModeReportEvent{Mode: ansi.ANSIMode(mode), Value: ansi.ModeSetting(value)}
}

func parseCsiTerminalEvent(cmd ansi.Cmd, params ansi.Params, paramsLen int, raw []byte) Event {
	switch cmd {
	case 'm' | '<'<<parser.PrefixShift, 'M' | '<'<<parser.PrefixShift:
		if paramsLen == 3 {
			return parseSGRMouseEvent(cmd, params)
		}
	case 'm' | '>'<<parser.PrefixShift:
		mok, _, mokOK := params.Param(0, 0)
		value, _, valueOK := params.Param(1, -1)
		if mokOK && mok == 4 && valueOK && value != -1 {
			return ModifyOtherKeysEvent{value}
		}
	case 'n' | '?'<<parser.PrefixShift:
		report, _, _ := params.Param(0, -1)
		scheme, _, _ := params.Param(1, -1)
		if report == 997 && scheme == 1 {
			return DarkColorSchemeEvent{}
		}
		if report == 997 && scheme == 2 {
			return LightColorSchemeEvent{}
		}
	case 'I':
		return FocusEvent{}
	case 'O':
		return BlurEvent{}
	}
	return UnknownCsiEvent(raw)
}

func parseCsiCursorKey(cmd ansi.Cmd, params ansi.Params, paramsLen int, raw []byte) Event {
	if cmd == 'R' && paramsLen != 0 {
		if event, ok := parseCsiCursorPositionOrF3(params, paramsLen); ok {
			return event
		}
		return UnknownCsiEvent(raw)
	}
	var key KeyPressEvent
	switch cmd {
	case 'a', 'b', 'c', 'd':
		key = KeyPressEvent{Code: KeyUp + rune(cmd-'a'), Mod: ModShift}
	case 'A', 'B', 'C', 'D':
		key = KeyPressEvent{Code: KeyUp + rune(cmd-'A')}
	case 'E':
		key = KeyPressEvent{Code: KeyBegin}
	case 'F':
		key = KeyPressEvent{Code: KeyEnd}
	case 'H':
		key = KeyPressEvent{Code: KeyHome}
	case 'P', 'Q', 'R', 'S':
		key = KeyPressEvent{Code: KeyF1 + rune(cmd-'P')}
	case 'Z':
		key = KeyPressEvent{Code: KeyTab, Mod: ModShift}
	}
	id, _, _ := params.Param(0, 1)
	mod, _, _ := params.Param(1, 1)
	if paramsLen > 2 && !params[1].HasMore() || id != 1 {
		return UnknownCsiEvent(raw)
	}
	if paramsLen > 1 && mod != -1 {
		key.Mod |= KeyMod(mod - 1)
	}
	return parseKittyKeyboardExt(params, key)
}

func parseCsiCursorPositionOrF3(params ansi.Params, paramsLen int) (Event, bool) {
	row, _, rowOK := params.Param(0, 1)
	col, _, colOK := params.Param(1, 1)
	if paramsLen == 2 && rowOK && colOK {
		position := CursorPositionEvent{Y: row - 1, X: col - 1}
		if row == 1 && col-1 <= int(ModMeta|ModShift|ModAlt|ModCtrl) {
			return MultiEvent{KeyPressEvent{Code: KeyF3, Mod: KeyMod(col - 1)}, position}, true
		}
		return position, true
	}
	return nil, false
}

func (p *EventDecoder) parseCsiWin32Input(params ansi.Params, paramsLen int, raw []byte) Event {
	if paramsLen != 6 {
		return UnknownCsiEvent(raw)
	}
	vk, _, _ := params.Param(0, 0)
	sc, _, _ := params.Param(1, 0)
	uc, _, _ := params.Param(2, 0)
	kd, _, _ := params.Param(3, 0)
	cs, _, _ := params.Param(4, 0)
	rc, _, _ := params.Param(5, 0)
	return p.parseWin32InputKeyEvent(
		uint16(vk),         //nolint:gosec // Vk wVirtualKeyCode
		uint16(sc),         //nolint:gosec // Sc wVirtualScanCode
		rune(uc),           // Uc UnicodeChar
		kd == 1,            // Kd bKeyDown
		uint32(cs),         //nolint:gosec // Cs dwControlKeyState
		max(1, uint16(rc)), //nolint:gosec // Rc wRepeatCount
	)
}

func parseCsiWindowOperation(params ansi.Params, paramsLen int, raw []byte) Event {
	operation, _, ok := params.Param(0, 0)
	if !ok {
		return UnknownCsiEvent(raw)
	}
	if event, ok := parseCsiWindowSize(operation, params, paramsLen); ok {
		return event
	}
	winop := WindowOpEvent{Op: operation}
	for i := 1; i < paramsLen; i++ {
		if value, _, ok := params.Param(i, 0); ok {
			winop.Args = append(winop.Args, value)
		}
	}
	return winop
}

func parseCsiWindowSize(operation int, params ansi.Params, paramsLen int) (Event, bool) {
	if operation == 48 && paramsLen == 5 {
		cellHeight, _, chOK := params.Param(1, 0)
		cellWidth, _, cwOK := params.Param(2, 0)
		pixelHeight, _, phOK := params.Param(3, 0)
		pixelWidth, _, pwOK := params.Param(4, 0)
		if chOK && cwOK && phOK && pwOK {
			return MultiEvent{WindowSizeEvent{Width: cellWidth, Height: cellHeight}, PixelSizeEvent{Width: pixelWidth, Height: pixelHeight}}, true
		}
		return nil, false
	}
	if paramsLen != 3 {
		return nil, false
	}
	height, _, heightOK := params.Param(1, 0)
	width, _, widthOK := params.Param(2, 0)
	if !heightOK || !widthOK {
		return nil, false
	}
	switch operation {
	case 4:
		return PixelSizeEvent{Width: width, Height: height}, true
	case 6:
		return CellSizeEvent{Width: width, Height: height}, true
	case 8:
		return WindowSizeEvent{Width: width, Height: height}, true
	default:
		return nil, false
	}
}

func (p *EventDecoder) parseCsiLegacyKey(cmd ansi.Cmd, pa ansi.Params, paramsLen int, raw []byte) Event {
	if paramsLen == 0 {
		return UnknownCsiEvent(raw)
	}

	param, _, _ := pa.Param(0, 0)
	switch cmd {
	case '~':
		switch param {
		case 27:
			// XTerm modifyOtherKeys 2
			if paramsLen != 3 {
				return UnknownCsiEvent(raw)
			}
			return parseXTermModifyOtherKeys(pa)
		case 200:
			// bracketed-paste start
			return PasteStartEvent{}
		case 201:
			// bracketed-paste end
			return PasteEndEvent{}
		}
	}

	switch param {
	case 1, 2, 3, 4, 5, 6, 7, 8,
		11, 12, 13, 14, 15,
		17, 18, 19, 20, 21,
		23, 24, 25, 26,
		28, 29, 31, 32, 33, 34:
		var k KeyPressEvent
		switch param {
		case 1:
			if p.Legacy&flagFind != 0 {
				k = KeyPressEvent{Code: KeyFind}
			} else {
				k = KeyPressEvent{Code: KeyHome}
			}
		case 2:
			k = KeyPressEvent{Code: KeyInsert}
		case 3:
			k = KeyPressEvent{Code: KeyDelete}
		case 4:
			if p.Legacy&flagSelect != 0 {
				k = KeyPressEvent{Code: KeySelect}
			} else {
				k = KeyPressEvent{Code: KeyEnd}
			}
		case 5:
			k = KeyPressEvent{Code: KeyPgUp}
		case 6:
			k = KeyPressEvent{Code: KeyPgDown}
		case 7:
			k = KeyPressEvent{Code: KeyHome}
		case 8:
			k = KeyPressEvent{Code: KeyEnd}
		case 11, 12, 13, 14, 15:
			k = KeyPressEvent{Code: KeyF1 + rune(param-11)}
		case 17, 18, 19, 20, 21:
			k = KeyPressEvent{Code: KeyF6 + rune(param-17)}
		case 23, 24, 25, 26:
			k = KeyPressEvent{Code: KeyF11 + rune(param-23)}
		case 28, 29:
			k = KeyPressEvent{Code: KeyF15 + rune(param-28)}
		case 31, 32, 33, 34:
			k = KeyPressEvent{Code: KeyF17 + rune(param-31)}
		}

		// modifiers
		mod, _, _ := pa.Param(1, -1)
		if paramsLen > 1 && mod != -1 {
			k.Mod |= KeyMod(mod - 1)
		}

		// Handle URxvt weird keys
		switch cmd {
		case '~':
			// Don't forget to handle Kitty keyboard protocol
			return parseKittyKeyboardExt(pa, k)
		case '^':
			k.Mod |= ModCtrl
		case '@':
			k.Mod |= ModCtrl | ModShift
		}

		return k
	}
	return UnknownCsiEvent(raw)
}

// parseSs3 parses a SS3 sequence.
// See https://vt100.net/docs/vt220-rm/chapter4.html#S4.4.4.2
func (p *EventDecoder) parseSs3(b []byte) (int, Event) {
	if len(b) == 2 && b[0] == ansi.ESC {
		// short cut if this is an alt+O key
		return 2, KeyPressEvent{Code: unicode.ToLower(rune(b[1])), Mod: ModShift | ModAlt}
	}

	var i int
	if b[i] == ansi.SS3 || b[i] == ansi.ESC {
		i++
	}
	if i < len(b) && b[i-1] == ansi.ESC && b[i] == 'O' {
		i++
	}

	// Scan numbers from 0-9
	var mod int
	for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
		mod *= 10
		mod += int(b[i]) - '0'
	}

	// Scan a GL character
	// A GL character is a single byte in the range 0x21-0x7E
	// See https://vt100.net/docs/vt220-rm/chapter2.html#S2.3.2
	if i >= len(b) || b[i] < 0x21 || b[i] > 0x7E {
		return i, UnknownEvent(b[:i])
	}

	// GL character(s)
	gl := b[i]
	i++

	var k KeyPressEvent
	switch gl {
	case 'a', 'b', 'c', 'd':
		k = KeyPressEvent{Code: KeyUp + rune(gl-'a'), Mod: ModCtrl}
	case 'A', 'B', 'C', 'D':
		k = KeyPressEvent{Code: KeyUp + rune(gl-'A')}
	case 'E':
		k = KeyPressEvent{Code: KeyBegin}
	case 'F':
		k = KeyPressEvent{Code: KeyEnd}
	case 'H':
		k = KeyPressEvent{Code: KeyHome}
	case 'P', 'Q', 'R', 'S':
		k = KeyPressEvent{Code: KeyF1 + rune(gl-'P')}
	case 'M':
		k = KeyPressEvent{Code: KeyKpEnter}
	case 'X':
		k = KeyPressEvent{Code: KeyKpEqual}
	case 'j', 'k', 'l', 'm', 'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y':
		k = KeyPressEvent{Code: KeyKpMultiply + rune(gl-'j')}
	default:
		return i, UnknownSs3Event(b[:i])
	}

	// Handle weird SS3 <modifier> Func
	if mod > 0 {
		k.Mod |= KeyMod(mod - 1)
	}

	return i, k
}

func (p *EventDecoder) parseOsc(b []byte) (int, Event) {
	defaultKey := func() KeyPressEvent {
		return KeyPressEvent{Code: rune(b[1]), Mod: ModAlt}
	}
	if len(b) == 2 && b[0] == ansi.ESC {
		// short cut if this is an alt+] key
		return 2, defaultKey()
	}

	var i int
	if b[i] == ansi.OSC || b[i] == ansi.ESC {
		i++
	}
	if i < len(b) && b[i-1] == ansi.ESC && b[i] == ']' {
		i++
	}

	// Parse OSC command
	// An OSC sequence is terminated by a BEL, ESC, or ST character
	var start, end int
	cmd := -1
	for ; i < len(b) && b[i] >= '0' && b[i] <= '9'; i++ {
		if cmd == -1 {
			cmd = 0
		} else {
			cmd *= 10
		}
		cmd += int(b[i]) - '0'
	}

	if i < len(b) && b[i] == ';' {
		// mark the start of the sequence data
		i++
		start = i
	}

	for ; i < len(b); i++ {
		// advance to the end of the sequence
		if slices.Contains([]byte{ansi.BEL, ansi.ESC, ansi.ST, ansi.CAN, ansi.SUB}, b[i]) {
			break
		}
	}

	if i >= len(b) {
		return i, UnknownEvent(b[:i])
	}

	end = i // end of the sequence data
	i++

	// Check 7-bit ST (string terminator) character
	switch b[i-1] {
	case ansi.CAN, ansi.SUB:
		return i, ignoredEvent(b[:i])
	case ansi.ESC:
		if i >= len(b) || b[i] != '\\' {
			// A second disjunct here tested "start == 0 && end == 2", the
			// payload boundary of the two-byte "ESC ]" introducer only. It was
			// also redundant: reaching the terminator at that boundary means no
			// command digits were scanned, which already implies cmd == -1.
			if cmd == -1 {
				if b[0] != ansi.ESC {
					// The 8-bit OSC introducer is a single byte and contains no
					// ESC, so this abandoned sequence has no Alt+<key> reading.
					// Reporting one would invent a key press out of a data byte
					// and consume that byte as part of the introducer.
					// Resynchronize after the introducer alone.
					return 1, UnknownEvent(b[:1])
				}
				return 2, defaultKey()
			}

			// If we don't have a valid ST terminator, then this is a
			// cancelled sequence and should be ignored.
			return i, ignoredEvent(b[:i])
		}

		i++
	}

	if end <= start {
		return i, UnknownEvent(b[:i])
	}

	// start is only set once a ";" separates the command from its payload, and
	// it is then always past the introducer. A zero value means the sequence
	// carried no payload, so the command handlers below must see an empty one:
	// slicing from zero would hand them a "payload" that begins with the
	// introducer bytes, which then decode as a clipboard selection or a color.
	var data string
	if start > 0 {
		data = string(b[start:end])
	}
	switch cmd {
	case 10:
		return i, ForegroundColorEvent{ansi.XParseColor(data)}
	case 11:
		return i, BackgroundColorEvent{ansi.XParseColor(data)}
	case 12:
		return i, CursorColorEvent{ansi.XParseColor(data)}
	case 52:
		parts := strings.Split(data, ";")
		if len(parts) != 2 || len(parts[0]) < 1 {
			return i, ClipboardEvent{}
		}

		b64 := parts[1]
		bts, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return i, ClipboardEvent{Content: parts[1]}
		}

		sel := ClipboardSelection(parts[0][0]) //nolint:unconvert
		return i, ClipboardEvent{Selection: sel, Content: string(bts)}
	}

	return i, UnknownOscEvent(b[:i])
}

// parseStTerminated parses a control sequence that gets terminated by a ST character.
func (p *EventDecoder) parseStTerminated(intro8, intro7 byte, fn func([]byte) Event) func([]byte) (int, Event) {
	defaultKey := func(b []byte) (int, Event) {
		if b[0] != ansi.ESC {
			// The 8-bit introducer is a single byte and contains no ESC, so
			// this abandoned sequence has no Alt+<key> reading. Reporting one
			// would invent a key press out of a data byte and consume that byte
			// as part of the introducer. Resynchronize after the introducer.
			return 1, UnknownEvent(b[:1])
		}
		switch intro8 {
		case ansi.SOS:
			return 2, KeyPressEvent{Code: unicode.ToLower(rune(b[1])), Mod: ModShift | ModAlt}
		case ansi.PM, ansi.APC:
			return 2, KeyPressEvent{Code: rune(b[1]), Mod: ModAlt}
		}
		return 0, nil
	}
	return func(b []byte) (int, Event) {
		if len(b) == 2 && b[0] == ansi.ESC {
			return defaultKey(b)
		}

		var i int
		if b[i] == intro8 || b[i] == ansi.ESC {
			i++
		}
		if i < len(b) && b[i-1] == ansi.ESC && b[i] == intro7 {
			i++
		}

		// Scan control sequence
		// Most common control sequence is terminated by a ST character
		// ST is a 7-bit string terminator character is (ESC \)
		start := i
		for ; i < len(b); i++ {
			if slices.Contains([]byte{ansi.ESC, ansi.ST, ansi.CAN, ansi.SUB}, b[i]) {
				break
			}
		}

		if i >= len(b) {
			return i, UnknownEvent(b[:i])
		}

		end := i // end of the sequence data
		i++

		// Check 7-bit ST (string terminator) character
		switch b[i-1] {
		case ansi.CAN, ansi.SUB:
			return i, ignoredEvent(b[:i])
		case ansi.ESC:
			if i >= len(b) || b[i] != '\\' {
				if start == end {
					return defaultKey(b)
				}

				// If we don't have a valid ST terminator, then this is a
				// cancelled sequence and should be ignored.
				return i, ignoredEvent(b[:i])
			}

			i++
		}

		// Call the function to parse the sequence and return the result
		if fn != nil {
			if e := fn(b[start:end]); e != nil {
				return i, e
			}
		}

		switch intro8 {
		case ansi.PM:
			return i, UnknownPmEvent(b[:i])
		case ansi.SOS:
			return i, UnknownSosEvent(b[:i])
		case ansi.APC:
			return i, UnknownApcEvent(b[:i])
		default:
			return i, UnknownEvent(b[:i])
		}
	}
}

func (p *EventDecoder) parseDcs(b []byte) (int, Event) {
	if len(b) == 2 && b[0] == ansi.ESC {
		// short cut if this is an alt+P key
		return 2, KeyPressEvent{Code: unicode.ToLower(rune(b[1])), Mod: ModShift | ModAlt}
	}

	var params [16]ansi.Param
	var paramsLen int
	var cmd ansi.Cmd

	// DCS sequences are introduced by DCS (0x90) or ESC P (0x1b 0x50)
	var i int
	if b[i] == ansi.DCS || b[i] == ansi.ESC {
		i++
	}
	if i < len(b) && b[i-1] == ansi.ESC && b[i] == 'P' {
		i++
	}

	// initial DCS byte
	if i < len(b) && b[i] >= '<' && b[i] <= '?' {
		cmd |= ansi.Cmd(b[i]) << parser.PrefixShift
	}

	i, paramsLen = scanParams(b, i, params[:])

	// Scan intermediate bytes in the range 0x20-0x2F
	var intermed byte
	for j := 0; i < len(b) && b[i] >= 0x20 && b[i] <= 0x2F; i, j = i+1, j+1 {
		intermed = b[i]
	}

	// set intermediate byte
	cmd |= ansi.Cmd(intermed) << parser.IntermedShift

	// Scan final byte in the range 0x40-0x7E
	if i >= len(b) || b[i] < 0x40 || b[i] > 0x7E {
		return i, UnknownEvent(b[:i])
	}

	// Add the final byte
	cmd |= ansi.Cmd(b[i])
	i++

	start := i // start of the sequence data
	for ; i < len(b); i++ {
		if b[i] == ansi.ST || b[i] == ansi.ESC {
			break
		}
	}

	if i >= len(b) {
		return i, UnknownEvent(b[:i])
	}

	end := i // end of the sequence data
	i++

	// Check 7-bit ST (string terminator) character
	if i < len(b) && b[i-1] == ansi.ESC && b[i] == '\\' {
		i++
	}

	pa := ansi.Params(params[:paramsLen])
	switch cmd {
	case 'r' | '+'<<parser.IntermedShift:
		// XTGETTCAP responses
		param, _, _ := pa.Param(0, 0)
		switch param {
		case 1: // 1 means valid response, 0 means invalid response
			tc := parseTermcap(b[start:end])
			// XXX: some terminals like KiTTY report invalid responses with
			// their queries i.e. sending a query for "Tc" using "\x1bP+q5463\x1b\\"
			// returns "\x1bP0+r5463\x1b\\".
			// The specs says that invalid responses should be in the form of
			// DCS 0 + r ST "\x1bP0+r\x1b\\"
			// We ignore invalid responses and only send valid ones to the program.
			//
			// See: https://invisible-island.net/xterm/ctlseqs/ctlseqs.html#h3-Operating-System-Commands
			return i, tc
		}
	case '|' | '>'<<parser.PrefixShift:
		// XTVersion response
		return i, TerminalVersionEvent{string(b[start:end])}
	case '|' | '!'<<parser.IntermedShift:
		// Teritary Device Attributes
		return i, parseTertiaryDevAttrs(b[start:end])
	}

	return i, UnknownDcsEvent(b[:i])
}

func (p *EventDecoder) parseApc(b []byte) (int, Event) {
	if len(b) == 2 && b[0] == ansi.ESC {
		// short cut if this is an alt+_ key
		return 2, KeyPressEvent{Code: rune(b[1]), Mod: ModAlt}
	}

	// APC sequences are introduced by APC (0x9f) or ESC _ (0x1b 0x5f)
	return p.parseStTerminated(ansi.APC, '_', func(b []byte) Event {
		if len(b) == 0 {
			return nil
		}

		switch b[0] {
		case 'G': // Kitty Graphics Protocol
			var g KittyGraphicsEvent
			parts := bytes.Split(b[1:], []byte{';'})
			g.Options.UnmarshalText(parts[0]) //nolint:errcheck,gosec
			if len(parts) > 1 {
				g.Payload = parts[1]
			}
			return g
		}

		return nil
	})(b)
}

func (p *EventDecoder) parseUtf8(b []byte) (int, Event) {
	if len(b) == 0 {
		return 0, nil
	}

	c := b[0]
	if c <= ansi.US || c == ansi.DEL {
		// Control codes get handled by parseControl
		return 1, p.parseControl(c)
	} else if c > ansi.US && c < ansi.DEL {
		// ASCII printable characters
		code := rune(c)
		k := KeyPressEvent{Code: code, Text: string(code)}
		if unicode.IsUpper(code) {
			// Convert upper case letters to lower case + shift modifier
			k.Code = unicode.ToLower(code)
			k.ShiftedCode = code
			k.Mod |= ModShift
		}

		return 1, k
	}

	code, _ := utf8.DecodeRune(b)
	if code == utf8.RuneError {
		return 1, UnknownEvent(b[0])
	}

	cluster, _, _, _ := uniseg.FirstGraphemeCluster(b, -1)
	text := string(cluster)
	for i := range text {
		if i > 0 {
			// Use [KeyExtended] for multi-rune graphemes
			code = KeyExtended
			break
		}
	}

	return len(cluster), KeyPressEvent{Code: code, Text: text}
}

func (p *EventDecoder) parseControl(b byte) Event {
	switch b {
	case ansi.NUL:
		if p.Legacy&flagCtrlAt != 0 {
			return KeyPressEvent{Code: '@', Mod: ModCtrl}
		}
		return KeyPressEvent{Code: KeySpace, Mod: ModCtrl}
	case ansi.BS:
		return KeyPressEvent{Code: 'h', Mod: ModCtrl}
	case ansi.HT:
		if p.Legacy&flagCtrlI != 0 {
			return KeyPressEvent{Code: 'i', Mod: ModCtrl}
		}
		return KeyPressEvent{Code: KeyTab}
	case ansi.CR:
		if p.Legacy&flagCtrlM != 0 {
			return KeyPressEvent{Code: 'm', Mod: ModCtrl}
		}
		return KeyPressEvent{Code: KeyEnter}
	case ansi.ESC:
		if p.Legacy&flagCtrlOpenBracket != 0 {
			return KeyPressEvent{Code: '[', Mod: ModCtrl}
		}
		return KeyPressEvent{Code: KeyEscape}
	case ansi.DEL:
		if p.Legacy&flagBackspace != 0 {
			return KeyPressEvent{Code: KeyDelete}
		}
		return KeyPressEvent{Code: KeyBackspace}
	case ansi.SP:
		return KeyPressEvent{Code: KeySpace, Text: " "}
	default:
		if b >= ansi.SOH && b <= ansi.SUB {
			// Use lower case letters for control codes
			code := rune(b + 0x60)
			return KeyPressEvent{Code: code, Mod: ModCtrl}
		} else if b >= ansi.FS && b <= ansi.US {
			code := rune(b + 0x40)
			return KeyPressEvent{Code: code, Mod: ModCtrl}
		}
		return UnknownEvent(b)
	}
}

func parseXTermModifyOtherKeys(params ansi.Params) Event {
	// XTerm modify other keys starts with ESC [ 27 ; <modifier> ; <code> ~
	xmod, _, _ := params.Param(1, 1)
	xrune, _, _ := params.Param(2, 1)
	mod := KeyMod(xmod - 1)
	r := rune(xrune)

	switch r {
	case ansi.BS:
		return KeyPressEvent{Mod: mod, Code: KeyBackspace}
	case ansi.HT:
		return KeyPressEvent{Mod: mod, Code: KeyTab}
	case ansi.CR:
		return KeyPressEvent{Mod: mod, Code: KeyEnter}
	case ansi.ESC:
		return KeyPressEvent{Mod: mod, Code: KeyEscape}
	case ansi.DEL:
		return KeyPressEvent{Mod: mod, Code: KeyBackspace}
	}

	// CSI 27 ; <modifier> ; <code> ~ keys defined in XTerm modifyOtherKeys
	k := KeyPressEvent{Code: r, Mod: mod}
	if k.Mod <= ModShift {
		k.Text = string(r)
	}

	return k
}

// Kitty Clipboard Control Sequences.
var kittyKeyMap = map[int]Key{
	ansi.BS:  {Code: KeyBackspace},
	ansi.HT:  {Code: KeyTab},
	ansi.CR:  {Code: KeyEnter},
	ansi.ESC: {Code: KeyEscape},
	ansi.DEL: {Code: KeyBackspace},

	57344: {Code: KeyEscape},
	57345: {Code: KeyEnter},
	57346: {Code: KeyTab},
	57347: {Code: KeyBackspace},
	57348: {Code: KeyInsert},
	57349: {Code: KeyDelete},
	57350: {Code: KeyLeft},
	57351: {Code: KeyRight},
	57352: {Code: KeyUp},
	57353: {Code: KeyDown},
	57354: {Code: KeyPgUp},
	57355: {Code: KeyPgDown},
	57356: {Code: KeyHome},
	57357: {Code: KeyEnd},
	57358: {Code: KeyCapsLock},
	57359: {Code: KeyScrollLock},
	57360: {Code: KeyNumLock},
	57361: {Code: KeyPrintScreen},
	57362: {Code: KeyPause},
	57363: {Code: KeyMenu},
	57364: {Code: KeyF1},
	57365: {Code: KeyF2},
	57366: {Code: KeyF3},
	57367: {Code: KeyF4},
	57368: {Code: KeyF5},
	57369: {Code: KeyF6},
	57370: {Code: KeyF7},
	57371: {Code: KeyF8},
	57372: {Code: KeyF9},
	57373: {Code: KeyF10},
	57374: {Code: KeyF11},
	57375: {Code: KeyF12},
	57376: {Code: KeyF13},
	57377: {Code: KeyF14},
	57378: {Code: KeyF15},
	57379: {Code: KeyF16},
	57380: {Code: KeyF17},
	57381: {Code: KeyF18},
	57382: {Code: KeyF19},
	57383: {Code: KeyF20},
	57384: {Code: KeyF21},
	57385: {Code: KeyF22},
	57386: {Code: KeyF23},
	57387: {Code: KeyF24},
	57388: {Code: KeyF25},
	57389: {Code: KeyF26},
	57390: {Code: KeyF27},
	57391: {Code: KeyF28},
	57392: {Code: KeyF29},
	57393: {Code: KeyF30},
	57394: {Code: KeyF31},
	57395: {Code: KeyF32},
	57396: {Code: KeyF33},
	57397: {Code: KeyF34},
	57398: {Code: KeyF35},
	57399: {Code: KeyKp0},
	57400: {Code: KeyKp1},
	57401: {Code: KeyKp2},
	57402: {Code: KeyKp3},
	57403: {Code: KeyKp4},
	57404: {Code: KeyKp5},
	57405: {Code: KeyKp6},
	57406: {Code: KeyKp7},
	57407: {Code: KeyKp8},
	57408: {Code: KeyKp9},
	57409: {Code: KeyKpDecimal},
	57410: {Code: KeyKpDivide},
	57411: {Code: KeyKpMultiply},
	57412: {Code: KeyKpMinus},
	57413: {Code: KeyKpPlus},
	57414: {Code: KeyKpEnter},
	57415: {Code: KeyKpEqual},
	57416: {Code: KeyKpSep},
	57417: {Code: KeyKpLeft},
	57418: {Code: KeyKpRight},
	57419: {Code: KeyKpUp},
	57420: {Code: KeyKpDown},
	57421: {Code: KeyKpPgUp},
	57422: {Code: KeyKpPgDown},
	57423: {Code: KeyKpHome},
	57424: {Code: KeyKpEnd},
	57425: {Code: KeyKpInsert},
	57426: {Code: KeyKpDelete},
	57427: {Code: KeyKpBegin},
	57428: {Code: KeyMediaPlay},
	57429: {Code: KeyMediaPause},
	57430: {Code: KeyMediaPlayPause},
	57431: {Code: KeyMediaReverse},
	57432: {Code: KeyMediaStop},
	57433: {Code: KeyMediaFastForward},
	57434: {Code: KeyMediaRewind},
	57435: {Code: KeyMediaNext},
	57436: {Code: KeyMediaPrev},
	57437: {Code: KeyMediaRecord},
	57438: {Code: KeyLowerVol},
	57439: {Code: KeyRaiseVol},
	57440: {Code: KeyMute},
	57441: {Code: KeyLeftShift},
	57442: {Code: KeyLeftCtrl},
	57443: {Code: KeyLeftAlt},
	57444: {Code: KeyLeftSuper},
	57445: {Code: KeyLeftHyper},
	57446: {Code: KeyLeftMeta},
	57447: {Code: KeyRightShift},
	57448: {Code: KeyRightCtrl},
	57449: {Code: KeyRightAlt},
	57450: {Code: KeyRightSuper},
	57451: {Code: KeyRightHyper},
	57452: {Code: KeyRightMeta},
	57453: {Code: KeyIsoLevel3Shift},
	57454: {Code: KeyIsoLevel5Shift},
}

func init() {
	// These are some faulty C0 mappings some terminals such as WezTerm have
	// and doesn't follow the specs.
	kittyKeyMap[ansi.NUL] = Key{Code: KeySpace, Mod: ModCtrl}
	for i := ansi.SOH; i <= ansi.SUB; i++ {
		if _, ok := kittyKeyMap[i]; !ok {
			kittyKeyMap[i] = Key{Code: rune(i + 0x60), Mod: ModCtrl}
		}
	}
	for i := ansi.FS; i <= ansi.US; i++ {
		if _, ok := kittyKeyMap[i]; !ok {
			kittyKeyMap[i] = Key{Code: rune(i + 0x40), Mod: ModCtrl}
		}
	}
}

const (
	kittyShift = 1 << iota
	kittyAlt
	kittyCtrl
	kittySuper
	kittyHyper
	kittyMeta
	kittyCapsLock
	kittyNumLock
)

func fromKittyMod(mod int) KeyMod {
	var m KeyMod
	if mod&kittyShift != 0 {
		m |= ModShift
	}
	if mod&kittyAlt != 0 {
		m |= ModAlt
	}
	if mod&kittyCtrl != 0 {
		m |= ModCtrl
	}
	if mod&kittySuper != 0 {
		m |= ModSuper
	}
	if mod&kittyHyper != 0 {
		m |= ModHyper
	}
	if mod&kittyMeta != 0 {
		m |= ModMeta
	}
	if mod&kittyCapsLock != 0 {
		m |= ModCapsLock
	}
	if mod&kittyNumLock != 0 {
		m |= ModNumLock
	}
	return m
}

// parseKittyKeyboard parses a Kitty Keyboard Protocol sequence.
//
// In `CSI u`, this is parsed as:
//
//	CSI codepoint ; modifiers u
//	codepoint: ASCII Dec value
//
// The Kitty Keyboard Protocol extends this with optional components that can be
// enabled progressively. The full sequence is parsed as:
//
//	CSI unicode-key-code:alternate-key-codes ; modifiers:event-type ; text-as-codepoints u
//
// See https://sw.kovidgoyal.net/kitty/keyboard-protocol/
func parseKittyKeyboard(params ansi.Params) (Event Event) {
	var isRelease bool
	var key Key

	// The index of parameters separated by semicolons ';'. Sub parameters are
	// separated by colons ':'.
	var paramIdx int
	var sudIdx int // The sub parameter index
	for _, p := range params {
		// Kitty Keyboard Protocol has 3 optional components.
		switch paramIdx {
		case 0:
			switch sudIdx {
			case 0:
				var foundKey bool
				code := p.Param(1) // CSI u has a default value of 1
				key, foundKey = kittyKeyMap[code]
				if !foundKey {
					r := rune(code)
					if !utf8.ValidRune(r) {
						r = utf8.RuneError
					}

					key.Code = r
				}

			case 2:
				// shifted key + base key
				if b := rune(p.Param(1)); unicode.IsPrint(b) {
					// XXX: When alternate key reporting is enabled, the protocol
					// can return 3 things, the unicode codepoint of the key,
					// the shifted codepoint of the key, and the standard
					// PC-101 key layout codepoint.
					// This is useful to create an unambiguous mapping of keys
					// when using a different language layout.
					key.BaseCode = b
				}
				fallthrough

			case 1:
				// shifted key
				if s := rune(p.Param(1)); unicode.IsPrint(s) {
					// XXX: We swap keys here because we want the shifted key
					// to be the Rune that is returned by the event.
					// For example, shift+a should produce "A" not "a".
					// In such a case, we set AltRune to the original key "a"
					// and Rune to "A".
					key.ShiftedCode = s
				}
			}
		case 1:
			switch sudIdx {
			case 0:
				mod := p.Param(1)
				if mod > 1 {
					key.Mod = fromKittyMod(mod - 1)
					if key.Mod > ModShift {
						// XXX: We need to clear the text if we have a modifier key
						// other than a [ModShift] key.
						key.Text = ""
					}
				}

			case 1:
				switch p.Param(1) {
				case 2:
					key.IsRepeat = true
				case 3:
					isRelease = true
				}
			case 2:
			}
		case 2:
			if code := p.Param(0); code != 0 {
				key.Text += string(rune(code))
			}
		}

		sudIdx++
		if !p.HasMore() {
			paramIdx++
			sudIdx = 0
		}
	}

	keyMod := key.Mod

	// Remove these lock modifiers from now on since they don't affect the text.
	keyMod &^= ModNumLock
	// keyMod &^= ModScrollLock // Kitty doesn't support scroll lock

	printMod := keyMod <= ModShift || keyMod == ModCapsLock || keyMod == (ModShift|ModCapsLock)
	printKeyPad := key.Code >= KeyKpEqual && key.Code <= KeyKpSep
	if len(key.Text) == 0 && printKeyPad && printMod {
		switch {
		case key.Code >= KeyKp0 && key.Code <= KeyKp9:
			key.Text = string('0' + key.Code - KeyKp0)
		case key.Code == KeyKpEqual:
			key.Text = "="
		case key.Code == KeyKpMultiply:
			key.Text = "*"
		case key.Code == KeyKpPlus:
			key.Text = "+"
		case key.Code == KeyKpMinus:
			key.Text = "-"
		case key.Code == KeyKpDecimal:
			key.Text = "."
		case key.Code == KeyKpDivide:
			key.Text = "/"
		case key.Code == KeyKpSep:
			key.Text = ","
		}
	}

	//nolint:nestif
	if len(key.Text) == 0 && unicode.IsPrint(key.Code) && printMod {
		if keyMod == 0 {
			key.Text = string(key.Code)
		} else {
			desiredCase := unicode.ToLower
			if keyMod.Contains(ModShift) || keyMod.Contains(ModCapsLock) {
				desiredCase = unicode.ToUpper
			}
			if key.ShiftedCode != 0 {
				key.Text = string(key.ShiftedCode)
			} else {
				key.Text = string(desiredCase(key.Code))
			}
		}
	}

	if isRelease {
		return KeyReleaseEvent(key)
	}

	return KeyPressEvent(key)
}

// parseKittyKeyboardExt parses a Kitty Keyboard Protocol sequence extensions
// for non CSI u sequences. This includes things like CSI A, SS3 A and others,
// and CSI ~.
func parseKittyKeyboardExt(params ansi.Params, k KeyPressEvent) Event {
	// Handle Kitty keyboard protocol
	if len(params) > 2 && // We have at least 3 parameters
		params[0].Param(1) == 1 && // The first parameter is 1 (defaults to 1)
		params[1].HasMore() { // The second parameter is a subparameter (separated by a ":")
		switch params[2].Param(1) { // The third parameter is the event type (defaults to 1)
		case 2:
			k.IsRepeat = true
		case 3:
			return KeyReleaseEvent(k)
		}
	}
	return k
}

func parsePrimaryDevAttrs(params ansi.Params) Event {
	// Primary Device Attributes
	da1 := make(PrimaryDeviceAttributesEvent, len(params))
	for i, p := range params {
		if !p.HasMore() {
			da1[i] = p.Param(0)
		}
	}
	return da1
}

func parseSecondaryDevAttrs(params ansi.Params) Event {
	// Secondary Device Attributes
	da2 := make(SecondaryDeviceAttributesEvent, len(params))
	for i, p := range params {
		if !p.HasMore() {
			da2[i] = p.Param(0)
		}
	}
	return da2
}

func parseTertiaryDevAttrs(b []byte) Event {
	// Tertiary Device Attributes
	// The response is a 4-digit hexadecimal number.
	bts, err := hex.DecodeString(string(b))
	if err != nil {
		return UnknownDcsEvent(fmt.Sprintf("\x1bP!|%s\x1b\\", b))
	}
	return TertiaryDeviceAttributesEvent(bts)
}

// Parse SGR-encoded mouse events; SGR extended mouse events. SGR mouse events
// look like:
//
//	ESC [ < Cb ; Cx ; Cy (M or m)
//
// where:
//
//	Cb is the encoded button code
//	Cx is the x-coordinate of the mouse
//	Cy is the y-coordinate of the mouse
//	M is for button press, m is for button release
//
// https://invisible-island.net/xterm/ctlseqs/ctlseqs.html#h3-Extended-coordinates
func parseSGRMouseEvent(cmd ansi.Cmd, params ansi.Params) Event {
	x, _, ok := params.Param(1, 1)
	if !ok {
		x = 1
	}
	y, _, ok := params.Param(2, 1)
	if !ok {
		y = 1
	}
	release := cmd.Final() == 'm'
	b, _, _ := params.Param(0, 0)
	mod, btn, _, isMotion := parseMouseButton(b)

	// (1,1) is the upper left. We subtract 1 to normalize it to (0,0).
	x--
	y--

	m := Mouse{X: x, Y: y, Button: btn, Mod: mod}

	// Wheel buttons don't have release events
	// Motion can be reported as a release event in some terminals (Windows Terminal)
	if isWheel(m.Button) {
		return MouseWheelEvent(m)
	} else if !isMotion && release {
		return MouseReleaseEvent(m)
	} else if isMotion {
		return MouseMotionEvent(m)
	}
	return MouseClickEvent(m)
}

const x10MouseByteOffset = 32

// Parse X10-encoded mouse events; the simplest kind. The last release of X10
// was December 1986, by the way. The original X10 mouse protocol limits the Cx
// and Cy coordinates to 223 (=255-032).
//
// X10 mouse events look like:
//
//	ESC [M Cb Cx Cy
//
// See: http://www.xfree86.org/current/ctlseqs.html#Mouse%20Tracking
func parseX10MouseEvent(buf []byte) Event {
	// The Cb/Cx/Cy payload is the final three bytes of the sequence. Its offset
	// is not fixed: the 7-bit introducer "ESC [ M" is three bytes but the 8-bit
	// C1 form "CSI M" is only two, so indexing from the front reads past the end
	// of a C1 report, or decodes the wrong bytes when the backing array happens
	// to be longer.
	if len(buf) < 3 {
		return UnknownCsiEvent(buf)
	}
	v := buf[len(buf)-3:]
	b := int(v[0])
	if b >= x10MouseByteOffset {
		// XXX: b < 32 should be impossible, but we're being defensive.
		b -= x10MouseByteOffset
	}

	mod, btn, isRelease, isMotion := parseMouseButton(b)

	// (1,1) is the upper left. We subtract 1 to normalize it to (0,0).
	x := int(v[1]) - x10MouseByteOffset - 1
	y := int(v[2]) - x10MouseByteOffset - 1

	m := Mouse{X: x, Y: y, Button: btn, Mod: mod}
	if isWheel(m.Button) {
		return MouseWheelEvent(m)
	} else if isMotion {
		return MouseMotionEvent(m)
	} else if isRelease {
		return MouseReleaseEvent(m)
	}
	return MouseClickEvent(m)
}

// See: https://invisible-island.net/xterm/ctlseqs/ctlseqs.html#h3-Extended-coordinates
func parseMouseButton(b int) (mod KeyMod, btn MouseButton, isRelease bool, isMotion bool) {
	// mouse bit shifts
	const (
		bitShift  = 0b0000_0100
		bitAlt    = 0b0000_1000
		bitCtrl   = 0b0001_0000
		bitMotion = 0b0010_0000
		bitWheel  = 0b0100_0000
		bitAdd    = 0b1000_0000 // additional buttons 8-11

		bitsMask = 0b0000_0011
	)

	// Modifiers
	if b&bitAlt != 0 {
		mod |= ModAlt
	}
	if b&bitCtrl != 0 {
		mod |= ModCtrl
	}
	if b&bitShift != 0 {
		mod |= ModShift
	}

	if b&bitAdd != 0 {
		btn = MouseBackward + MouseButton(b&bitsMask)
	} else if b&bitWheel != 0 {
		btn = MouseWheelUp + MouseButton(b&bitsMask)
	} else {
		btn = MouseLeft + MouseButton(b&bitsMask)
		// X10 reports a button release as 0b0000_0011 (3)
		if b&bitsMask == bitsMask {
			btn = MouseNone
			isRelease = true
		}
	}

	// Motion bit doesn't get reported for wheel events.
	if b&bitMotion != 0 && !isWheel(btn) {
		isMotion = true
	}

	return //nolint:nakedret
}

// isWheel returns true if the mouse event is a wheel event.
func isWheel(btn MouseButton) bool {
	return btn >= MouseWheelUp && btn <= MouseWheelRight
}

type shiftable interface {
	~uint | ~uint16 | ~uint32 | ~uint64
}

func shift[T shiftable](x T) T {
	if x > 0xff {
		x >>= 8
	}
	return x
}

func colorToHex(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", shift(r), shift(g), shift(b))
}

func getMaxMin(a, b, c float64) (ma, mi float64) {
	if a > b {
		ma = a
		mi = b
	} else {
		ma = b
		mi = a
	}
	if c > ma {
		ma = c
	} else if c < mi {
		mi = c
	}
	return ma, mi
}

func round(x float64) float64 {
	return math.Round(x*1000) / 1000
}

// rgbToHSL converts an RGB triple to an HSL triple.
func rgbToHSL(r, g, b uint8) (h, s, l float64) {
	// convert uint32 pre-multiplied value to uint8
	// The r,g,b values are divided by 255 to change the range from 0..255 to 0..1:
	Rnot := float64(r) / 255
	Gnot := float64(g) / 255
	Bnot := float64(b) / 255
	Cmax, Cmin := getMaxMin(Rnot, Gnot, Bnot)
	Δ := Cmax - Cmin
	// Lightness calculation:
	l = (Cmax + Cmin) / 2
	// Hue and Saturation Calculation:
	if Δ == 0 {
		h = 0
		s = 0
	} else {
		switch Cmax {
		case Rnot:
			h = 60 * (math.Mod((Gnot-Bnot)/Δ, 6))
		case Gnot:
			h = 60 * (((Bnot - Rnot) / Δ) + 2)
		case Bnot:
			h = 60 * (((Rnot - Gnot) / Δ) + 4)
		}
		if h < 0 {
			h += 360
		}

		s = Δ / (1 - math.Abs((2*l)-1))
	}

	return h, round(s), round(l)
}

// isDarkColor returns whether the given color is dark.
func isDarkColor(c color.Color) bool {
	if c == nil {
		return true
	}

	r, g, b, _ := c.RGBA()
	_, _, l := rgbToHSL(uint8(r>>8), uint8(g>>8), uint8(b>>8)) //nolint:gosec
	return l < 0.5
}

func parseTermcap(data []byte) CapabilityEvent {
	// XTGETTCAP
	if len(data) == 0 {
		return CapabilityEvent{""}
	}

	var tc strings.Builder
	split := bytes.Split(data, []byte{';'})
	for _, s := range split {
		parts := bytes.SplitN(s, []byte{'='}, 2)
		if len(parts) == 0 {
			return CapabilityEvent{""}
		}

		name, err := hex.DecodeString(string(parts[0]))
		if err != nil || len(name) == 0 {
			continue
		}

		var value []byte
		if len(parts) > 1 {
			value, err = hex.DecodeString(string(parts[1]))
			if err != nil {
				continue
			}
		}

		if tc.Len() > 0 {
			tc.WriteByte(';')
		}
		tc.WriteString(string(name))
		if len(value) > 0 {
			tc.WriteByte('=')
			tc.WriteString(string(value))
		}
	}

	return CapabilityEvent{tc.String()}
}

// parseWin32InputKeyEvent converts a Windows Input Record Key Event into a
// KeyPressEvent, KeyReleaseEvent, or MultiEvent including multiple of the
// former type.
// A special case is when the key is either part of VtInputMode which produces
// vkc == 0, or when a key is a UTF-16 surrogate pair.  In both of these cases,
// we need to handle key encoding and properly parse the key event.
func (p *EventDecoder) parseWin32InputKeyEvent(vkc uint16, _ uint16, r rune, keyDown bool, cks uint32, repeatCount uint16) (event Event) {
	defer func() {
		// A coalesced KEY_EVENT_RECORD includes the initial key event. Mark only
		// subsequent key presses as repeats while preserving release events.
		if repeatCount > 1 {
			multi := make(MultiEvent, 0, repeatCount)
			for i := 0; i < int(repeatCount); i++ {
				repeated := event
				if i > 0 {
					if key, ok := repeated.(KeyPressEvent); ok {
						key.IsRepeat = true
						repeated = key
					}
				}
				multi = append(multi, repeated)
			}
			event = multi
		}
	}()
	defer func() {
		if vkc != 0 {
			p.lastCks = cks
		}
	}()

	if vkc == 0 {
		key := Key{Code: 0, BaseCode: r, Mod: translateControlKeyState(cks)}
		if keyDown {
			return KeyPressEvent(key)
		}
		return KeyReleaseEvent(key)
	}

	key := p.win32InputKey(vkc, cks)

	// AltGr is left ctrl + right alt. On non-US keyboards, this is used to type
	// special characters and produce printable events.
	// XXX: Should this be a KeyMod?
	const altGrPressed = xwindows.LEFT_CTRL_PRESSED | xwindows.RIGHT_ALT_PRESSED
	altGr := cks&altGrPressed == altGrPressed

	// Remove these lock keys from the control key state from now on.
	cks &^= xwindows.NUMLOCK_ON
	cks &^= xwindows.SCROLLLOCK_ON
	key.Code = key.BaseCode
	if !unicode.IsControl(r) {
		key.Code = r
		if unicode.IsPrint(key.Code) && (cks == 0 || // no modifiers pressed
			cks == xwindows.SHIFT_PRESSED || // shift pressed
			cks == xwindows.CAPSLOCK_ON || // caps lock on
			cks == (xwindows.SHIFT_PRESSED|xwindows.CAPSLOCK_ON) || // Shift + caps lock pressed
			altGr) { // AltGr pressed
			// If the control key state is 0, shift is pressed, or caps lock
			// then the key event is a printable event i.e. [text] is not empty.
			key.Text = string(key.Code)
		}
	}

	key.Mod = translateControlKeyState(cks)
	key = ensureKeyCase(key, cks)
	if keyDown {
		return KeyPressEvent(key)
	}

	return KeyReleaseEvent(key)
}

func (p *EventDecoder) win32InputKey(vkc uint16, cks uint32) Key {
	if code, ok := p.win32ModifierKeyCode(vkc, cks); ok {
		return Key{BaseCode: code}
	}
	if code, ok := win32EditingKeyCode(vkc); ok {
		return Key{BaseCode: code}
	}
	if code, ok := win32NavigationKeyCode(vkc); ok {
		return Key{BaseCode: code}
	}
	if key, ok := win32PrintableKey(vkc); ok {
		return key
	}
	if key, ok := win32KeypadKey(vkc); ok {
		return key
	}
	if code, ok := win32SystemKeyCode(vkc); ok {
		return Key{BaseCode: code}
	}
	if code, ok := win32OEMKeyCode(vkc); ok {
		return Key{BaseCode: code}
	}
	return Key{}
}

func (p *EventDecoder) win32ModifierKeyCode(vkc uint16, cks uint32) (rune, bool) {
	switch vkc {
	case xwindows.VK_SHIFT:
		state := cks
		if state&xwindows.SHIFT_PRESSED == 0 {
			state = p.lastCks
		}
		if state&xwindows.SHIFT_PRESSED == 0 {
			return 0, true
		}
		if state&xwindows.ENHANCED_KEY != 0 {
			return KeyRightShift, true
		}
		return KeyLeftShift, true
	case xwindows.VK_CONTROL:
		state := cks
		if state&(xwindows.LEFT_CTRL_PRESSED|xwindows.RIGHT_CTRL_PRESSED) == 0 {
			state = p.lastCks
		}
		if state&xwindows.LEFT_CTRL_PRESSED != 0 {
			return KeyLeftCtrl, true
		}
		if state&xwindows.RIGHT_CTRL_PRESSED != 0 {
			return KeyRightCtrl, true
		}
		return 0, true
	case xwindows.VK_MENU:
		state := cks
		if state&(xwindows.LEFT_ALT_PRESSED|xwindows.RIGHT_ALT_PRESSED) == 0 {
			state = p.lastCks
		}
		if state&xwindows.LEFT_ALT_PRESSED != 0 {
			return KeyLeftAlt, true
		}
		if state&xwindows.RIGHT_ALT_PRESSED != 0 {
			return KeyRightAlt, true
		}
		return 0, true
	case xwindows.VK_LSHIFT:
		return KeyLeftShift, true
	case xwindows.VK_RSHIFT:
		return KeyRightShift, true
	case xwindows.VK_LCONTROL:
		return KeyLeftCtrl, true
	case xwindows.VK_RCONTROL:
		return KeyRightCtrl, true
	case xwindows.VK_LMENU:
		return KeyLeftAlt, true
	case xwindows.VK_RMENU:
		return KeyRightAlt, true
	default:
		return 0, false
	}
}

func win32EditingKeyCode(vkc uint16) (rune, bool) {
	switch vkc {
	case xwindows.VK_BACK:
		return KeyBackspace, true
	case xwindows.VK_TAB:
		return KeyTab, true
	case xwindows.VK_RETURN:
		return KeyEnter, true
	case xwindows.VK_PAUSE:
		return KeyPause, true
	case xwindows.VK_CAPITAL:
		return KeyCapsLock, true
	case xwindows.VK_ESCAPE:
		return KeyEscape, true
	case xwindows.VK_SPACE:
		return KeySpace, true
	case xwindows.VK_SELECT:
		return KeySelect, true
	case xwindows.VK_SNAPSHOT:
		return KeyPrintScreen, true
	case xwindows.VK_INSERT:
		return KeyInsert, true
	case xwindows.VK_DELETE:
		return KeyDelete, true
	default:
		return 0, false
	}
}

func win32NavigationKeyCode(vkc uint16) (rune, bool) {
	switch vkc {
	case xwindows.VK_PRIOR:
		return KeyPgUp, true
	case xwindows.VK_NEXT:
		return KeyPgDown, true
	case xwindows.VK_END:
		return KeyEnd, true
	case xwindows.VK_HOME:
		return KeyHome, true
	case xwindows.VK_LEFT:
		return KeyLeft, true
	case xwindows.VK_UP:
		return KeyUp, true
	case xwindows.VK_RIGHT:
		return KeyRight, true
	case xwindows.VK_DOWN:
		return KeyDown, true
	default:
		return 0, false
	}
}

func win32PrintableKey(vkc uint16) (Key, bool) {
	if vkc >= '0' && vkc <= '9' {
		return Key{BaseCode: rune(vkc)}, true
	}
	if vkc >= 'A' && vkc <= 'Z' {
		return Key{BaseCode: rune(vkc) + ('a' - 'A')}, true
	}
	return Key{}, false
}

func win32KeypadKey(vkc uint16) (Key, bool) {
	if vkc >= xwindows.VK_NUMPAD0 && vkc <= xwindows.VK_NUMPAD9 {
		return Key{BaseCode: rune(vkc-xwindows.VK_NUMPAD0) + KeyKp0, Text: string('0' + rune(vkc-xwindows.VK_NUMPAD0))}, true
	}
	switch vkc {
	case xwindows.VK_MULTIPLY:
		return Key{BaseCode: KeyKpMultiply, Text: "*"}, true
	case xwindows.VK_ADD:
		return Key{BaseCode: KeyKpPlus, Text: "+"}, true
	case xwindows.VK_SEPARATOR:
		return Key{BaseCode: KeyKpComma, Text: ","}, true
	case xwindows.VK_SUBTRACT:
		return Key{BaseCode: KeyKpMinus, Text: "-"}, true
	case xwindows.VK_DECIMAL:
		return Key{BaseCode: KeyKpDecimal, Text: "."}, true
	case xwindows.VK_DIVIDE:
		return Key{BaseCode: KeyKpDivide, Text: "/"}, true
	default:
		return Key{}, false
	}
}

func win32SystemKeyCode(vkc uint16) (rune, bool) {
	if vkc >= xwindows.VK_F1 && vkc <= xwindows.VK_F24 {
		return rune(vkc-xwindows.VK_F1) + KeyF1, true
	}
	switch vkc {
	case xwindows.VK_NUMLOCK:
		return KeyNumLock, true
	case xwindows.VK_SCROLL:
		return KeyScrollLock, true
	case xwindows.VK_LWIN:
		return KeyLeftSuper, true
	case xwindows.VK_RWIN:
		return KeyRightSuper, true
	case xwindows.VK_APPS:
		return KeyMenu, true
	case xwindows.VK_VOLUME_MUTE:
		return KeyMute, true
	case xwindows.VK_VOLUME_DOWN:
		return KeyLowerVol, true
	case xwindows.VK_VOLUME_UP:
		return KeyRaiseVol, true
	case xwindows.VK_MEDIA_NEXT_TRACK:
		return KeyMediaNext, true
	case xwindows.VK_MEDIA_PREV_TRACK:
		return KeyMediaPrev, true
	case xwindows.VK_MEDIA_STOP:
		return KeyMediaStop, true
	case xwindows.VK_MEDIA_PLAY_PAUSE:
		return KeyMediaPlayPause, true
	default:
		return 0, false
	}
}

func win32OEMKeyCode(vkc uint16) (rune, bool) {
	switch vkc {
	case xwindows.VK_OEM_1:
		return ';', true
	case xwindows.VK_OEM_PLUS:
		return '+', true
	case xwindows.VK_OEM_COMMA:
		return ',', true
	case xwindows.VK_OEM_MINUS:
		return '-', true
	case xwindows.VK_OEM_PERIOD:
		return '.', true
	case xwindows.VK_OEM_2:
		return '/', true
	case xwindows.VK_OEM_3:
		return '`', true
	case xwindows.VK_OEM_4:
		return '[', true
	case xwindows.VK_OEM_5:
		return '\\', true
	case xwindows.VK_OEM_6:
		return ']', true
	case xwindows.VK_OEM_7:
		return '\'', true
	default:
		return 0, false
	}
}

// ensureKeyCase ensures that the key's text is in the correct case based on the
// control key state.
func ensureKeyCase(key Key, cks uint32) Key {
	if len(key.Text) == 0 {
		return key
	}

	hasShift := cks&xwindows.SHIFT_PRESSED != 0
	hasCaps := cks&xwindows.CAPSLOCK_ON != 0
	if hasShift != hasCaps {
		if unicode.IsLower(key.Code) {
			key.ShiftedCode = unicode.ToUpper(key.Code)
			key.Text = string(key.ShiftedCode)
		}
	} else {
		if unicode.IsUpper(key.Code) {
			key.ShiftedCode = unicode.ToLower(key.Code)
			key.Text = string(key.ShiftedCode)
		}
	}

	return key
}

// translateControlKeyState translates the control key state from the Windows
// Console API into a Mod bitmask.
func translateControlKeyState(cks uint32) (m KeyMod) {
	if cks&xwindows.LEFT_CTRL_PRESSED != 0 || cks&xwindows.RIGHT_CTRL_PRESSED != 0 {
		m |= ModCtrl
	}
	if cks&xwindows.LEFT_ALT_PRESSED != 0 || cks&xwindows.RIGHT_ALT_PRESSED != 0 {
		m |= ModAlt
	}
	if cks&xwindows.SHIFT_PRESSED != 0 {
		m |= ModShift
	}
	if cks&xwindows.CAPSLOCK_ON != 0 {
		m |= ModCapsLock
	}
	if cks&xwindows.NUMLOCK_ON != 0 {
		m |= ModNumLock
	}
	if cks&xwindows.SCROLLLOCK_ON != 0 {
		m |= ModScrollLock
	}
	return
}
