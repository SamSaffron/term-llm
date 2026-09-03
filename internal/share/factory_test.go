package share

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewPublisherReturnsActionableSafeConfigurationErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  config.ShareConfig
		want string
	}{
		{name: "timeout", cfg: config.ShareConfig{Provider: "command", Command: []string{"helper"}, Timeout: "not-a-duration\nSECRET"}, want: "timeout configuration is invalid"},
		{name: "provider", cfg: config.ShareConfig{Provider: "unknown\nSECRET"}, want: "unsupported provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewPublisher(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
