package tools

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// Execute parks shared script permits while polling independently owned jobs.
// The call remains charged to its cell and resumes under the same permissions.
func (t *WaitForJobsTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return core.WithCodeModeWait(ctx, func() (core.ToolOutput, error) {
		return t.execute(ctx, args)
	})
}
