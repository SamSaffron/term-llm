package llm

import "strings"

func collectTextParts(parts []Part) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type == PartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func collectToolResultText(parts []Part) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type == PartToolResult && part.ToolResult != nil {
			b.WriteString(part.ToolResult.Content)
		}
	}
	return b.String()
}

func flattenSystemUser(messages []Message) (string, string) {
	var systemParts []string
	var userParts []string
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		case RoleUser:
			userParts = append(userParts, collectTextParts(msg.Parts))
		case RoleTool:
			userParts = append(userParts, collectToolResultText(msg.Parts))
		}
	}
	return strings.Join(systemParts, "\n\n"), strings.Join(userParts, "\n\n")
}
