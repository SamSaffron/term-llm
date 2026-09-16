package live

import (
	"strings"
	"testing"
)

func TestParseOpenAIEventNormalizesRealtimeEvents(t *testing.T) {
	tests := []struct {
		name string
		json string
		want Event
	}{
		{
			name: "session update",
			json: `{"type":"session.updated","session":{"audio":{"output":{"voice":"marin"}}}}`,
			want: Event{Kind: EventSessionUpdated, RawType: "session.updated", Voice: "marin"},
		},
		{
			name: "user delta",
			json: `{"type":"conversation.item.input_audio_transcription.delta","delta":"run "}`,
			want: Event{Kind: EventUserTranscript, RawType: "conversation.item.input_audio_transcription.delta", Role: RoleUser, Text: "run "},
		},
		{
			name: "user done",
			json: `{"type":"conversation.item.input_audio_transcription.completed","transcript":"run tests"}`,
			want: Event{Kind: EventTurnDone, RawType: "conversation.item.input_audio_transcription.completed", Role: RoleUser, Text: "run tests"},
		},
		{
			name: "assistant delta",
			json: `{"type":"response.output_audio_transcript.delta","delta":"Sure."}`,
			want: Event{Kind: EventAssistantTranscript, RawType: "response.output_audio_transcript.delta", Role: RoleAssistant, Text: "Sure."},
		},
		{
			name: "assistant done",
			json: `{"type":"response.output_audio_transcript.done","transcript":"Sure."}`,
			want: Event{Kind: EventTurnDone, RawType: "response.output_audio_transcript.done", Role: RoleAssistant, Text: "Sure."},
		},
		{
			name: "assistant text done",
			json: `{"type":"response.output_text.done","text":"Typed answer."}`,
			want: Event{Kind: EventTurnDone, RawType: "response.output_text.done", Role: RoleAssistant, Text: "Typed answer."},
		},
		{
			name: "delegation",
			json: `{"type":"response.function_call_arguments.done","name":"delegate_to_controller","call_id":"call_42","arguments":"{\"input\":\"inspect the repository\"}"}`,
			want: Event{Kind: EventDelegationCreated, RawType: "response.function_call_arguments.done", DelegationID: "call_42", Text: "inspect the repository"},
		},
		{
			name: "provider error",
			json: `{"type":"error","error":{"type":"invalid_request_error","code":"bad","message":"nope"}}`,
			want: Event{Kind: EventError, RawType: "error", Text: "nope"},
		},
		{
			name: "closed",
			json: `{"type":"session.closed"}`,
			want: Event{Kind: EventEnded, RawType: "session.closed"},
		},
		{
			name: "other tool ignored",
			json: `{"type":"response.function_call_arguments.done","name":"other","call_id":"call_1","arguments":"{}"}`,
			want: Event{Kind: EventUnknown, RawType: "response.function_call_arguments.done"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseOpenAIEvent([]byte(test.json))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("event = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseOpenAIEventRejectsBadDelegationArgumentsAsProviderError(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.function_call_arguments.done","name":"delegate_to_controller","arguments":"{\"input\":\"work\"}"}`,
		`{"type":"response.function_call_arguments.done","name":"delegate_to_controller","call_id":"call_1","arguments":"not-json"}`,
		`{"type":"response.function_call_arguments.done","name":"delegate_to_controller","call_id":"call_1","arguments":"{\"input\":\"  \"}"}`,
	} {
		event, err := parseOpenAIEvent([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind != EventError || !strings.Contains(event.Text, "OpenAI Realtime delegation") {
			t.Fatalf("event = %+v", event)
		}
	}
}

func TestParseOpenAILiveEventNormalizesDocumentedEvents(t *testing.T) {
	tests := []struct {
		name string
		json string
		want Event
	}{
		{
			name: "started",
			json: `{"type":"session.started","event_id":"event_1","session":{"id":"live_1"}}`,
			want: Event{Kind: EventSessionStarted, RawType: "session.started"},
		},
		{
			name: "user transcript",
			json: `{"type":"session.input_transcript.delta","event_id":"event_2","delta":"run ","start_ms":100,"end_ms":200}`,
			want: Event{Kind: EventUserTranscript, RawType: "session.input_transcript.delta", Role: RoleUser, Text: "run "},
		},
		{
			name: "assistant transcript",
			json: `{"type":"session.output_transcript.delta","event_id":"event_3","delta":"Okay.","start_ms":200,"end_ms":300}`,
			want: Event{Kind: EventAssistantTranscript, RawType: "session.output_transcript.delta", Role: RoleAssistant, Text: "Okay."},
		},
		{
			name: "client delegation metadata",
			json: `{"type":"session.delegation.created","event_id":"event_4","offset_ms":300,"delegation":{"id":"item_42","type":"delegation","target":"client"}}`,
			want: Event{Kind: EventDelegationCreated, RawType: "session.delegation.created", DelegationID: "item_42"},
		},
		{
			name: "responses delegation ignored",
			json: `{"type":"session.delegation.created","event_id":"event_5","offset_ms":300,"delegation":{"id":"item_43","type":"delegation","target":"responses","response_id":"resp_1"}}`,
			want: Event{Kind: EventUnknown, RawType: "session.delegation.created"},
		},
		{
			name: "provider error",
			json: `{"type":"error","error":{"type":"invalid_request_error","code":"bad","message":"nope","client_event_id":"client_1"}}`,
			want: Event{Kind: EventError, RawType: "error", Text: "nope"},
		},
		{
			name: "closed",
			json: `{"type":"session.closed","reason":"close_requested"}`,
			want: Event{Kind: EventEnded, RawType: "session.closed"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseOpenAILiveEvent([]byte(test.json))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("event = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseOpenAILiveEventRejectsMissingClientDelegationID(t *testing.T) {
	event, err := parseOpenAILiveEvent([]byte(`{"type":"session.delegation.created","delegation":{"type":"delegation","target":"client"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != EventError || !strings.Contains(event.Text, "missing an id") {
		t.Fatalf("event = %+v", event)
	}
}
func TestParseOpenAIEventMalformedJSON(t *testing.T) {
	if event, err := parseOpenAIEvent([]byte(`{"type":`)); err == nil || event.Kind != EventUnknown {
		t.Fatalf("event/error = %+v/%v", event, err)
	}
}
