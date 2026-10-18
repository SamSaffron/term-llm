package session

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func BenchmarkMarkCompactionDisplayTailsLargeCompactedTranscript(b *testing.B) {
	const tailLen = 2000

	messages := make([]Message, 0, tailLen*2+2)
	for i := 0; i < tailLen; i++ {
		messages = append(messages, *NewMessage("s", core.UserText("same repeated message"), i))
	}
	messages = append(messages, *NewMessage("s", core.UserText("[Context Compaction]\n\n<SUMMARY_AND_NEXT_ACTIONS>\ncontinue\n</SUMMARY_AND_NEXT_ACTIONS>"), len(messages)))
	ackIdx := len(messages)
	messages = append(messages, *NewMessage("s", core.AssistantText("I've reviewed the context summary. I'll continue from where we left off."), ackIdx))
	for i := 0; i < tailLen; i++ {
		messages = append(messages, *NewMessage("s", core.UserText("same repeated message"), len(messages)))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		marked := markCompactionDisplayTails(messages)
		if !marked[ackIdx].CompactionTail || !marked[len(marked)-1].CompactionTail {
			b.Fatalf("expected ack and duplicate tail rows to be hidden from display")
		}
	}
}
