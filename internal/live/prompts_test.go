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
