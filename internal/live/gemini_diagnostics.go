package live

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	geminiDiagnosticSecretKey = regexp.MustCompile(`(?i)(api[_-]?key|key|authorization|auth|bearer|token|capability|credential|secret|handle)`)
	geminiDiagnosticSecret    = regexp.MustCompile(`(?i)((?:api[_-]?key|key|authorization|auth|token|capability|credential|secret)\s*[:=]\s*)(?:bearer\s+)?[^\s,;&"']+`)
	geminiDiagnosticBearer    = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
)

type geminiDiagnostics struct {
	enabled bool
	raw     bool
	started time.Time
	last    time.Time
	logf    func(string, ...any)
	liveID  string
	apiKey  string // excluded even if an upstream error echoes it without a label

	mu                    sync.Mutex
	incoming              int
	inputPCMChunks        int
	inputPCMBytes         int
	outputPCMChunks       int
	outputPCMBytes        int
	inputTranscriptions   int
	interimTranscriptions int
	outputTranscriptions  int
	turns                 int
	interruptions         int
}

func newGeminiDiagnostics(opts SessionOptions) *geminiDiagnostics {
	d := newGeminiDiagnosticsWithLogger(opts.Debug || opts.DebugRaw, opts.DebugRaw, log.Printf)
	if d != nil {
		d.liveID = opts.LiveID
	}
	return d
}

func newGeminiDiagnosticsWithLogger(enabled, raw bool, logf func(string, ...any)) *geminiDiagnostics {
	if !enabled {
		return nil
	}
	if logf == nil {
		logf = log.Printf
	}
	now := time.Now()
	return &geminiDiagnostics{enabled: true, raw: raw, started: now, last: now, logf: logf}
}

func (d *geminiDiagnostics) metadata(format string, args ...any) {
	if d == nil || !d.enabled {
		return
	}
	line := fmt.Sprintf("[live gemini] live_id=%q ", d.liveID) + fmt.Sprintf(format, args...)
	if d.apiKey != "" {
		for _, secret := range []string{d.apiKey, url.QueryEscape(d.apiKey), url.PathEscape(d.apiKey)} {
			line = strings.ReplaceAll(line, secret, "[REDACTED]")
		}
	}
	d.logf("%s", line)
}

func (d *geminiDiagnostics) setup(model, voice string, historyItems int) {
	if d == nil {
		return
	}
	d.metadata("setup sent elapsed_ms=%d model=%q voice=%q history_items=%d", time.Since(d.started).Milliseconds(), redactGeminiDiagnosticString(model), redactGeminiDiagnosticString(voice), historyItems)
}

func (d *geminiDiagnostics) inputPCM(size int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.inputPCMChunks++
	d.inputPCMBytes += size
	chunks, bytes := d.inputPCMChunks, d.inputPCMBytes
	d.mu.Unlock()
	d.metadata("pcm input chunk_bytes=%d chunks=%d bytes=%d elapsed_ms=%d", size, chunks, bytes, time.Since(d.started).Milliseconds())
}

func (d *geminiDiagnostics) incomingMessage(payload []byte, message *geminiServerMessage) {
	if d == nil {
		return
	}
	now := time.Now()
	d.mu.Lock()
	d.incoming++
	sequence := d.incoming
	delta := now.Sub(d.last).Milliseconds()
	d.last = now
	kinds := geminiIncomingKinds(message)
	if content := message.ServerContent; content != nil {
		if content.InputTranscription != nil {
			d.inputTranscriptions++
		}
		if content.InterimInputTranscription != nil {
			d.interimTranscriptions++
		}
		if content.OutputTranscription != nil {
			d.outputTranscriptions++
		}
		if content.TurnComplete {
			d.turns++
		}
		if content.Interrupted {
			d.interruptions++
		}
		if content.ModelTurn != nil {
			for _, part := range content.ModelTurn.Parts {
				if part.InlineData == nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(part.InlineData.MIMEType)), "audio/pcm") {
					continue
				}
				d.outputPCMChunks++
				d.outputPCMBytes += len(part.InlineData.Data)
			}
		}
	}
	inputTranscriptions := d.inputTranscriptions
	interimTranscriptions := d.interimTranscriptions
	outputTranscriptions := d.outputTranscriptions
	outputChunks, outputBytes := d.outputPCMChunks, d.outputPCMBytes
	var outputEventSizes []int
	if content := message.ServerContent; content != nil && content.ModelTurn != nil {
		for _, part := range content.ModelTurn.Parts {
			if part.InlineData != nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(part.InlineData.MIMEType)), "audio/pcm") {
				outputEventSizes = append(outputEventSizes, len(part.InlineData.Data))
			}
		}
	}
	turns, interruptions := d.turns, d.interruptions
	d.mu.Unlock()

	d.metadata("upstream event=%d kinds=%s elapsed_ms=%d since_previous_ms=%d input_transcriptions=%d interim_transcriptions=%d output_transcriptions=%d output_pcm_event_sizes=%v output_pcm_chunks=%d output_pcm_bytes=%d turns=%d interruptions=%d",
		sequence, strings.Join(kinds, "+"), now.Sub(d.started).Milliseconds(), delta,
		inputTranscriptions, interimTranscriptions, outputTranscriptions, outputEventSizes, outputChunks, outputBytes, turns, interruptions)
	if d.raw {
		d.metadata("upstream raw=%s", sanitizeGeminiInboundJSON(payload))
	}
}

func (d *geminiDiagnostics) malformed(payload []byte, err error) {
	if d == nil {
		return
	}
	d.metadata("upstream malformed bytes=%d elapsed_ms=%d error=%q", len(payload), time.Since(d.started).Milliseconds(), redactGeminiDiagnosticString(err.Error()))
	if d.raw {
		d.metadata("upstream raw=%s", sanitizeGeminiInboundJSON(payload))
	}
}

func (d *geminiDiagnostics) connectionError(stage string, err error) {
	if d == nil || err == nil {
		return
	}
	d.metadata("%s elapsed_ms=%d error=%q", stage, time.Since(d.started).Milliseconds(), redactGeminiDiagnosticString(err.Error()))
}

func geminiIncomingKinds(message *geminiServerMessage) []string {
	if message == nil {
		return []string{"unknown"}
	}
	var kinds []string
	if message.SetupComplete != nil {
		kinds = append(kinds, "setupComplete")
	}
	if message.Error != nil {
		kinds = append(kinds, "error")
	}
	if message.GoAway != nil {
		kinds = append(kinds, "goAway")
	}
	if message.ToolCall != nil {
		kinds = append(kinds, "toolCall")
	}
	if message.ToolCallCancellation != nil {
		kinds = append(kinds, "toolCallCancellation")
	}
	if content := message.ServerContent; content != nil {
		kinds = append(kinds, "serverContent")
		if content.InputTranscription != nil {
			kinds = append(kinds, "inputTranscription")
		}
		if content.InterimInputTranscription != nil {
			kinds = append(kinds, "interimInputTranscription")
		}
		if content.OutputTranscription != nil {
			kinds = append(kinds, "outputTranscription")
		}
		if content.ModelTurn != nil {
			kinds = append(kinds, "modelTurn")
		}
		if content.GenerationComplete {
			kinds = append(kinds, "generationComplete")
		}
		if content.TurnComplete {
			kinds = append(kinds, "turnComplete")
		}
		if content.Interrupted {
			kinds = append(kinds, "interrupted")
		}
	}
	if len(kinds) == 0 {
		return []string{"unknown"}
	}
	return kinds
}

func sanitizeGeminiInboundJSON(payload []byte) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Sprintf(`{"invalid_json":true,"bytes":%d}`, len(payload))
	}
	value = sanitizeGeminiDiagnosticValue(value)
	safe, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf(`{"unavailable":true,"bytes":%d}`, len(payload))
	}
	return string(safe)
}

func sanitizeGeminiDiagnosticValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]any, len(typed))
		mime := strings.ToLower(strings.TrimSpace(fmt.Sprint(typed["mimeType"])))
		for _, key := range keys {
			child := typed[key]
			lower := strings.ToLower(key)
			switch {
			case geminiDiagnosticSecretKey.MatchString(lower):
				result[key] = "[REDACTED]"
			case lower == "args" || lower == "arguments" || lower == "prompt" || lower == "history" || lower == "instructions" || lower == "systeminstruction" || lower == "clientcontent" || lower == "turns":
				// Tool arguments and prompt/history containers can contain full user requests.
				result[key] = "[REDACTED]"
			case lower == "data" && strings.HasPrefix(mime, "audio/pcm"):
				result[key] = map[string]any{"pcmBytes": diagnosticBase64Bytes(child)}
			case lower == "data":
				// Unknown data containers are withheld rather than risk emitting an
				// audio payload from a future Gemini event shape.
				result[key] = "[REDACTED]"
			default:
				result[key] = sanitizeGeminiDiagnosticValue(child)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = sanitizeGeminiDiagnosticValue(typed[index])
		}
		return result
	case string:
		return redactGeminiDiagnosticString(typed)
	default:
		return value
	}
}

func diagnosticBase64Bytes(value any) int {
	encoded, ok := value.(string)
	if !ok || encoded == "" {
		return 0
	}
	if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
		return len(decoded)
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(encoded); err == nil {
		return len(decoded)
	}
	return base64.StdEncoding.DecodedLen(len(encoded))
}

func redactGeminiDiagnosticString(value string) string {
	value = geminiDiagnosticSecret.ReplaceAllString(value, `${1}[REDACTED]`)
	value = geminiDiagnosticBearer.ReplaceAllString(value, `${1}[REDACTED]`)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		query := parsed.Query()
		changed := false
		for key := range query {
			if geminiDiagnosticSecretKey.MatchString(key) {
				query.Set(key, "[REDACTED]")
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = query.Encode()
			value = parsed.String()
		}
	}
	return value
}
