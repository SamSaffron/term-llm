package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type responseRunStartErrorStore struct {
	NoopStore
	err error
}

func (s *responseRunStartErrorStore) GetResponseRunStartState(context.Context, string) (ResponseRunStartState, error) {
	return ResponseRunStartState{}, s.err
}

func TestLoggingStoreBatchTranscriptCapabilityReflectsWrappedStore(t *testing.T) {
	unsupported := NewLoggingStore(&responseRunStartErrorStore{}, func(string, ...any) {})
	if SupportsBatchTranscriptWriter(unsupported) {
		t.Fatal("capabilityless wrapped store reported batch support")
	}
	store, _ := newTranscriptTestStore(t)
	wrapped := NewLoggingStore(store, func(string, ...any) {})
	if !SupportsBatchTranscriptWriter(wrapped) {
		t.Fatal("SQLite-backed logging store did not report batch support")
	}
}

func TestLoggingStoreResponseRunStartStateIgnoresExpectedErrors(t *testing.T) {
	for _, expected := range []error{ErrNotFound, ErrTranscriptRevisionUnsupported} {
		t.Run(expected.Error(), func(t *testing.T) {
			var warnings []string
			store := NewLoggingStore(&responseRunStartErrorStore{err: expected}, func(format string, args ...any) {
				warnings = append(warnings, fmt.Sprintf(format, args...))
			})
			if _, err := store.GetResponseRunStartState(context.Background(), "missing"); !errors.Is(err, expected) {
				t.Fatalf("error = %v, want %v", err, expected)
			}
			if len(warnings) != 0 {
				t.Fatalf("warnings = %q, want none", warnings)
			}
		})
	}
}

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
