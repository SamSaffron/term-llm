package protocol

import (
	"strings"
	"testing"
)

func TestParserFragmentedMarker(t *testing.T) {
	t.Parallel()
	nonce := strings.Repeat("A", NonceSize)
	var parser Parser
	if got := parser.Feed(0, []byte("hello\x1b]7770;P;")); len(got) != 0 {
		t.Fatalf("incomplete marker: %+v", got)
	}
	got := parser.Feed(14, []byte(nonce+"\x07"))
	if len(got) != 1 || got[0].Kind != 'P' || got[0].Nonce != nonce {
		t.Fatalf("marker: %+v", got)
	}
}

func TestPayloadValidation(t *testing.T) {
	t.Parallel()
	nonce := strings.Repeat("B", NonceSize)
	for _, tc := range []struct{ nonce, command string }{
		{"bad", "echo ok"},
		{nonce, "bad\x00command"},
		{nonce, strings.Repeat("x", CommandBytes+1)},
	} {
		if _, err := BuildCommandPayload(tc.nonce, tc.command); err == nil {
			t.Errorf("accepted nonce %q and command length %d", tc.nonce, len(tc.command))
		}
	}
	payload, err := BuildCommandPayload(nonce, "printf '%s\\n' hello")
	if err != nil || !strings.Contains(string(payload), "7770;E;") {
		t.Fatalf("payload: %q, %v", payload, err)
	}
}

func TestSanitizeText(t *testing.T) {
	t.Parallel()
	got, truncated := SanitizeText([]byte("old\rnew\x1b]7770;P;secret\x07\n"), 128)
	if got != "new\n" || truncated {
		t.Fatalf("SanitizeText = %q, %v", got, truncated)
	}
}
