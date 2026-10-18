package responses

import "github.com/samsaffron/term-llm/internal/llm/core"

type (
	Request         = core.Request
	ToolFileData    = core.ToolFileData
	ToolResult      = core.ToolResult
	ToolImageData   = core.ToolImageData
	ToolContentPart = core.ToolContentPart
	ToolOutput      = core.ToolOutput
)

const (
	ToolContentPartText      = core.ToolContentPartText
	ToolContentPartImageData = core.ToolContentPartImageData
	ServiceTierFast          = "priority"
)

var (
	UserText                    = core.UserText
	AssistantText               = core.AssistantText
	SystemText                  = core.SystemText
	ToolResultMessage           = core.ToolResultMessage
	ToolResultMessageFromOutput = core.ToolResultMessageFromOutput
)
