package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestWithServeSessionLoggingReplacesExistingLogger(t *testing.T) {
	base := &session.NoopStore{}
	original := session.NewLoggingStore(base, t.Logf)

	wrapped, ok := withServeSessionLogging(original).(*session.LoggingStore)
	if !ok {
		t.Fatalf("store type = %T, want *session.LoggingStore", wrapped)
	}
	if wrapped.Store != base {
		t.Fatalf("wrapped store = %T, want the base store without a second LoggingStore", wrapped.Store)
	}
}
