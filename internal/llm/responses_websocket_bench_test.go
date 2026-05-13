package llm

import "testing"

func BenchmarkResponsesWebSocketTextDeltaDispatch(b *testing.B) {
	data := []byte(`{"type":"response.output_text.delta","delta":"hello world"}`)
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "bench", false)
	send := eventSender{}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eventType, err := responsesJSONEventType(data)
		if err != nil {
			b.Fatalf("decode event type: %v", err)
		}
		completed, err := handler.HandleJSONEvent(data, eventType, send)
		if err != nil {
			b.Fatalf("HandleJSONEvent: %v", err)
		}
		if completed {
			b.Fatal("unexpected completion")
		}
	}
}
