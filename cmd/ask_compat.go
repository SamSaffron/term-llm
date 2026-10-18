package cmd

import (
	"context"
	"io"

	"github.com/samsaffron/term-llm/internal/app/ask"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tools"
)

// The ask command implementation lives in internal/app/ask. The declarations
// below keep the narrow set of helpers that other commands (edit, loop, runner,
// serve, guardian wiring) call, and install the cmd-owned helpers ask needs.

// MCPOptions contains options for enabling MCP servers.
type MCPOptions = ask.MCPOptions

func enableMCPServersWithFeedback(ctx context.Context, mcpFlag string, engine *llm.Engine, errWriter io.Writer, opts *MCPOptions) (*mcp.Manager, error) {
	return ask.EnableMCPServersWithFeedback(ctx, mcpFlag, engine, errWriter, opts)
}

func parseServerList(mcpFlag string) []string { return ask.ParseServerList(mcpFlag) }

func writeGuardianStatus(w io.Writer, event tools.GuardianEvent) {
	ask.WriteGuardianStatus(w, event)
}

func runOnCompleteCaptureInDir(command, input, baseDir string) (ask.OnCompleteResult, error) {
	return ask.RunOnCompleteCaptureInDir(command, input, baseDir)
}

func init() {
	ask.Uploads = ask.UploadHooks{
		SaveBytes:          saveUploadedBytesWithLimit,
		ResizeImage:        resizeImageForLLMQuiet,
		ImageDimensions:    imageDisplayDimensions,
		NormalizeMediaType: normalizeUploadMediaType,
		Dir:                serveUploadsDir,
		MaxImagePixels:     maxLLMImagePixels,
	}
	ask.Usage = ask.UsageRecorder{
		RecordTurn:   recordStoreTurnUsage,
		RecordHelper: recordStoreHelperUsage,
	}
	ask.SubagentProgressCallback = func(send func(string, tools.SubagentEvent)) func(string, tools.SubagentEvent) {
		return newSubagentProgressDispatcher(send).Callback
	}
}
