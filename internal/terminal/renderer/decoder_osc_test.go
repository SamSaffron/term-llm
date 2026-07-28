package uv

import (
	"fmt"
	"testing"
)

// An OSC payload begins after the ";" that separates it from the command
// number. When no separator is present there is no payload, and the parser must
// not fall back to slicing from offset zero — that range starts at the
// introducer, so the command handlers would parse the introducer bytes as a
// clipboard selection or as a color.
func TestDecodeOscWithoutSeparatorDoesNotParseIntroducerAsPayload(t *testing.T) {
	for _, body := range []string{"52A;", "52", "10A", "11rgb:00/00/00", "12"} {
		for _, terminator := range []string{"\x07", "\x1b\\"} {
			escSeq := "\x1b]" + body + terminator
			c1Seq := "\x9d" + body + terminator

			var escDecoder, c1Decoder EventDecoder
			escN, escEvent := escDecoder.Decode([]byte(escSeq))
			c1N, c1Event := c1Decoder.Decode([]byte(c1Seq))

			if clip, ok := escEvent.(ClipboardEvent); ok && clip.Selection == 0x1b {
				t.Errorf("Decode(%q) = %#v; the ESC introducer was parsed as a clipboard selection", escSeq, clip)
			}
			if clip, ok := c1Event.(ClipboardEvent); ok && clip.Selection == 0x9d {
				t.Errorf("Decode(%q) = %#v; the C1 introducer was parsed as a clipboard selection", c1Seq, clip)
			}
			if escN != c1N+1 {
				t.Errorf("Decode(%q) consumed %d bytes and Decode(%q) consumed %d; want a difference of one introducer byte", escSeq, escN, c1Seq, c1N)
			}
			if escEvent != c1Event {
				t.Errorf("introducer changed the decoded event for body %q:\n ESC: %#v\n C1:  %#v", body, escEvent, c1Event)
			}
		}
	}
}

// An OSC abandoned by a bare ESC must resolve the same way regardless of how it
// was introduced. The give-up path used to test the payload boundary against
// the literal offset 2, which is where a payload starts only after the two-byte
// "ESC ]" introducer.
func TestDecodeUnterminatedOscIsIndependentOfIntroducerLength(t *testing.T) {
	for _, body := range []string{"0\x1b", "\x1b", "52\x1b", "0;x\x1b", "9;n\x1bZ"} {
		escSeq := "\x1b]" + body
		c1Seq := "\x9d" + body

		var escDecoder, c1Decoder EventDecoder
		escN, escEvent := escDecoder.Decode([]byte(escSeq))
		c1N, c1Event := c1Decoder.Decode([]byte(c1Seq))

		if escN != c1N+1 {
			t.Errorf("Decode(%q) consumed %d bytes and Decode(%q) consumed %d; want a difference of one introducer byte", escSeq, escN, c1Seq, c1N)
		}
		if _, ok := escEvent.(KeyPressEvent); ok {
			// The 7-bit form may legitimately resynchronize to Alt+]; the C1
			// form is covered by the introducer tests.
			continue
		}
		if escType, c1Type := eventTypeName(escEvent), eventTypeName(c1Event); escType != c1Type {
			t.Errorf("introducer changed the decoded event kind for body %q: ESC %s, C1 %s", body, escType, c1Type)
		}
	}
}

func eventTypeName(ev Event) string {
	return fmt.Sprintf("%T", ev)
}

// A well-formed OSC still parses its payload.
func TestDecodeOscWithSeparatorKeepsPayload(t *testing.T) {
	for _, seq := range []string{"\x1b]52;c;aGk=\x07", "\x9d52;c;aGk=\x07"} {
		var p EventDecoder
		n, ev := p.Decode([]byte(seq))
		clip, ok := ev.(ClipboardEvent)
		if !ok {
			t.Fatalf("Decode(%q) = %d, %T %v; want a clipboard event", seq, n, ev, ev)
		}
		if clip.Selection != SystemClipboard || clip.Content != "hi" {
			t.Fatalf("Decode(%q) = %#v; want the system selection carrying %q", seq, clip, "hi")
		}
	}
}
