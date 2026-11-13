package engine

import "github.com/samsaffron/term-llm/internal/runtimeoutput"

// reportToolRegistrationError makes rejection observable even for historical
// void-style callers that intentionally ignore Register's new error result.
// Rejection never changes the installed execution binding or its visibility.
func reportToolRegistrationError(err error) error {
	if err != nil {
		runtimeoutput.Warn("tool registration rejected", "error", err)
	}
	return err
}
