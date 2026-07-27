package tea

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

type vtTestStyle struct {
	bold, faint, italic                    bool
	underline                              int
	blink, reverse, conceal, strikethrough bool
	foreground, background, underlineColor string
}

type vtTestCell struct {
	content      string
	continuation bool
	style        vtTestStyle
}

type vtTestTerminal struct {
	width, height int
	cursorX       int
	cursorY       int
	marginTop     int
	marginBottom  int
	savedX        int
	savedY        int
	wrapPending   bool
	style         vtTestStyle
	cells         [][]vtTestCell
	lastPrinted   vtTestCell
	marginSets    int
	marginResets  int
}

func newVTTestTerminal(width, height int) *vtTestTerminal {
	v := &vtTestTerminal{width: width, height: height, marginBottom: height - 1}
	v.cells = make([][]vtTestCell, height)
	for y := range v.cells {
		v.cells[y] = make([]vtTestCell, width)
		v.clearLine(y, 0, width-1)
	}
	return v
}

func (v *vtTestTerminal) apply(stream []byte) error {
	for len(stream) > 0 {
		switch stream[0] {
		case 0x1b:
			consumed, err := v.applyEscape(stream)
			if err != nil {
				return err
			}
			stream = stream[consumed:]
		case '\r':
			v.cursorX = 0
			v.wrapPending = false
			stream = stream[1:]
		case '\n', '\v', '\f':
			v.index()
			v.wrapPending = false
			stream = stream[1:]
		case '\b':
			if v.cursorX > 0 {
				v.cursorX--
			}
			v.wrapPending = false
			stream = stream[1:]
		case '\t':
			v.cursorX = min(v.width-1, (v.cursorX/8+1)*8)
			v.wrapPending = false
			stream = stream[1:]
		default:
			if stream[0] < 0x20 || stream[0] == 0x7f {
				stream = stream[1:]
				continue
			}
			r, size := utf8.DecodeRune(stream)
			if r == utf8.RuneError && size == 1 {
				return fmt.Errorf("invalid UTF-8 byte %#x", stream[0])
			}
			v.print(string(stream[:size]))
			stream = stream[size:]
		}
	}
	return nil
}

func (v *vtTestTerminal) applyEscape(stream []byte) (int, error) {
	if len(stream) < 2 {
		return 0, fmt.Errorf("truncated escape")
	}
	switch stream[1] {
	case '[':
		for i := 2; i < len(stream); i++ {
			if stream[i] >= 0x40 && stream[i] <= 0x7e {
				return i + 1, v.applyCSI(string(stream[2:i]), stream[i])
			}
		}
		return 0, fmt.Errorf("truncated CSI %q", stream)
	case ']', 'P', '_', '^':
		for i := 2; i < len(stream); i++ {
			if stream[1] == ']' && stream[i] == '\a' {
				return i + 1, nil
			}
			if stream[i] == 0x1b && i+1 < len(stream) && stream[i+1] == '\\' {
				return i + 2, nil
			}
		}
		return 0, fmt.Errorf("truncated string escape %q", stream)
	case '7':
		v.savedX, v.savedY = v.cursorX, v.cursorY
		return 2, nil
	case '8':
		v.cursorX, v.cursorY = v.savedX, v.savedY
		v.wrapPending = false
		return 2, nil
	case 'D':
		v.index()
		v.wrapPending = false
		return 2, nil
	case 'E':
		v.cursorX = 0
		v.index()
		v.wrapPending = false
		return 2, nil
	case 'M':
		v.reverseIndex()
		v.wrapPending = false
		return 2, nil
	case 'c':
		v.reset()
		return 2, nil
	case '(', ')', '*', '+':
		if len(stream) < 3 {
			return 0, fmt.Errorf("truncated character-set escape")
		}
		return 3, nil
	default:
		return 0, fmt.Errorf("unsupported escape %#x", stream[1])
	}
}

func (v *vtTestTerminal) applyCSI(body string, final byte) error {
	private := byte(0)
	if len(body) > 0 && strings.ContainsRune("?<=>", rune(body[0])) {
		private = body[0]
		body = body[1:]
	}
	for len(body) > 0 && body[len(body)-1] >= 0x20 && body[len(body)-1] <= 0x2f {
		body = body[:len(body)-1]
	}
	params, err := vtTestParams(body)
	if err != nil && final != 'm' {
		return fmt.Errorf("parse CSI %q%c: %w", body, final, err)
	}
	param := func(index, fallback int) int {
		if index >= len(params) || params[index] == 0 {
			return fallback
		}
		return params[index]
	}
	if private != 0 {
		if private == '?' && (final == 'h' || final == 'l') && strings.Contains(body, "1049") && final == 'h' {
			v.reset()
		}
		return nil
	}

	switch final {
	case 'A':
		v.cursorY = max(0, v.cursorY-param(0, 1))
	case 'B':
		v.cursorY = min(v.height-1, v.cursorY+param(0, 1))
	case 'C', 'a':
		v.cursorX = min(v.width-1, v.cursorX+param(0, 1))
	case 'D':
		v.cursorX = max(0, v.cursorX-param(0, 1))
	case 'E':
		v.cursorY = min(v.height-1, v.cursorY+param(0, 1))
		v.cursorX = 0
	case 'F':
		v.cursorY = max(0, v.cursorY-param(0, 1))
		v.cursorX = 0
	case 'G', '`':
		v.cursorX = min(v.width-1, param(0, 1)-1)
	case 'H', 'f':
		v.cursorY = min(v.height-1, param(0, 1)-1)
		v.cursorX = min(v.width-1, param(1, 1)-1)
	case 'd':
		v.cursorY = min(v.height-1, param(0, 1)-1)
	case 'I':
		for range param(0, 1) {
			v.cursorX = min(v.width-1, (v.cursorX/8+1)*8)
		}
	case 'Z':
		for range param(0, 1) {
			if v.cursorX == 0 {
				break
			}
			v.cursorX = ((v.cursorX - 1) / 8) * 8
		}
	case 'J':
		v.eraseDisplay(param(0, 0))
	case 'K':
		v.eraseLine(param(0, 0))
	case 'X':
		v.clearLine(v.cursorY, v.cursorX, min(v.width-1, v.cursorX+param(0, 1)-1))
	case '@':
		v.insertCharacters(param(0, 1))
	case 'P':
		v.deleteCharacters(param(0, 1))
	case 'L':
		v.insertLines(param(0, 1))
	case 'M':
		v.deleteLines(param(0, 1))
	case 'S':
		v.scrollUp(param(0, 1))
	case 'T':
		v.scrollDown(param(0, 1))
	case 'b':
		for range param(0, 1) {
			v.printCell(v.lastPrinted)
		}
	case 'm':
		if err := v.applySGR(body); err != nil {
			return err
		}
	case 'r':
		top, bottom := param(0, 1)-1, param(1, v.height)-1
		if top < 0 || bottom >= v.height || top >= bottom {
			return fmt.Errorf("invalid DECSTBM %d;%d for height %d", top+1, bottom+1, v.height)
		}
		if top == 0 && bottom == v.height-1 {
			if v.marginTop != 0 || v.marginBottom != v.height-1 {
				v.marginResets++
			}
		} else {
			if v.marginTop != 0 || v.marginBottom != v.height-1 {
				return fmt.Errorf("DECSTBM changed before reset: %d;%d", top+1, bottom+1)
			}
			v.marginSets++
		}
		v.marginTop, v.marginBottom = top, bottom
		v.cursorX, v.cursorY = 0, 0
	case 's':
		v.savedX, v.savedY = v.cursorX, v.cursorY
	case 'u':
		v.cursorX, v.cursorY = v.savedX, v.savedY
	case 'c', 'n', 'p', 'q', 't':
		// Capability requests, mode controls, and cursor styling do not alter cells.
	default:
		return fmt.Errorf("unsupported CSI %q%c", body, final)
	}
	v.wrapPending = false
	return nil
}

func vtTestParams(body string) ([]int, error) {
	if body == "" {
		return nil, nil
	}
	parts := strings.Split(body, ";")
	params := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			continue
		}
		if strings.Contains(part, ":") {
			part = strings.Split(part, ":")[0]
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		params[i] = value
	}
	return params, nil
}

func (v *vtTestTerminal) applySGR(body string) error {
	params, err := vtTestParams(body)
	if err != nil {
		return fmt.Errorf("parse SGR %q: %w", body, err)
	}
	if len(params) == 0 {
		params = []int{0}
	}
	for i := 0; i < len(params); i++ {
		p := params[i]
		switch {
		case p == 0:
			v.style = vtTestStyle{}
		case p == 1:
			v.style.bold = true
		case p == 2:
			v.style.faint = true
		case p == 3:
			v.style.italic = true
		case p == 4:
			v.style.underline = 1
		case p == 5 || p == 6:
			v.style.blink = true
		case p == 7:
			v.style.reverse = true
		case p == 8:
			v.style.conceal = true
		case p == 9:
			v.style.strikethrough = true
		case p == 21:
			v.style.underline = 2
		case p == 22:
			v.style.bold, v.style.faint = false, false
		case p == 23:
			v.style.italic = false
		case p == 24:
			v.style.underline = 0
		case p == 25:
			v.style.blink = false
		case p == 27:
			v.style.reverse = false
		case p == 28:
			v.style.conceal = false
		case p == 29:
			v.style.strikethrough = false
		case (p >= 30 && p <= 37) || (p >= 90 && p <= 97):
			v.style.foreground = strconv.Itoa(p)
		case p == 39:
			v.style.foreground = ""
		case (p >= 40 && p <= 47) || (p >= 100 && p <= 107):
			v.style.background = strconv.Itoa(p)
		case p == 49:
			v.style.background = ""
		case p == 38 || p == 48 || p == 58:
			color, consumed, err := vtTestColor(params[i+1:])
			if err != nil {
				return fmt.Errorf("parse SGR color %q: %w", body, err)
			}
			i += consumed
			switch p {
			case 38:
				v.style.foreground = color
			case 48:
				v.style.background = color
			case 58:
				v.style.underlineColor = color
			}
		case p == 59:
			v.style.underlineColor = ""
		}
	}
	return nil
}

func vtTestColor(params []int) (string, int, error) {
	if len(params) >= 2 && params[0] == 5 {
		return fmt.Sprintf("5:%d", params[1]), 2, nil
	}
	if len(params) >= 4 && params[0] == 2 {
		return fmt.Sprintf("2:%d:%d:%d", params[1], params[2], params[3]), 4, nil
	}
	return "", 0, fmt.Errorf("unsupported color parameters %v", params)
}

func (v *vtTestTerminal) print(content string) {
	width := ansi.StringWidth(content)
	if width == 0 {
		x := v.cursorX - 1
		if v.wrapPending {
			x = v.cursorX
		}
		if x >= 0 && x < v.width {
			v.cells[v.cursorY][x].content += content
		}
		return
	}
	v.printCell(vtTestCell{content: content, style: v.style})
}

func (v *vtTestTerminal) printCell(cell vtTestCell) {
	width := ansi.StringWidth(cell.content)
	if width < 1 {
		return
	}
	if width > v.width {
		width = v.width
	}
	if v.wrapPending || v.cursorX+width > v.width {
		v.cursorX = 0
		v.index()
		v.wrapPending = false
	}
	for x := v.cursorX; x < v.cursorX+width && x < v.width; x++ {
		v.clearGlyphAt(v.cursorY, x)
	}
	cell.continuation = false
	v.cells[v.cursorY][v.cursorX] = cell
	for offset := 1; offset < width && v.cursorX+offset < v.width; offset++ {
		v.cells[v.cursorY][v.cursorX+offset] = vtTestCell{continuation: true, style: cell.style}
	}
	v.lastPrinted = cell
	if v.cursorX+width >= v.width {
		v.cursorX = v.width - 1
		v.wrapPending = true
	} else {
		v.cursorX += width
	}
}

func (v *vtTestTerminal) index() {
	if v.cursorY == v.marginBottom {
		v.scrollUp(1)
	} else if v.cursorY < v.height-1 {
		v.cursorY++
	}
}

func (v *vtTestTerminal) reverseIndex() {
	if v.cursorY == v.marginTop {
		v.scrollDown(1)
	} else if v.cursorY > 0 {
		v.cursorY--
	}
}

func (v *vtTestTerminal) scrollUp(amount int) {
	amount = min(max(amount, 1), v.marginBottom-v.marginTop+1)
	for y := v.marginTop; y <= v.marginBottom-amount; y++ {
		copy(v.cells[y], v.cells[y+amount])
	}
	for y := v.marginBottom - amount + 1; y <= v.marginBottom; y++ {
		v.clearLine(y, 0, v.width-1)
	}
}

func (v *vtTestTerminal) scrollDown(amount int) {
	amount = min(max(amount, 1), v.marginBottom-v.marginTop+1)
	for y := v.marginBottom; y >= v.marginTop+amount; y-- {
		copy(v.cells[y], v.cells[y-amount])
	}
	for y := v.marginTop; y < v.marginTop+amount; y++ {
		v.clearLine(y, 0, v.width-1)
	}
}

func (v *vtTestTerminal) insertLines(amount int) {
	if v.cursorY < v.marginTop || v.cursorY > v.marginBottom {
		return
	}
	amount = min(max(amount, 1), v.marginBottom-v.cursorY+1)
	for y := v.marginBottom; y >= v.cursorY+amount; y-- {
		copy(v.cells[y], v.cells[y-amount])
	}
	for y := v.cursorY; y < v.cursorY+amount; y++ {
		v.clearLine(y, 0, v.width-1)
	}
}

func (v *vtTestTerminal) deleteLines(amount int) {
	if v.cursorY < v.marginTop || v.cursorY > v.marginBottom {
		return
	}
	amount = min(max(amount, 1), v.marginBottom-v.cursorY+1)
	for y := v.cursorY; y <= v.marginBottom-amount; y++ {
		copy(v.cells[y], v.cells[y+amount])
	}
	for y := v.marginBottom - amount + 1; y <= v.marginBottom; y++ {
		v.clearLine(y, 0, v.width-1)
	}
}

func (v *vtTestTerminal) insertCharacters(amount int) {
	amount = min(max(amount, 1), v.width-v.cursorX)
	line := v.cells[v.cursorY]
	copy(line[v.cursorX+amount:], line[v.cursorX:v.width-amount])
	v.clearLine(v.cursorY, v.cursorX, v.cursorX+amount-1)
}

func (v *vtTestTerminal) deleteCharacters(amount int) {
	amount = min(max(amount, 1), v.width-v.cursorX)
	line := v.cells[v.cursorY]
	copy(line[v.cursorX:], line[v.cursorX+amount:])
	v.clearLine(v.cursorY, v.width-amount, v.width-1)
}

func (v *vtTestTerminal) eraseDisplay(mode int) {
	switch mode {
	case 0:
		v.clearLine(v.cursorY, v.cursorX, v.width-1)
		for y := v.cursorY + 1; y < v.height; y++ {
			v.clearLine(y, 0, v.width-1)
		}
	case 1:
		for y := 0; y < v.cursorY; y++ {
			v.clearLine(y, 0, v.width-1)
		}
		v.clearLine(v.cursorY, 0, v.cursorX)
	case 2, 3:
		for y := range v.cells {
			v.clearLine(y, 0, v.width-1)
		}
	}
}

func (v *vtTestTerminal) eraseLine(mode int) {
	switch mode {
	case 0:
		v.clearLine(v.cursorY, v.cursorX, v.width-1)
	case 1:
		v.clearLine(v.cursorY, 0, v.cursorX)
	case 2:
		v.clearLine(v.cursorY, 0, v.width-1)
	}
}

func (v *vtTestTerminal) clearGlyphAt(y, x int) {
	if y < 0 || y >= v.height || x < 0 || x >= v.width {
		return
	}
	start := x
	for start > 0 && v.cells[y][start].continuation {
		start--
	}
	width := ansi.StringWidth(v.cells[y][start].content)
	if width < 1 {
		width = 1
	}
	for column := start; column < start+width && column < v.width; column++ {
		v.cells[y][column] = vtTestCell{content: " ", style: v.style}
	}
}

func (v *vtTestTerminal) clearLine(y, start, end int) {
	if y < 0 || y >= v.height || start > end {
		return
	}
	start, end = max(0, start), min(v.width-1, end)
	for x := start; x <= end; x++ {
		v.cells[y][x] = vtTestCell{content: " ", style: v.style}
	}
}

func (v *vtTestTerminal) reset() {
	v.cursorX, v.cursorY = 0, 0
	v.marginTop, v.marginBottom = 0, v.height-1
	v.wrapPending = false
	v.style = vtTestStyle{}
	for y := range v.cells {
		v.clearLine(y, 0, v.width-1)
	}
}

func (v *vtTestTerminal) assertBalancedMargins() error {
	if v.marginTop != 0 || v.marginBottom != v.height-1 {
		return fmt.Errorf("scroll margins remain clamped to %d;%d", v.marginTop+1, v.marginBottom+1)
	}
	if v.marginSets != v.marginResets {
		return fmt.Errorf("DECSTBM sets/resets = %d/%d", v.marginSets, v.marginResets)
	}
	return nil
}

func assertVTTestTerminalsEqual(t *testing.T, incremental, full *vtTestTerminal) {
	t.Helper()
	if !reflect.DeepEqual(incremental.cells, full.cells) {
		for y := range incremental.cells {
			if !reflect.DeepEqual(incremental.cells[y], full.cells[y]) {
				t.Fatalf("interpreted terminal row %d differs\nincremental: %#v\nforced full: %#v", y, incremental.cells[y], full.cells[y])
			}
		}
		t.Fatal("interpreted terminal cells differ")
	}
	incrementalState := []any{incremental.marginTop, incremental.marginBottom, incremental.style}
	fullState := []any{full.marginTop, full.marginBottom, full.style}
	if !reflect.DeepEqual(incrementalState, fullState) {
		t.Fatalf("interpreted terminal state differs\nincremental: %#v\nforced full: %#v", incrementalState, fullState)
	}
}
