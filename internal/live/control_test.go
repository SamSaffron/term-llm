package live

import (
	"errors"
	"strings"
	"testing"
)

// TestControlRejectionTextStatesTheReason is the whole retry channel for a request
// the host could not handle: the voice model is the only thing that can explain it,
// so the reason has to be in the text it hears.
//
// It must not prescribe a wire form. Nothing instructs the model to address a
// request any more — the host routes every delegation itself — so the rejection that
// used to repeat the "control:" syntax would now be teaching a format that does not
// exist.
func TestControlRejectionTextStatesTheReason(t *testing.T) {
	text := controlRejectionText(errors.New("too many requests are already waiting"))
	if !strings.Contains(text, "too many requests are already waiting") {
		t.Fatalf("rejection text lost its reason: %q", text)
	}
	if !strings.Contains(text, "ask again") {
		t.Fatalf("rejection text leaves the model nothing to do: %q", text)
	}
	for _, forbidden := range []string{"control:", "NOT_CONTROL", "JSON", "prefix", "delegation without"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("rejection text still prescribes the removed wire format %q: %q", forbidden, text)
		}
	}
	empty := controlRejectionText(nil)
	if !strings.Contains(empty, "unknown error") {
		t.Fatalf("empty rejection text = %q", empty)
	}
}

// TestRefuseControlTagsAReasonForHosts pins the one thing a host cannot build for
// itself: the refusal marker that keeps model-directed guidance off the browser's
// error channel. The routing lane itself falls open rather than refusing, so the
// header on RefuseControl has to stay honest about who uses it.
func TestRefuseControlTagsAReasonForHosts(t *testing.T) {
	reason := errors.New("the call is ending")
	tagged := RefuseControl(reason)
	if !isControlRefusal(tagged) {
		t.Fatal("RefuseControl produced an error the controller reads as a failure")
	}
	if tagged.Error() != reason.Error() {
		t.Fatalf("refusal text = %q, want %q", tagged.Error(), reason.Error())
	}
	if !errors.Is(tagged, reason) {
		t.Fatal("refusal no longer unwraps to its reason")
	}
	if isControlRefusal(errors.New("tool exploded")) {
		t.Fatal("an untagged error was read as a refusal")
	}
	if got := controlRejectionText(tagged); !strings.Contains(got, reason.Error()) {
		t.Fatalf("refusal did not reach the voice model with its reason: %q", got)
	}
	if got := refuseControl(nil); got != nil {
		t.Fatalf("refusing nothing produced %v", got)
	}
}
