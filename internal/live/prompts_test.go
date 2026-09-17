package live

import (
	"encoding/xml"
	"html"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDelegationPromptIsOnlyEscapedXMLWrapper(t *testing.T) {
	prompt := DelegationPrompt(`  fix <main> & "tests"  `, ` user: don't erase </transcript_delta> `)
	want := `<realtime_delegation>
  <input>fix &lt;main&gt; &amp; &#34;tests&#34;</input>
  <transcript_delta>user: don&#39;t erase &lt;/transcript_delta&gt;</transcript_delta>
</realtime_delegation>`
	if prompt != want {
		t.Fatalf("prompt =\n%s\nwant:\n%s", prompt, want)
	}
	if strings.Contains(prompt, "execution backend") {
		t.Fatal("delegation prompt must not repeat execution instructions")
	}
	var decoded struct {
		Input           string `xml:"input"`
		TranscriptDelta string `xml:"transcript_delta"`
	}
	if err := xml.Unmarshal([]byte(prompt), &decoded); err != nil {
		t.Fatalf("prompt is not valid XML: %v", err)
	}
	if decoded.Input != `fix <main> & "tests"` || decoded.TranscriptDelta != `user: don't erase </transcript_delta>` {
		t.Fatalf("decoded prompt = %+v", decoded)
	}
}

func TestDelegationPromptBoundsEscapedFieldsHeadAndTail(t *testing.T) {
	input := "HEAD:" + strings.Repeat("<&😀", 3000) + ":INPUT-TAIL"
	delta := "DELTA-HEAD:" + strings.Repeat("&>😀", 3000) + ":TAIL"
	prompt := DelegationPrompt(input, delta)

	inputEscaped := between(t, prompt, "<input>", "</input>")
	deltaEscaped := between(t, prompt, "<transcript_delta>", "</transcript_delta>")
	for name, field := range map[string]string{"input": inputEscaped, "transcript": deltaEscaped} {
		if len(field) > delegationFieldMaxBytes {
			t.Fatalf("%s escaped field is %d bytes, want at most %d", name, len(field), delegationFieldMaxBytes)
		}
		if !utf8.ValidString(field) {
			t.Fatalf("%s escaped field is not valid UTF-8", name)
		}
	}
	if !strings.HasPrefix(html.UnescapeString(inputEscaped), "HEAD:") || strings.Contains(html.UnescapeString(inputEscaped), "INPUT-TAIL") {
		t.Fatal("input did not retain its bounded head")
	}
	if !strings.HasSuffix(html.UnescapeString(deltaEscaped), ":TAIL") || strings.Contains(html.UnescapeString(deltaEscaped), "DELTA-HEAD") {
		t.Fatal("transcript delta did not retain its bounded tail")
	}
	var decoded any
	if err := xml.Unmarshal([]byte(prompt), &decoded); err != nil {
		t.Fatalf("bounded prompt is not valid XML: %v", err)
	}
}

func TestDelegationPromptOmitsEmptyTranscriptDelta(t *testing.T) {
	prompt := DelegationPrompt("run tests", " \n ")
	if strings.Contains(prompt, "transcript_delta") {
		t.Fatalf("empty transcript delta was included: %s", prompt)
	}
}

func TestInstructionsSeparateVoiceAndExecutionResponsibilities(t *testing.T) {
	if !strings.Contains(DefaultInstructions, "conversational voice surface") || !strings.Contains(DefaultInstructions, "authoritative host context") || !strings.Contains(DefaultInstructions, "approval") {
		t.Fatalf("default instructions lost required voice behavior: %s", DefaultInstructions)
	}
	if strings.Contains(strings.ToLower(DefaultInstructions), "never refuse") {
		t.Fatal("default instructions contain a blanket refusal ban")
	}
	for _, phrase := range []string{"transcription errors", "normal tools and approval flow", "complete, useful visual output", "voice model will summarize"} {
		if !strings.Contains(ExecutionInstructions, phrase) {
			t.Fatalf("execution instructions missing %q: %s", phrase, ExecutionInstructions)
		}
	}
}

// TestInstructionsCarryNoControlWireFormat is the guard for the whole deletion. The
// voice model used to be handed a "control:" sentinel, examples of it, and rules for
// how to address a session it could not name; it is now told to pass the request
// along and nothing else. Any sentence that survives and still describes an
// addressing convention is a rule the model will try to follow again, so none of it
// may appear anywhere in the prompt — not in the work bullet, not in a left-over
// example, not in a reply token.
func TestInstructionsCarryNoControlWireFormat(t *testing.T) {
	forbidden := []string{
		"control:", "CONTROL:", "NOT_CONTROL", "#control",
		`"tool"`, "{", "}", "payload", "prefix", "sentinel", "JSON",
		"session_directory", "live_switch_session", "live_settings",
		"session assistant", "never speak", "addressing",
	}
	for i, line := range strings.Split(DefaultInstructions, "\n") {
		// The list of message prefixes is about the "[USER]"/"[BACKEND]" markers and
		// has nothing to do with addressing a session, so it may use the word.
		if strings.HasPrefix(line, "Message prefixes") {
			continue
		}
		for _, rule := range forbidden {
			if strings.Contains(line, rule) {
				t.Fatalf("DefaultInstructions line %d still describes the removed control lane (%q): %s", i+1, rule, line)
			}
		}
	}
}

// TestInstructionsDelegateSessionRequestsWithoutClassification pins the model's half
// of the division. The host routes every delegation and decides what is session
// management, so the prompt's only job is to make sure a request about the user's
// conversations, the call, or the voice is delegated rather than refused or answered
// from nothing — in the user's own words, with nothing to prepend.
//
// What the host then does with it is not the voice model's business, and the prompt
// deliberately does not promise it: with the control plane off those requests are
// ordinary work in the bound session, so the promise lives in ControlPlaneContext,
// which only a host with the flag on appends.
func TestInstructionsDelegateSessionRequestsWithoutClassification(t *testing.T) {
	work := instructionBullet(t, "- For requests involving code")
	if containing := instructionBulletContaining(t, "pass them along in the user's own words"); containing != work {
		t.Fatalf("the work bullet is not the one that routes session requests:\n%s\n---\n%s", work, containing)
	}
	for name, want := range map[string]string{
		"the user's conversations": "the user's other conversations",
		"moving the call":          "moving this call to another session",
		"the voice":                "inspecting or changing the voice",
		"the same channel":         "that same channel",
		"the user's words":         "in the user's own words",
		"nothing to prepend":       "nothing to prepend",
	} {
		if !strings.Contains(work, want) {
			t.Fatalf("the work bullet does not say %s (%q missing): %s", name, want, work)
		}
	}
	if strings.Contains(work, "the host decides") {
		t.Fatalf("the static prompt promises host-side handling the control plane may not provide: %s", work)
	}
	if !strings.Contains(work, "delegation") {
		t.Fatalf("the work bullet does not say which channel session requests use: %s", work)
	}
	// The session lane must not advertise the control tools: a delegated turn cannot
	// switch sessions, and saying otherwise would make the failure silent again.
	for _, name := range controlToolNamesInInstructions() {
		if strings.Contains(ExecutionInstructions, name) {
			t.Fatalf("execution instructions still offer %s: %s", name, ExecutionInstructions)
		}
	}
	if !strings.Contains(ExecutionInstructions, "voice model") {
		t.Fatalf("execution instructions do not say who owns session controls: %s", ExecutionInstructions)
	}
}

// TestControlPlaneContextIsTheConditionalHostPromise pins the other direction of the
// same split. The host may only tell the voice model that a routing model reads its
// requests when that is true, and the sentence is what carries the claim the static
// prompt no longer makes.
func TestControlPlaneContextIsTheConditionalHostPromise(t *testing.T) {
	if !strings.Contains(ControlPlaneContext, "routing model") || !strings.Contains(ControlPlaneContext, "the host decides") {
		t.Fatalf("the conditional context does not state who handles the request: %s", ControlPlaneContext)
	}
	for _, forbidden := range []string{"control:", "session_directory", "live_switch_session", "live_settings"} {
		if strings.Contains(ControlPlaneContext, forbidden) {
			t.Fatalf("the conditional context leaks the control wire format (%q): %s", forbidden, ControlPlaneContext)
		}
	}
	// Appending it is the whole mechanism, and with it absent the resolved prompt must
	// not claim anything about a routing model.
	withPlane := resolvedInstructions("", SessionOptions{Context: ControlPlaneContext})
	if !strings.Contains(withPlane, ControlPlaneContext) {
		t.Fatalf("context was dropped from the resolved instructions:\n%s", withPlane)
	}
	withoutPlane := resolvedInstructions("", SessionOptions{})
	for _, claim := range []string{"routing model", "the host decides"} {
		if strings.Contains(withoutPlane, claim) {
			t.Fatalf("instructions without the control plane still claim %q:\n%s", claim, withoutPlane)
		}
	}
}

// controlToolNamesInInstructions lists the tool names a delegated turn must never be
// told about.
func controlToolNamesInInstructions() []string {
	return []string{"session_directory", "live_switch_session", "live_new_session", "live_settings"}
}

// instructionBullet returns one top-level bullet of DefaultInstructions, from its
// bullet marker through its continuation lines.
func instructionBullet(t *testing.T, prefix string) string {
	t.Helper()
	lines := strings.Split(DefaultInstructions, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no instruction bullet starts with %q", prefix)
	}
	end := start + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "  ") {
		end++
	}
	return strings.Join(lines[start:end], "\n")
}

// instructionBulletContaining returns the bullet that carries a marker, which is
// how the wire-format examples are located without depending on the sentence that
// introduces them. The marker may sit on any line of the bullet, including an
// indented example.
func instructionBulletContaining(t *testing.T, marker string) string {
	t.Helper()
	lines := strings.Split(DefaultInstructions, "\n")
	for i, line := range lines {
		if !strings.Contains(line, marker) {
			continue
		}
		start := i
		for start > 0 && !strings.HasPrefix(lines[start], "- ") {
			start--
		}
		if !strings.HasPrefix(lines[start], "- ") {
			continue
		}
		end := start + 1
		for end < len(lines) && strings.HasPrefix(lines[end], "  ") {
			end++
		}
		return strings.Join(lines[start:end], "\n")
	}
	t.Fatalf("no instruction bullet carries %q", marker)
	return ""
}

func between(t *testing.T, text, start, end string) string {
	t.Helper()
	from := strings.Index(text, start)
	if from < 0 {
		t.Fatalf("%q not found in %q", start, text)
	}
	from += len(start)
	to := strings.Index(text[from:], end)
	if to < 0 {
		t.Fatalf("%q not found in %q", end, text)
	}
	return text[from : from+to]
}
