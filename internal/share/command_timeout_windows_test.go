//go:build windows

package share

import (
	"context"
	"testing"
	"time"
)

func TestCommandPublisherTimeout(t *testing.T) {
	publisher := commandTestPublisher(t, "timeout", 100*time.Millisecond)
	_, err := publisher.Create(context.Background(), commandTestRequest())
	if err == nil || AsError(err).Code != ErrorTimeout {
		t.Fatalf("error=%v", err)
	}
}
