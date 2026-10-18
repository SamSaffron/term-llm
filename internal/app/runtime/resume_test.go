package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestRepairChildResumeHistoryRequiresStore(t *testing.T) {
	t.Parallel()
	_, err := RepairChildResumeHistory(context.Background(), nil, "child")
	if err == nil || !strings.Contains(err.Error(), "persistence is unavailable") {
		t.Fatalf("missing store error = %v", err)
	}
}
