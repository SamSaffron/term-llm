package uv

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

// decoderFuzzSeeds are inputs that exercise every introducer form the decoder
// dispatches on, in both their 7-bit ESC-prefixed and 8-bit C1 encodings.
var decoderFuzzSeeds = []string{
	"",
	"\x1b",
	"hello",
	"\x1b[A",
	"\x1b[1;5A",
	"\x1b[M !!",
	"\x9bM !!",
	"\x1b\x1b\x1b\x9bM000",
	"\x1b[<0;1;1M",
	"\x1b[200~pasted\x1b[201~",
	"\x1bOP",
	"\x8fP",
	"\x1b]52;?\x07",
	"\x9d52;?\x07",
	"\x1bP>|term\x1b\\",
	"\x90>|term\x1b\\",
	"\x1b_Gi=123\x1b\\",
	"\x9fGi=123\x1b\\",
	"\x1b^priv\x1b\\",
	"\x1bXsos\x1b\\",
	"\x00\x7f\x20",
	"界 ﾞ",
	"\xff\xfe\xfd",
}

// FuzzDecodeConsumesInputWithinBounds drives the loop that [EventDecoder.Decode]
// documents for callers: decode, advance by the reported width, repeat. Two
// properties make that loop safe, and neither is checked by decoding only the
// first event of a buffer:
//
//   - the width is positive, so the loop always terminates;
//   - the width never exceeds the remaining input, so the caller's reslice
//     cannot panic and no event can be built from bytes that do not exist.
func FuzzDecodeConsumesInputWithinBounds(f *testing.F) {
	for _, seed := range decoderFuzzSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seq string) {
		buf := []byte(seq)
		var p EventDecoder
		for i := 0; i < len(buf); {
			rest := buf[i:]
			n, ev := p.Decode(rest)
			if n <= 0 {
				t.Fatalf("Decode(%q) at offset %d returned width %d, which cannot advance the caller's loop", rest, i, n)
			}
			if n > len(rest) {
				t.Fatalf("Decode(%q) at offset %d consumed %d of %d available bytes (event %T %v)", rest, i, n, len(rest), ev, ev)
			}
			i += n
		}
	})
}

// FuzzDecodeIgnoresBytesBeyondLength proves the decoder's result depends only
// on the bytes it was given. Terminal input arrives in a reused read buffer, so
// the bytes just past a sequence are whatever the previous read left there. A
// decoder that indexes from a fixed offset instead of the slice length reads
// them, which either panics or, when the backing array happens to be long
// enough, silently decodes an event from stale bytes.
func FuzzDecodeIgnoresBytesBeyondLength(f *testing.F) {
	for _, seed := range decoderFuzzSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seq string) {
		// Identical contents, different bytes beyond len: an exact-capacity
		// copy (where an over-read panics) and two padded copies (where an
		// over-read is silent but observable as a difference).
		withTrailer := func(filler byte) []byte {
			backing := make([]byte, len(seq)+8)
			for i := range backing {
				backing[i] = filler
			}
			copy(backing, seq)
			return backing[:len(seq)]
		}
		inputs := []struct {
			name string
			buf  []byte
		}{
			{name: "exact capacity", buf: append([]byte(nil), seq...)},
			{name: "0x00 trailer", buf: withTrailer(0x00)},
			{name: "0xff trailer", buf: withTrailer(0xff)},
		}

		type result struct {
			n  int
			ev Event
		}
		results := make([]result, len(inputs))
		for i, in := range inputs {
			var p EventDecoder
			n, ev := p.Decode(in.buf)
			results[i] = result{n: n, ev: ev}
		}
		for i := 1; i < len(results); i++ {
			if results[i].n != results[0].n || !reflect.DeepEqual(results[i].ev, results[0].ev) {
				t.Fatalf("Decode(%q) depends on bytes past the input:\n %s: %d, %T %v\n %s: %d, %T %v",
					seq,
					inputs[0].name, results[0].n, results[0].ev, results[0].ev,
					inputs[i].name, results[i].n, results[i].ev, results[i].ev)
			}
		}
	})
}

// c1IntroducerForms pairs each 7-bit ESC-prefixed introducer with the 8-bit C1
// control that encodes the same thing in a single byte. Terminals in 8-bit mode
// send the C1 form, so both must decode to the same event.
var c1IntroducerForms = []struct {
	name string
	esc  string
	c1   byte
}{
	{name: "CSI", esc: "\x1b[", c1: 0x9b},
	{name: "OSC", esc: "\x1b]", c1: 0x9d},
	{name: "DCS", esc: "\x1bP", c1: 0x90},
	{name: "SS3", esc: "\x1bO", c1: 0x8f},
	{name: "APC", esc: "\x1b_", c1: 0x9f},
	{name: "PM", esc: "\x1b^", c1: 0x9e},
	{name: "SOS", esc: "\x1bX", c1: 0x98},
}

// FuzzDecodeC1IntroducerMatchesEscForm checks that the introducer encoding does
// not change the decoded event. This generalizes the X10 mouse defect, where
// the payload was read at an offset valid only for the three-byte ESC form and
// so the two-byte C1 form decoded different coordinates.
func FuzzDecodeC1IntroducerMatchesEscForm(f *testing.F) {
	bodies := []string{
		"A", "M !!", "M000", "1;5A", "<0;1;1M", "200~", "P", "52;?\x07",
		">|term\x1b\\", "Gi=123\x1b\\", "0", "?1049h", "\x1b\\",
	}
	for form := range c1IntroducerForms {
		for _, body := range bodies {
			f.Add(uint8(form), body)
		}
	}
	f.Fuzz(func(t *testing.T, form uint8, body string) {
		if body == "" {
			// An empty body is not a shared sequence: the two-byte ESC form is
			// the documented Alt+<char> shortcut, while the C1 form is a lone
			// control byte.
			return
		}
		intro := c1IntroducerForms[int(form)%len(c1IntroducerForms)]

		var escDecoder, c1Decoder EventDecoder
		escN, escEvent := escDecoder.Decode([]byte(intro.esc + body))
		c1N, c1Event := c1Decoder.Decode(append([]byte{intro.c1}, body...))

		// Whatever the two forms decide, they must consume the same amount of
		// body. Anything else means one encoding ate a byte the other treated
		// as input.
		if escN != c1N+1 {
			t.Fatalf("%s body %q: ESC form consumed %d bytes and C1 form consumed %d, want a difference of exactly one introducer byte",
				intro.name, body, escN, c1N)
		}

		// The 7-bit introducer doubles as Alt+<char>, so an abandoned sequence
		// may legitimately resynchronize to that key press. The 8-bit form has
		// no such reading, and must never fabricate one from a body byte.
		if isIntroducerAltResync(c1Event, c1N, string([]byte{intro.c1})+body) {
			t.Fatalf("%s body %q: C1 form decoded to %#v, but an 8-bit introducer carries no ESC and cannot yield an alt-modified key",
				intro.name, body, c1Event)
		}
		if isIntroducerAltResync(escEvent, escN, intro.esc+body) {
			return
		}

		escKey := introducerNormalizedEvent(escEvent, intro.esc, intro.c1)
		c1Key := introducerNormalizedEvent(c1Event, intro.esc, intro.c1)
		if escKey != c1Key {
			t.Fatalf("%s body %q decoded differently:\n ESC: %s\n C1:  %s", intro.name, body, escKey, c1Key)
		}
	})
}

// isIntroducerAltResync reports whether an abandoned sequence was reported as
// Alt+<second byte>. That is the decoder's give-up path for a 7-bit introducer,
// where "ESC x" really is Alt+x. Recognizing it by shape rather than by
// introducer table also catches the 8-bit form fabricating the same event from
// a data byte, which is never correct.
func isIntroducerAltResync(ev Event, n int, seq string) bool {
	k, ok := ev.(KeyPressEvent)
	if !ok || n != 2 || len(seq) < 2 || k.Mod&ModAlt == 0 {
		return false
	}
	second := rune(seq[1])
	return k.Code == second || k.Code == unicode.ToLower(second)
}

// introducerNormalizedEvent renders an event for comparison across introducer
// encodings. Unrecognized and ignored sequences are reported verbatim, so their
// payloads legitimately differ by the introducer itself; every other event must
// match exactly.
func introducerNormalizedEvent(ev Event, esc string, c1 byte) string {
	trim := func(s string) string {
		if rest, ok := strings.CutPrefix(s, esc); ok {
			return rest
		}
		if len(s) > 0 && s[0] == c1 {
			return s[1:]
		}
		return s
	}
	switch e := ev.(type) {
	case ignoredEvent:
		return "ignoredEvent:" + trim(string(e))
	case UnknownEvent:
		return "UnknownEvent:" + trim(string(e))
	case UnknownCsiEvent:
		return "UnknownCsiEvent:" + trim(string(e))
	case UnknownSs3Event:
		return "UnknownSs3Event:" + trim(string(e))
	case UnknownOscEvent:
		return "UnknownOscEvent:" + trim(string(e))
	case UnknownDcsEvent:
		return "UnknownDcsEvent:" + trim(string(e))
	case UnknownSosEvent:
		return "UnknownSosEvent:" + trim(string(e))
	case UnknownPmEvent:
		return "UnknownPmEvent:" + trim(string(e))
	case UnknownApcEvent:
		return "UnknownApcEvent:" + trim(string(e))
	}
	return fmt.Sprintf("%T:%#v", ev, ev)
}
