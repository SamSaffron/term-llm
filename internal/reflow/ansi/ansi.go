package ansi

const Marker = '\x1B'

// IsTerminator returns true if the rune is a CSI final byte per ECMA-48 (0x40–0x7E).
func IsTerminator(c rune) bool {
	return c >= 0x40 && c <= 0x7e
}

// Parser tracks ANSI escape sequence state. Use Feed to advance the parser
// one rune at a time. The parser handles CSI (ESC [), OSC (ESC ]), DCS (ESC P),
// PM (ESC ^), APC (ESC _), and SOS (ESC X) sequences.
type Parser struct {
	state parserState
}

type parserState int

const (
	pNormal parserState = iota
	pEsc                // saw ESC
	pCSI                // in CSI sequence (ESC [), terminated by letter
	pOSC                // in OSC/DCS/PM/APC/SOS, terminated by BEL or ST
	pOSCEsc             // in OSC, saw ESC (possible ST = ESC \)
)

// Feed advances the parser with the given rune and reports whether the rune
// is part of an escape sequence (i.e., non-printable).
func (p *Parser) Feed(c rune) bool {
	switch p.state {
	case pNormal:
		if c == Marker {
			p.state = pEsc
			return true
		}
		return false
	case pEsc:
		switch {
		case c == '[':
			p.state = pCSI
		case c == ']', c == 'P', c == '^', c == '_', c == 'X':
			p.state = pOSC
		default:
			p.state = pNormal
		}
		return true
	case pCSI:
		if IsTerminator(c) {
			p.state = pNormal
		}
		return true
	case pOSC:
		if c == '\x07' {
			p.state = pNormal
		} else if c == Marker {
			p.state = pOSCEsc
		}
		return true
	case pOSCEsc:
		if c == '\\' {
			p.state = pNormal
		} else {
			p.state = pOSC
		}
		return true
	}
	return false
}

// InSequence reports whether the parser is currently inside an escape sequence.
func (p *Parser) InSequence() bool {
	return p.state != pNormal
}

// Reset resets the parser to its initial state.
func (p *Parser) Reset() {
	p.state = pNormal
}
