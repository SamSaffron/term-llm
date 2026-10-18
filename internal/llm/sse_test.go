package llm

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

type testSSEEvent struct {
	eventType string
	data      string
}

func TestSSEDecoderNext(t *testing.T) {
	longData := strings.Repeat("x", 1024*1024+128)

	tests := []struct {
		name           string
		input          string
		requireDone    bool
		wantEvents     []testSSEEvent
		wantDone       bool
		wantIncomplete bool
	}{
		{
			name:        "well-formed multi-event stream",
			requireDone: true,
			input: "event: message\n" +
				"data: {\"delta\":\"hello\"}\n\n" +
				"event: usage\n" +
				"data: {\"tokens\":3}\n\n" +
				"data: [DONE]\n\n",
			wantEvents: []testSSEEvent{
				{eventType: "message", data: `{"delta":"hello"}`},
				{eventType: "usage", data: `{"tokens":3}`},
			},
			wantDone: true,
		},
		{
			name:           "missing done required",
			requireDone:    true,
			input:          "data: {\"delta\":\"hello\"}\n\n",
			wantEvents:     []testSSEEvent{{data: `{"delta":"hello"}`}},
			wantIncomplete: true,
		},
		{
			name:        "missing done lenient",
			requireDone: false,
			input:       "data: {\"delta\":\"hello\"}\n\n",
			wantEvents:  []testSSEEvent{{data: `{"delta":"hello"}`}},
		},
		{
			name:        "malformed json passes through raw",
			requireDone: true,
			input: "event: chunk\n" +
				"data: {not-json\n\n" +
				"data: [DONE]\n\n",
			wantEvents: []testSSEEvent{{eventType: "chunk", data: `{not-json`}},
			wantDone:   true,
		},
		{
			name:        "very long line has no scanner limit",
			requireDone: true,
			input:       "data: " + longData + "\n\ndata: [DONE]\n\n",
			wantEvents:  []testSSEEvent{{data: longData}},
			wantDone:    true,
		},
		{
			name:        "crlf line endings",
			requireDone: true,
			input:       "event: message\r\ndata: hello\r\n\r\ndata: [DONE]\r\n\r\n",
			wantEvents:  []testSSEEvent{{eventType: "message", data: "hello"}},
			wantDone:    true,
		},
		{
			name:        "comment keepalive lines are ignored",
			requireDone: true,
			input:       ": ping\n\n: another ping\n\ndata: ok\n\ndata: [DONE]\n\n",
			wantEvents:  []testSSEEvent{{data: "ok"}},
			wantDone:    true,
		},
		{
			name:        "multi-line data fields are joined",
			requireDone: true,
			input:       "event: chunk\ndata: hello\ndata: world\n\ndata: [DONE]\n\n",
			wantEvents:  []testSSEEvent{{eventType: "chunk", data: "hello\nworld"}},
			wantDone:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoder := stream.NewDecoder(strings.NewReader(tt.input), stream.DecoderOptions{
				RequireDone: tt.requireDone,
				Transport:   "test SSE",
			})

			var got []testSSEEvent
			var gotErr error
			for {
				eventType, data, err := decoder.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					gotErr = err
					break
				}
				got = append(got, testSSEEvent{eventType: eventType, data: string(data)})
			}

			assertSSEEvents(t, got, tt.wantEvents)
			if decoder.DoneSeen() != tt.wantDone {
				t.Fatalf("DoneSeen() = %v, want %v", decoder.DoneSeen(), tt.wantDone)
			}

			if tt.wantIncomplete {
				var incomplete *stream.StreamIncompleteError
				if !errors.As(gotErr, &incomplete) {
					t.Fatalf("error = %T %v, want StreamIncompleteError", gotErr, gotErr)
				}
				if !strings.Contains(gotErr.Error(), "[DONE]") {
					t.Fatalf("incomplete error = %v, want mention [DONE]", gotErr)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("unexpected error: %v", gotErr)
			}
		})
	}
}

func assertSSEEvents(t *testing.T, got, want []testSSEEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].eventType != want[i].eventType {
			t.Fatalf("event %d type = %q, want %q", i, got[i].eventType, want[i].eventType)
		}
		if got[i].data != want[i].data {
			t.Fatalf("event %d data len = %d, want %d; prefix = %q", i, len(got[i].data), len(want[i].data), prefixForTest(got[i].data, 80))
		}
	}
}

func prefixForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
