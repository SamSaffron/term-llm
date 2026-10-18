package llm

import "github.com/samsaffron/term-llm/internal/llm/core/stream"

// StreamIncompleteError reports a streaming response that ended before the
// protocol terminal marker arrived.
// Deprecated: use stream.StreamIncompleteError in new packages.
type StreamIncompleteError = stream.StreamIncompleteError

// ResponsesIncompleteError reports an explicit response.incomplete terminal
// event from the provider.
// Deprecated: use stream.ResponsesIncompleteError in new packages.
type ResponsesIncompleteError = stream.ResponsesIncompleteError
