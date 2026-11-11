package agyproxy

import "strings"

// expandAgyArtifacts visits only native tool-result output slots. In particular,
// user/model text, functionCall arguments, and schemas are never artifact sources.
func expandAgyArtifacts(request map[string]any, artifactRoot string) bool {
	if artifactRoot == "" {
		return false
	}
	contents, _ := request["contents"].([]any)
	legacyCalls := make(map[string]bool)
	changed := false
	for _, rawContent := range contents {
		content, _ := rawContent.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if call, ok := part["functionCall"].(map[string]any); ok {
				rememberLegacyBridgeCall(call, legacyCalls)
			}
			result, ok := part["functionResponse"].(map[string]any)
			if !ok || !isBridgeArtifactResult(result, legacyCalls) {
				continue
			}
			response, _ := result["response"].(map[string]any)
			output, ok := response["output"].(string)
			if !ok {
				continue
			}
			if restored, ok := expandAgyArtifactText(output, artifactRoot); ok {
				response["output"] = restored
				changed = true
			}
		}
	}
	return changed
}

// Legacy lazy results share the call_mcp_tool name with other MCP servers.
// Require a matching call ID and the actual agy dispatcher argument envelope;
// the response name alone cannot establish that this is a term-llm result.
func rememberLegacyBridgeCall(call map[string]any, calls map[string]bool) {
	id, _ := call["id"].(string)
	if id == "" {
		return
	}
	name, _ := call["name"].(string)
	if name != "call_mcp_tool" {
		return
	}
	args, _ := call["args"].(map[string]any)
	server, _ := args["ServerName"].(string)
	tool, _ := args["ToolName"].(string)
	calls[id] = server == BridgeServerName && tool != ""
}

func isBridgeArtifactResult(result map[string]any, legacyCalls map[string]bool) bool {
	name, _ := result["name"].(string)
	if strings.HasPrefix(name, bridgeToolPrefix) && len(name) > len(bridgeToolPrefix) {
		return true
	}
	if name != "call_mcp_tool" {
		return false
	}
	id, _ := result["id"].(string)
	bridge := legacyCalls[id]
	delete(legacyCalls, id)
	return bridge
}
