package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

// Deprecated: use core.ModelSwapEventType.
const ModelSwapEventType = core.ModelSwapEventType

// Deprecated: use core.ModelSwapMarker.
type ModelSwapMarker = core.ModelSwapMarker

// Deprecated: use core.ModelSwapEventMessage.
func ModelSwapEventMessage(marker ModelSwapMarker) Message { return core.ModelSwapEventMessage(marker) }

// Deprecated: use core.ParseModelSwapMarker.
func ParseModelSwapMarker(msg Message) (ModelSwapMarker, bool) { return core.ParseModelSwapMarker(msg) }

// Deprecated: use core.FormatModelSwapMarker.
func FormatModelSwapMarker(marker ModelSwapMarker) string { return core.FormatModelSwapMarker(marker) }
