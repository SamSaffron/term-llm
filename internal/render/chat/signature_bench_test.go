package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func benchmarkHistoryMessages(n int) []session.Message {
	body := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 90) // ~4KB
	msgs := make([]session.Message, 0, n)
	for i := 0; i < n; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		msgs = append(msgs, session.Message{
			ID:          int64(i + 1),
			Sequence:    i,
			Role:        role,
			TextContent: body,
			Parts:       []llm.Part{{Type: llm.PartText, Text: body}},
		})
	}
	return msgs
}

func BenchmarkMessageHistorySignature200x4KB(b *testing.B) {
	msgs := benchmarkHistoryMessages(200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MessageHistorySignature(msgs)
	}
}

func BenchmarkCachedHistorySignature200x4KB(b *testing.B) {
	msgs := benchmarkHistoryMessages(200)
	r := NewRenderer(100, 40)
	r.CachedHistorySignature(msgs) // warm the per-message cache
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.CachedHistorySignature(msgs)
	}
}
