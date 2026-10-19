package llm

import (
	"bufio"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

func BenchmarkReadSSEEventChatCompletions(b *testing.B) {
	const chunk = `data: {"choices":[{"delta":{"content":"hello world"}}]}` + "\n\n"
	payload := strings.Repeat(chunk, 128) + "data: [DONE]\n\n"

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		reader := bufio.NewReader(strings.NewReader(payload))
		for {
			_, data, eof, err := stream.ReadEvent(reader)
			if err != nil {
				b.Fatalf("readSSEEvent: %v", err)
			}
			if eof && data == "" {
				break
			}
			if data == "" {
				if eof {
					break
				}
				continue
			}
			if data == "[DONE]" {
				break
			}
			if eof {
				break
			}
		}
	}
}
