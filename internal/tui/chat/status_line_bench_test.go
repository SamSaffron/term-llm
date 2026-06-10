package chat

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func benchStatusLineModel(width int) *Model {
	m := newTestChatModel(false)
	m.width = width
	m.engine.ConfigureContextManagement(m.provider, m.providerKey, m.modelName, false)
	m.localTools = []string{"shell", "read_file", "edit_file"}
	m.yolo = true
	m.searchEnabled = true
	return m
}

func BenchmarkRenderStatusLineWide(b *testing.B) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "mock", Model: "mock-model", InputLimit: 1048576}})
	defer llm.RegisterConfigLimits(nil)

	m := benchStatusLineModel(120)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.renderStatusLine()
	}
}

func BenchmarkRenderStatusLineNarrow(b *testing.B) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "mock", Model: "mock-model", InputLimit: 1048576}})
	defer llm.RegisterConfigLimits(nil)

	m := benchStatusLineModel(48)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.renderStatusLine()
	}
}
