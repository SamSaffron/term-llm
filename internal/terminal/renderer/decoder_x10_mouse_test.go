package uv

import (
	"testing"
)

// x10MousePayload is a well-formed Cb/Cx/Cy triple: button 0 at column 16,
// row 8, using the protocol's 32-byte coordinate offset.
var x10MousePayload = []byte{32, 32 + 16, 32 + 8}

// TestDecodeX10MouseC1FormMatchesEscForm checks that the 8-bit C1 CSI form of
// an X10 mouse report decodes to the same event, and consumes the same number
// of bytes relative to its own length, as the 7-bit ESC [ form. The payload is
// the final three bytes of the sequence in both encodings, so a decoder that
// assumes a fixed three-byte prefix reads past the end of the C1 form.
func TestDecodeX10MouseC1FormMatchesEscForm(t *testing.T) {
	var p EventDecoder

	escSeq := append([]byte{'\x1b', '[', 'M'}, x10MousePayload...)
	escN, escEvent := p.Decode(escSeq)
	if escN != len(escSeq) {
		t.Fatalf("ESC form consumed %d bytes, want %d", escN, len(escSeq))
	}

	c1Seq := append([]byte{0x9b, 'M'}, x10MousePayload...)
	c1N, c1Event := p.Decode(c1Seq)
	if c1N != len(c1Seq) {
		t.Fatalf("C1 form consumed %d bytes, want %d", c1N, len(c1Seq))
	}
	if c1Event != escEvent {
		t.Fatalf("C1 form decoded %#v, want %#v (same as ESC form)", c1Event, escEvent)
	}
}

// TestDecodeX10MouseC1FormAfterEscPrefixes replays the minimized fuzz failure.
// Leading ESC bytes make the decoder recurse before reaching the C1 sequence,
// leaving a five-byte buffer where the fixed-offset payload slice panicked.
func TestDecodeX10MouseC1FormAfterEscPrefixes(t *testing.T) {
	var p EventDecoder
	seq := []byte("\x1b\x1b\x1b\x9bM000")
	n, _ := p.Decode(seq)
	if n <= 0 {
		t.Fatalf("Decode consumed %d bytes, want a positive width", n)
	}
}

// TestDecodeX10MouseTruncatedC1Form checks that a C1 X10 report missing payload
// bytes is reported as an unknown sequence rather than decoded from whatever
// follows it in the buffer.
func TestDecodeX10MouseTruncatedC1Form(t *testing.T) {
	var p EventDecoder
	for _, payload := range [][]byte{nil, {32}, {32, 32}} {
		seq := append([]byte{0x9b, 'M'}, payload...)
		n, event := p.Decode(seq)
		if n <= 0 {
			t.Fatalf("payload %v: Decode consumed %d bytes, want a positive width", payload, n)
		}
		if _, ok := event.(MouseClickEvent); ok {
			t.Fatalf("payload %v: truncated report decoded as a mouse click", payload)
		}
	}
}
