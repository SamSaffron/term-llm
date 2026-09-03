package share

import (
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

func NewPublisher(cfg config.ShareConfig) (Publisher, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", config.DefaultShareProvider:
		return NewGitHubPublisher(), nil
	case "command":
		timeoutValue := strings.TrimSpace(cfg.Timeout)
		if timeoutValue == "" {
			timeoutValue = config.DefaultShareTimeout
		}
		timeout, err := time.ParseDuration(timeoutValue)
		if err != nil {
			return nil, NewError(ErrorProtocol, "sharing provider timeout configuration is invalid")
		}
		return NewCommandPublisher(cfg.Command, timeout)
	default:
		return nil, NewError(ErrorProtocol, "sharing provider configuration selects an unsupported provider")
	}
}
