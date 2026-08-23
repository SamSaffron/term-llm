package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLoggingStoreIgnoresContextCancellation(t *testing.T) {
	var warnings []string
	store := NewLoggingStore(&NoopStore{}, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})

	store.logOnce("GetTranscriptSnapshot", fmt.Errorf("iterate transcript index: %w", context.Canceled))
	if len(warnings) != 0 {
		t.Fatalf("cancellation warnings = %q, want none", warnings)
	}

	store.logOnce("GetTranscriptSnapshot", errors.New("database unavailable"))
	if len(warnings) != 1 || !strings.Contains(warnings[0], "database unavailable") {
		t.Fatalf("warnings = %q, want the later persistence failure", warnings)
	}
}
