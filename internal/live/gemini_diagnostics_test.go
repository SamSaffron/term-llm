package live

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestGeminiRawDiagnosticsSanitizeSecretsAudioAndPrompts(t *testing.T) {
	audio := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})
	payload := []byte(fmt.Sprintf(`{
		"authorization":"Bearer auth-secret",
		"serverContent":{
			"inputTranscription":{"text":"spoken transcript"},
			"modelTurn":{"parts":[{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":%q}}]}
		},
		"toolCall":{"functionCalls":[{"args":{"input":"full private prompt"}}]},
		"url":"wss://example.test/live?key=query-secret&capability=media-secret",
		"sessionResumptionUpdate":{"newHandle":"resumption-secret","resumable":true},
		"nested":{"api_key":"api-secret","key":"plain-key-secret","token":"token-secret"}
	}`, audio))

	safe := sanitizeGeminiInboundJSON(payload)
	for _, secret := range []string{"auth-secret", "query-secret", "media-secret", "api-secret", "plain-key-secret", "token-secret", "resumption-secret", audio, "full private prompt"} {
		if strings.Contains(safe, secret) {
			t.Fatalf("sanitized payload contains %q: %s", secret, safe)
		}
	}
	for _, expected := range []string{"spoken transcript", `"pcmBytes":4`, "[REDACTED]"} {
		if !strings.Contains(safe, expected) {
			t.Fatalf("sanitized payload missing %q: %s", expected, safe)
		}
	}
}

func TestGeminiDiagnosticsCorrelateAndRedactUnlabelledKey(t *testing.T) {
	var lines []string
	d := newGeminiDiagnosticsWithLogger(true, true, func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	d.liveID = "live_test"
	d.apiKey = "synthetic-secret-key"
	d.metadata("provider echoed %s", d.apiKey)
	d.incomingMessage([]byte(`{"serverContent":{"inputTranscription":{"text":"synthetic-secret-key"}}}`), &geminiServerMessage{})
	for _, line := range lines {
		if strings.Contains(line, d.apiKey) {
			t.Fatal("configured key leaked without a credential field label")
		}
		if !strings.Contains(line, `live_id="live_test"`) {
			t.Fatalf("missing call correlation: %s", line)
		}
	}
}

func TestGeminiDiagnosticsAreDisabledByDefault(t *testing.T) {
	calls := 0
	diagnostics := newGeminiDiagnosticsWithLogger(false, true, func(string, ...any) { calls++ })
	if diagnostics != nil {
		t.Fatal("disabled diagnostics allocated state")
	}
	// Nil receivers are intentional so disabled call sites do no logging or raw
	// JSON sanitization work.
	diagnostics.metadata("not logged")
	diagnostics.incomingMessage([]byte(`{"serverContent":{}}`), &geminiServerMessage{})
	if calls != 0 {
		t.Fatalf("disabled diagnostics logged %d entries", calls)
	}
}

func TestGeminiDiagnosticsMetadataCountsAndRawOptIn(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	diagnostics := newGeminiDiagnosticsWithLogger(true, false, logf)
	message := &geminiServerMessage{ServerContent: &geminiServerContent{
		InputTranscription:        &geminiTranscription{Text: "input"},
		InterimInputTranscription: &geminiTranscription{Text: "interim"},
		OutputTranscription:       &geminiTranscription{Text: "output"},
		TurnComplete:              true,
		Interrupted:               true,
		ModelTurn: &geminiContent{Parts: []geminiPart{{InlineData: &geminiInlineData{
			MIMEType: "audio/pcm;rate=24000", Data: []byte{1, 0, 2, 0},
		}}}},
	}}
	diagnostics.inputPCM(3200)
	diagnostics.incomingMessage([]byte(`{"serverContent":{}}`), message)
	joined := strings.Join(lines, "\n")
	for _, expected := range []string{"input chunk_bytes=3200", "input_transcriptions=1", "interim_transcriptions=1", "output_transcriptions=1", "output_pcm_event_sizes=[4]", "output_pcm_chunks=1", "output_pcm_bytes=4", "turns=1", "interruptions=1"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("metadata missing %q:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "upstream raw=") {
		t.Fatalf("raw event logged without raw opt-in:\n%s", joined)
	}
}
