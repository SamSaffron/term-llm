package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// mcpServerToolsJSON holds the JSON tool definitions passed via flag
var mcpServerToolsJSON string

var mcpServerCmd = &cobra.Command{
	Use:    "mcp-server",
	Short:  "Run as an MCP server (internal use)",
	Hidden: true, // Internal command, not for direct user use
	Long: `Run term-llm as an MCP server over stdio.

DEPRECATED: This command is deprecated. The claude-bin provider now uses
an in-process HTTP MCP server instead of spawning a subprocess. This command
remains available for external MCP clients that want to connect via stdio.

This command reads tool definitions from --tools-json and handles tool calls
via the MCP protocol. Note: Tools are not actually executed - this server
only returns tool call information for the caller to handle.`,
	RunE: runMCPServer,
}

func init() {
	mcpServerCmd.Flags().StringVar(&mcpServerToolsJSON, "tools-json", "", "JSON array of tool definitions")
	rootCmd.AddCommand(mcpServerCmd)
}

// ToolDefinition represents a tool definition passed to the MCP server
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

func runMCPServer(cmd *cobra.Command, args []string) error {
	if mcpServerToolsJSON == "" {
		return fmt.Errorf("--tools-json is required")
	}

	// Parse tool definitions
	var tools []ToolDefinition
	if err := json.Unmarshal([]byte(mcpServerToolsJSON), &tools); err != nil {
		return fmt.Errorf("parse tools-json: %w", err)
	}

	// Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "term-llm",
		Version: "1.0.0",
	}, nil)

	// Add each tool
	for _, tool := range tools {
		toolName := tool.Name // capture for closure
		server.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Schema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Write tool call to stdout as a special message that the parent process can intercept
			// For now, we just return the arguments as-is for the parent to handle
			callInfo := map[string]any{
				"tool": toolName,
				"args": req.Params.Arguments,
			}
			callJSON, _ := json.Marshal(callInfo)

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: string(callJSON)},
				},
			}, nil
		})
	}

	// Set up signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Run the server on stdio
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("MCP server error: %w", err)
	}

	return nil
}
