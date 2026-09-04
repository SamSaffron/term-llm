package cmd

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	serveShellProtocolOSC       = "7770"
	serveShellProtocolNonceSize = 32
	serveShellCommandBytes      = 64 << 10
	serveShellPhysicalLineBytes = 512
	serveShellCaptureBytes      = 1 << 20
)

type serveShellProtocolMarker struct {
	Kind      byte
	Nonce     string
	Status    int
	HasStatus bool
	Malformed bool
	Start     int64
	End       int64
}

type serveShellProtocolParser struct {
	inOSC       bool
	c1          bool
	escape      bool
	skipCSI     bool
	skipString  bool
	skipEscape  bool
	utf8Remain  int
	startOffset int64
	payload     []byte
}

func (p *serveShellProtocolParser) Feed(offset int64, data []byte) []serveShellProtocolMarker {
	var markers []serveShellProtocolMarker
	for i, b := range data {
		absolute := offset + int64(i)
		// C1 controls occupy the same byte range as UTF-8 continuation bytes.
		// Track valid multibyte text so Unicode output cannot accidentally open a
		// control string and consume a following protocol marker.
		if p.utf8Remain > 0 {
			if b >= 0x80 && b <= 0xbf {
				p.utf8Remain--
				continue
			}
			p.utf8Remain = 0
		}
		switch {
		case b >= 0xc2 && b <= 0xdf:
			p.utf8Remain = 1
			continue
		case b >= 0xe0 && b <= 0xef:
			p.utf8Remain = 2
			continue
		case b >= 0xf0 && b <= 0xf4:
			p.utf8Remain = 3
			continue
		}
		if p.skipCSI {
			if b >= 0x40 && b <= 0x7e {
				p.skipCSI = false
			}
			continue
		}
		if p.skipString {
			if p.skipEscape {
				p.skipEscape = false
				if b == '\\' {
					p.skipString = false
				}
				continue
			}
			if b == 0x07 || b == 0x9c {
				p.skipString = false
			} else if b == 0x1b {
				p.skipEscape = true
			}
			continue
		}
		if !p.inOSC && p.escape {
			p.escape = false
			switch b {
			case ']':
				p.beginOSC(p.startOffset, false)
			case '[':
				p.skipCSI = true
			case 'P', 'X', '^', '_':
				p.skipString = true
			}
			continue
		}
		if !p.inOSC {
			switch b {
			case 0x9d:
				p.beginOSC(absolute, true)
				continue
			case 0x9b:
				p.skipCSI = true
				continue
			case 0x90, 0x98, 0x9e, 0x9f:
				p.skipString = true
				continue
			}
			if b == 0x1b {
				p.escape = true
				p.startOffset = absolute
			}
			continue
		}
		if b == 0x07 || b == 0x9c {
			if marker, ok := p.finishOSC(absolute + 1); ok {
				markers = append(markers, marker)
			}
			continue
		}
		if p.escape {
			p.escape = false
			if len(p.payload) == 0 && b == ']' {
				p.inOSC = true
				p.c1 = false
				continue
			}
			if b == '\\' {
				if marker, ok := p.finishOSC(absolute + 1); ok {
					markers = append(markers, marker)
				}
				continue
			}
			p.payload = append(p.payload, 0x1b, b)
			continue
		}
		if b == 0x1b {
			p.escape = true
			continue
		}
		if len(p.payload) < 512 {
			p.payload = append(p.payload, b)
		} else {
			// Keep consuming to the terminator, but make a protocol-looking
			// oversized OSC unambiguously malformed.
			p.payload = append(p.payload[:512], 0xff)
		}
	}
	return markers
}

func (p *serveShellProtocolParser) beginOSC(offset int64, c1 bool) {
	p.inOSC = true
	p.c1 = c1
	p.escape = false
	p.startOffset = offset
	p.payload = p.payload[:0]
}

func (p *serveShellProtocolParser) finishOSC(end int64) (serveShellProtocolMarker, bool) {
	payload := string(p.payload)
	start := p.startOffset
	p.inOSC = false
	p.escape = false
	p.payload = p.payload[:0]
	parts := strings.Split(payload, ";")
	if len(parts) < 3 || parts[0] != serveShellProtocolOSC || len(parts[1]) != 1 {
		return serveShellProtocolMarker{}, false
	}
	marker := serveShellProtocolMarker{Kind: parts[1][0], Start: start, End: end}
	if marker.Kind != 'P' && marker.Kind != 'B' && marker.Kind != 'E' {
		return serveShellProtocolMarker{}, false
	}
	marker.Nonce = parts[2]
	if !validServeShellNonce(marker.Nonce) {
		marker.Malformed = true
	}
	if marker.Kind == 'E' {
		if len(parts) != 4 {
			marker.Malformed = true
		} else if status, err := strconv.Atoi(parts[3]); err != nil || status < 0 || status > 255 {
			marker.Malformed = true
		} else {
			marker.Status, marker.HasStatus = status, true
		}
	} else if len(parts) != 3 {
		marker.Malformed = true
	}
	return marker, true
}

func newServeShellProtocolNonce() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var random [serveShellProtocolNonceSize]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate shell protocol nonce: %w", err)
	}
	out := make([]byte, len(random))
	for i, value := range random {
		out[i] = alphabet[int(value)%len(alphabet)]
	}
	return string(out), nil
}

func validServeShellNonce(value string) bool {
	if len(value) != serveShellProtocolNonceSize {
		return false
	}
	for i := range value {
		b := value[i]
		if !((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')) {
			return false
		}
	}
	return true
}

func buildServeShellProbe(nonce string) ([]byte, error) {
	if !validServeShellNonce(nonce) {
		return nil, errors.New("invalid shell protocol nonce")
	}
	// A shell with inherited errexit deliberately emits no marker. The eval
	// returns a known non-zero status so enable verifies that foreground eval,
	// status expansion, test, and printf all survive before sharing is enabled.
	return []byte(fmt.Sprintf("case $- in *e*) :;; *) eval ':; (exit 7)'; test \"$(printf '%%d' \"$?\")\" -eq 7 && printf '\\033]7770;P;%%s\\007' '%s';; esac\n", nonce)), nil
}

func buildServeShellCommandPayload(nonce, command string) ([]byte, error) {
	if !validServeShellNonce(nonce) {
		return nil, errors.New("invalid shell protocol nonce")
	}
	if strings.IndexByte(command, 0) >= 0 {
		return nil, errors.New("shared shell command contains NUL")
	}
	if len(command) > serveShellCommandBytes {
		return nil, fmt.Errorf("shared shell command exceeds %d bytes", serveShellCommandBytes)
	}
	chunks := quoteServeShellEvalSource(":;\n" + command)
	var out strings.Builder
	fmt.Fprintf(&out, "printf '\\033]7770;P;%%s\\007' '%s'; printf '\\033]7770;B;%%s\\007' '%s'; PAGER=cat; GIT_PAGER=cat; SYSTEMD_PAGER=cat; MANPAGER=cat; GIT_TERMINAL_PROMPT=0; export PAGER GIT_PAGER SYSTEMD_PAGER MANPAGER GIT_TERMINAL_PROMPT; eval ", nonce, nonce)
	for i, chunk := range chunks {
		if i > 0 {
			out.WriteString("\\\n")
		}
		out.WriteString(chunk)
	}
	fmt.Fprintf(&out, "; printf '\\033]7770;E;%%s;%%d\\007' '%s' \"$?\"\n", nonce)
	payload := out.String()
	for _, line := range strings.Split(payload, "\n") {
		if len(line) > serveShellPhysicalLineBytes {
			return nil, errors.New("shared shell wrapper exceeds canonical line limit")
		}
		if strings.HasPrefix(line, "~") {
			return nil, errors.New("unsafe SSH escape at start of injected line")
		}
	}
	return []byte(payload), nil
}

func quoteServeShellEvalSource(source string) []string {
	const chunkBytes = 48
	chunks := make([]string, 0, (len(source)/chunkBytes)+1)
	for len(source) > 0 {
		n := chunkBytes
		if len(source) < n {
			n = len(source)
		}
		part := source[:n]
		source = source[n:]
		escaped := strings.ReplaceAll(part, "'", "'\"'\"'")
		// A literal newline is valid inside POSIX single quotes, but the next
		// physical PTY line must not begin with user-controlled bytes: OpenSSH
		// recognizes a leading '~' before the remote shell sees it. Close and
		// immediately reopen the quote at every physical line start so each such
		// line begins with a quote while eval reconstructs the exact newline.
		escaped = strings.ReplaceAll(escaped, "\n", "\n''")
		chunks = append(chunks, "'"+escaped+"'")
	}
	if len(chunks) == 0 {
		return []string{"''"}
	}
	return chunks
}

// sanitizeServeShellText strips terminal control protocols and models carriage
// returns as line overwrites, producing bounded plain UTF-8 for model context.
func sanitizeServeShellText(raw []byte, limit int) (string, bool) {
	var plain bytes.Buffer
	state := byte(0) // 0 text, 1 ESC, 2 CSI, 3 OSC, 4 string control, 5 string ESC
	line := make([]byte, 0, 256)
	pendingCR := false
	utf8Remain := 0
	flushLine := func(newline bool) {
		plain.Write(line)
		line = line[:0]
		if newline {
			plain.WriteByte('\n')
		}
	}
	for _, b := range raw {
		if state == 0 && pendingCR {
			pendingCR = false
			if b == '\n' {
				flushLine(true)
				continue
			}
			line = line[:0]
		}
		if state == 0 && utf8Remain > 0 {
			if b >= 0x80 && b <= 0xbf {
				line = append(line, b)
				utf8Remain--
				continue
			}
			utf8Remain = 0
		}
		if state == 0 {
			switch {
			case b >= 0xc2 && b <= 0xdf:
				line = append(line, b)
				utf8Remain = 1
				continue
			case b >= 0xe0 && b <= 0xef:
				line = append(line, b)
				utf8Remain = 2
				continue
			case b >= 0xf0 && b <= 0xf4:
				line = append(line, b)
				utf8Remain = 3
				continue
			}
		}
		switch state {
		case 0:
			switch b {
			case 0x1b:
				state = 1
			case 0x9b:
				state = 2
			case 0x9d:
				state = 3
			case 0x90, 0x98, 0x9e, 0x9f:
				state = 4
			case '\r':
				pendingCR = true
			case '\n':
				flushLine(true)
			case '\t':
				line = append(line, b)
			default:
				if b >= 0x20 && b != 0x7f {
					line = append(line, b)
				}
			}
		case 1:
			switch b {
			case '[':
				state = 2
			case ']':
				state = 3
			case 'P', 'X', '^', '_':
				state = 4
			default:
				state = 0
			}
		case 2:
			if b >= 0x40 && b <= 0x7e {
				state = 0
			}
		case 3, 4:
			if b == 0x07 || b == 0x9c {
				state = 0
			} else if b == 0x1b {
				state = 5
			}
		case 5:
			if b == '\\' {
				state = 0
			} else if b != 0x1b {
				state = 4
			}
		}
	}
	if pendingCR {
		line = line[:0]
	}
	flushLine(false)
	valid := strings.ToValidUTF8(plain.String(), "")
	var cleaned strings.Builder
	cleaned.Grow(len(valid))
	for _, r := range valid {
		if r >= 0x80 && r <= 0x9f {
			continue
		}
		cleaned.WriteRune(r)
	}
	text := cleaned.String()
	truncated := false
	if limit > 0 && len(text) > limit {
		text = text[:limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		truncated = true
	}
	return text, truncated
}
