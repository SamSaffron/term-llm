package agyproxy

import (
	"regexp"
	"strings"
)

// agy decorates agent requests with harness chatter that has no meaning to a
// term-llm conversation. The rewrites below are deterministic, so every
// request in a conversation sees the same history and upstream prompt caching
// is unaffected.
const (
	agyToolDescriptionPrefix = "This is a tool from the " + BridgeServerName + " MCP server.\n"
	agyRestartNotice         = "content=[Notice] All your subagents and background tasks have been stopped due to server restart."
	agySystemMessageIntro    = "The following is a <SYSTEM_MESSAGE> not actually sent by the user."
	agyUserRequestEnd        = "</USER_REQUEST>"
)

var (
	agyToolTimestamps  = regexp.MustCompile(`\ACreated At: [^\n]*\nCompleted At: [^\n]*\n`)
	agySettingsChange  = regexp.MustCompile(`\n?<USER_SETTINGS_CHANGE>\n[^<]*</USER_SETTINGS_CHANGE>`)
	agySystemMessageID = regexp.MustCompile(`\[Message\] `)
)

// cleanBridgeDeclaration removes agy's "This is a tool from the term-llm MCP
// server." banner. agy's required toolSummary argument must stay: agy rejects
// calls that omit it, then strips it before invoking the MCP server.
func cleanBridgeDeclaration(decl map[string]any) {
	if description, ok := decl["description"].(string); ok {
		decl["description"] = strings.TrimPrefix(description, agyToolDescriptionPrefix)
	}
}

// cleanAgyContents strips tool-result timestamps, model-selection notices, and
// the restart notice agy injects into every resumed conversation.
func cleanAgyContents(request map[string]any) {
	contents, ok := request["contents"].([]any)
	if !ok {
		return
	}
	cleaned := make([]any, 0, len(contents))
	for _, raw := range contents {
		content, ok := raw.(map[string]any)
		if !ok {
			cleaned = append(cleaned, raw)
			continue
		}
		parts, ok := content["parts"].([]any)
		if !ok {
			cleaned = append(cleaned, raw)
			continue
		}
		role, _ := content["role"].(string)
		keptParts := make([]any, 0, len(parts))
		for _, part := range parts {
			if cleanAgyPart(part, role) {
				keptParts = append(keptParts, part)
			}
		}
		if len(keptParts) == 0 && len(parts) > 0 {
			continue
		}
		content["parts"] = keptParts
		cleaned = append(cleaned, content)
	}
	request["contents"] = cleaned
}

// cleanAgyPart rewrites part in place and reports whether it should be kept.
// Only user-role text is inspected: agy injects its notices there, and model
// text must replay exactly as generated.
func cleanAgyPart(raw any, role string) bool {
	part, ok := raw.(map[string]any)
	if !ok {
		return true
	}
	if text, ok := part["text"].(string); ok && role == "user" {
		if isAgyRestartNotice(text) {
			return false
		}
		part["text"] = stripAgySettingsChange(text)
	}
	if functionResponse, ok := part["functionResponse"].(map[string]any); ok {
		if response, ok := functionResponse["response"].(map[string]any); ok {
			if output, ok := response["output"].(string); ok {
				response["output"] = agyToolTimestamps.ReplaceAllString(output, "")
			}
		}
	}
	return true
}

// stripAgySettingsChange removes settings notices from the metadata agy
// appends after the user's request, leaving the request body untouched.
func stripAgySettingsChange(text string) string {
	end := strings.LastIndex(text, agyUserRequestEnd)
	if end < 0 {
		return text
	}
	end += len(agyUserRequestEnd)
	return text[:end] + agySettingsChange.ReplaceAllString(text[end:], "")
}

// isAgyRestartNotice matches a system message carrying only agy's notice that
// background work stopped when the previous agy process exited. term-llm runs
// no agy subagents or background tasks, so the notice is always irrelevant.
func isAgyRestartNotice(text string) bool {
	return strings.HasPrefix(text, agySystemMessageIntro) &&
		strings.Contains(text, agyRestartNotice) &&
		len(agySystemMessageID.FindAllStringIndex(text, -1)) == 1
}
