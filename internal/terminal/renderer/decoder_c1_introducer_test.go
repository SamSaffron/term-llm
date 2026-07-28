package uv

import "testing"

// c1StringIntroducers are the string-terminated sequences that exist in both a
// 7-bit "ESC x" form and a single-byte 8-bit form.
var c1StringIntroducers = []struct {
	name    string
	esc     string
	c1      byte
	altCode rune
	altMod  KeyMod
}{
	{name: "OSC", esc: "\x1b]", c1: 0x9d, altCode: ']', altMod: ModAlt},
	{name: "APC", esc: "\x1b_", c1: 0x9f, altCode: '_', altMod: ModAlt},
	{name: "PM", esc: "\x1b^", c1: 0x9e, altCode: '^', altMod: ModAlt},
	{name: "SOS", esc: "\x1bX", c1: 0x98, altCode: 'x', altMod: ModShift | ModAlt},
}

var c1UnterminatedBodies = []string{"\x1b", "hi\x1b", "abc\x1bZ", "1;2\x1b", "52;x\x1b"}

// A sequence introduced by the 7-bit "ESC x" pair is also a valid Alt+x key
// press, so an abandoned sequence may resynchronize by consuming both
// introducer bytes and reporting that key. The 8-bit introducer is a single
// byte and contains no ESC, so it has no such reading: reporting one invents a
// key press out of a data byte and consumes that byte along with it.
func TestDecodeUnterminatedC1SequenceDoesNotFabricateAltKey(t *testing.T) {
	for _, intro := range c1StringIntroducers {
		t.Run(intro.name, func(t *testing.T) {
			for _, body := range c1UnterminatedBodies {
				var c1Decoder, escDecoder EventDecoder
				c1Seq := append([]byte{intro.c1}, body...)
				c1N, c1Event := c1Decoder.Decode(c1Seq)
				escN, _ := escDecoder.Decode([]byte(intro.esc + body))

				if k, ok := c1Event.(KeyPressEvent); ok && k.Mod&ModAlt != 0 {
					t.Errorf("Decode(%q) = %d, %#v; an 8-bit introducer carries no ESC and cannot produce an alt-modified key", c1Seq, c1N, k)
				}
				if c1N != escN-1 {
					t.Errorf("Decode(%q) consumed %d bytes and the ESC form consumed %d; both encodings must consume the same body", c1Seq, c1N, escN)
				}
			}
		})
	}
}

// The bytes following an abandoned 8-bit introducer are ordinary input and must
// still be delivered rather than swallowed by the failed sequence.
func TestDecodeUnterminatedC1SequencePreservesFollowingInput(t *testing.T) {
	for _, intro := range c1StringIntroducers {
		t.Run(intro.name, func(t *testing.T) {
			buf := []byte{intro.c1, 0x1b}
			var p EventDecoder
			n, _ := p.Decode(buf)
			if n != 1 {
				t.Fatalf("Decode(%q) consumed %d bytes, want only the 1-byte C1 introducer", buf, n)
			}
			w, ev := p.Decode(buf[n:])
			k, ok := ev.(KeyPressEvent)
			if !ok || k.Code != KeyEscape || w != 1 {
				t.Fatalf("trailing ESC decoded as %d, %T %v; want the Escape key", w, ev, ev)
			}
		})
	}
}

// The 7-bit alt-key resynchronization is long-standing behavior and must be
// preserved: "ESC ]" really is Alt+].
func TestDecodeUnterminatedEscSequenceKeepsAltShortcut(t *testing.T) {
	for _, intro := range c1StringIntroducers {
		t.Run(intro.name, func(t *testing.T) {
			for _, body := range []string{"\x1b", ""} {
				seq := intro.esc + body
				var p EventDecoder
				n, ev := p.Decode([]byte(seq))
				k, ok := ev.(KeyPressEvent)
				if !ok || k.Code != intro.altCode || k.Mod != intro.altMod {
					t.Errorf("Decode(%q) = %d, %T %v; want code %q mod %v", seq, n, ev, ev, intro.altCode, intro.altMod)
				}
				if n != 2 {
					t.Errorf("Decode(%q) consumed %d bytes, want 2", seq, n)
				}
			}
		})
	}
}
