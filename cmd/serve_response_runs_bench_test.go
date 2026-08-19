package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func BenchmarkResponseRunAppendTextDeltaRetainedReplay(b *testing.B) {
	run := newResponseRun("resp_bench", "sess_bench", "", "mock", time.Now().Unix(), func() {})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := run.appendTextDeltaSegmentEvent(0, 0, ""); err != nil {
			b.Fatalf("appendTextDeltaSegmentEvent failed: %v", err)
		}
	}
}

func BenchmarkConfigureResponseRunRevisionLargeBodies(b *testing.B) {
	for _, rowCount := range []int{1, 256} {
		b.Run(fmt.Sprintf("rows=%d", rowCount), func(b *testing.B) {
			ctx := context.Background()
			rawStore, err := session.NewSQLiteStore(session.Config{
				Enabled: true,
				Path:    filepath.Join(b.TempDir(), "sessions.db"),
			})
			if err != nil {
				b.Fatalf("NewSQLiteStore: %v", err)
			}
			b.Cleanup(func() { _ = rawStore.Close() })
			sess := &session.Session{ID: fmt.Sprintf("sess-bench-%d", rowCount), Provider: "test", Model: "test", Mode: session.ModeChat}
			if err := rawStore.Create(ctx, sess); err != nil {
				b.Fatalf("Create: %v", err)
			}
			payload := strings.Repeat("a", 32<<10)
			for i := 0; i < rowCount; i++ {
				var message llm.Message
				if i%2 == 0 {
					message = llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{
						Type:       llm.PartToolResult,
						ToolResult: &llm.ToolResult{ID: fmt.Sprintf("call-%d", i), Name: "shell", Content: payload},
					}}}
				} else {
					message = llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{
						Type:      llm.PartImage,
						ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: payload},
					}}}
				}
				if err := rawStore.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
					b.Fatalf("AddMessage %d: %v", i, err)
				}
			}
			server := &serveServer{store: session.NewLoggingStore(rawStore, nil)}

			b.Run("compact-start-state", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					run := newResponseRun("resp-bench", sess.ID, "", "test", time.Now().Unix(), nil)
					server.configureResponseRunRevision(run, sess.ID)
				}
			})
			b.Run("full-snapshot-reference", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := rawStore.GetTranscriptSnapshot(ctx, sess.ID); err != nil {
						b.Fatalf("GetTranscriptSnapshot: %v", err)
					}
				}
			})
		})
	}
}
